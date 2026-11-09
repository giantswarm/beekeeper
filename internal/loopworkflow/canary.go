package loopworkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// CanaryTimeout bounds a staged canary run so hung commands fail closed.
const CanaryTimeout = 90 * time.Second

// verifyCandidateBundle verifies a candidate binary against its Sigstore bundle;
// tests may replace it to exercise bundle verification without network calls.
var verifyCandidateBundle = update.VerifyCandidateBundle

// CanaryConfig configures a staged release-candidate canary run.
type CanaryConfig struct {
	Candidate string
	Bundle    string
	Server    string
	Scratch   string
	Recorded  bool
	Repo      string
	AllowOrg  string
	Project   int
	Run       string
	Scenario  string
	Siblings  []string
}

// StageCanary runs a signed release candidate in an isolated staging environment
// through worker start, PR park, independent exact-head review, and coordinator
// merge, returning CanaryEvidence bound to the candidate binary's digest.
// Any failure, timeout, missing permission, cleanup error, or unobserved merge
// returns a non-success evidence record and an error.
func StageCanary(parentCtx context.Context, cfg CanaryConfig) (update.CanaryEvidence, error) {
	ctx, cancel := context.WithTimeout(parentCtx, CanaryTimeout)
	defer cancel()

	runID := strings.TrimSpace(cfg.Run)
	if runID == "" {
		runID = fmt.Sprintf("canary-%d", time.Now().UTC().UnixNano())
	}
	if cfg.Recorded && !strings.HasPrefix(runID, update.RecordedRunPrefix) {
		runID = update.RecordedRunPrefix + runID
	}
	ev := update.CanaryEvidence{
		Schema: 1,
		OS:     runtime.GOOS,
		Arch:   runtime.GOARCH,
		Status: update.CanaryStatusFailed,
		Run:    runID,
	}
	fail := func(err error) (update.CanaryEvidence, error) {
		ev.Status = update.CanaryStatusFailed
		ev.MergeCommit = ""
		ev.Reason = oneLineReason(err.Error())
		return ev, err
	}

	candidatePath, err := filepath.Abs(strings.TrimSpace(cfg.Candidate))
	if err != nil || strings.TrimSpace(cfg.Candidate) == "" {
		return fail(errors.New("staged candidate binary path is required"))
	}
	if same, serr := sameExecutable(candidatePath); serr != nil {
		return fail(serr)
	} else if same {
		return fail(errors.New("canary refuses to run against the live installed executable"))
	}

	binaryBytes, err := os.ReadFile(candidatePath) //nolint:gosec // explicit staged candidate path
	if err != nil {
		return fail(fmt.Errorf("reading staged candidate: %w", err))
	}
	sum := sha256.Sum256(binaryBytes)
	ev.SHA256 = hex.EncodeToString(sum[:])

	if err := validateSandboxConfig(cfg); err != nil {
		return fail(err)
	}

	bundlePath := strings.TrimSpace(cfg.Bundle)
	if !cfg.Recorded && bundlePath == "" {
		return fail(errors.New("live sandbox canary requires --canary-bundle"))
	}
	if bundlePath != "" {
		bundleBytes, berr := os.ReadFile(bundlePath) //nolint:gosec // explicit candidate bundle path
		if berr != nil {
			return fail(fmt.Errorf("reading candidate bundle: %w", berr))
		}
		if verr := verifyCandidateBundle(filepath.Base(candidatePath), binaryBytes, bundleBytes); verr != nil {
			return fail(fmt.Errorf("verifying candidate bundle: %w", verr))
		}
	}

	idDir, idEnv, err := newIsolatedCanaryEnv(cfg.Scratch)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(idDir) }()

	ver, commit, err := candidateIdentity(ctx, candidatePath, idDir, idEnv)
	if err != nil {
		return fail(err)
	}
	ev.Version = ver
	ev.Commit = commit

	var mergeOID string
	if cfg.Recorded {
		mergeOID, err = runRecordedCanary(ctx, candidatePath, cfg)
	} else {
		rawRunID := strings.TrimPrefix(runID, update.RecordedRunPrefix)
		mergeOID, err = runLiveSandboxCanary(ctx, candidatePath, ev.SHA256, cfg, rawRunID)
	}
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(mergeOID) == "" {
		return fail(errors.New("canary did not observe a merged commit"))
	}

	ev.Status = update.CanaryStatusSuccess
	ev.MergeCommit = mergeOID
	ev.Reason = ""

	if cfg.Recorded {
		if _, verr := update.ValidateRecordedEvidenceFor(mustJSON(ev), ev.Version, ev.SHA256, ev.OS, ev.Arch); verr != nil {
			return fail(fmt.Errorf("validating recorded canary evidence: %w", verr))
		}
		return ev, nil
	}

	if _, verr := update.ValidateEvidence(mustJSON(ev), ev.Version, ev.SHA256); verr != nil {
		return fail(fmt.Errorf("validating generated canary evidence: %w", verr))
	}

	for _, sibling := range cfg.Siblings {
		sibling = strings.TrimSpace(sibling)
		if sibling == "" {
			continue
		}
		if serr := writeSiblingEvidence(sibling, ev); serr != nil {
			return fail(serr)
		}
	}
	return ev, nil
}

func validateSandboxConfig(cfg CanaryConfig) error {
	if cfg.Recorded {
		if cfg.Repo != "" || cfg.AllowOrg != "" || cfg.Project != 0 {
			return errors.New("--canary-recorded cannot be combined with live sandbox repository flags")
		}
		for _, sib := range cfg.Siblings {
			if strings.TrimSpace(sib) != "" {
				return errors.New("--canary-recorded cannot emit sibling release evidence")
			}
		}
		return nil
	}
	repo := strings.TrimSpace(cfg.Repo)
	allowOrg := strings.TrimSpace(cfg.AllowOrg)
	if repo == "" || allowOrg == "" || cfg.Project <= 0 {
		return errors.New("sandbox canary requires explicit --canary-repo, --canary-allow-org and --canary-project (or --canary-recorded)")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("invalid sandbox repository %q: want <owner>/<repo>", repo)
	}
	if !strings.EqualFold(owner, allowOrg) {
		return fmt.Errorf("sandbox repository %q is outside allowlisted organization %q", repo, allowOrg)
	}
	if strings.EqualFold(repo, update.Repository) || strings.EqualFold(repo, "giantswarm/devctl") {
		return fmt.Errorf("sandbox repository %q must not be a production repository", repo)
	}
	return nil
}

func newIsolatedCanaryEnv(scratch string, extra ...string) (string, []string, error) {
	dir, err := os.MkdirTemp(scratch, "canary-isolated-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating isolated canary env: %w", err)
	}
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	stateDir := filepath.Join(dir, "state")
	for _, sub := range []string{home, tmp, stateDir} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := fmt.Sprintf("stateDir: %s\nidentity:\n  host: canary-isolated\n", stateDir)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_STATE_HOME=" + home,
		"TMPDIR=" + tmp,
		"BEEKEEPER_CONFIG=" + cfgPath,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		envLang,
		envTZ,
		envGitGlobal,
		envGitSystem,
		envGitPrompt,
	}
	env = append(env, extra...)
	return dir, env, nil
}

func sameExecutable(candidatePath string) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	exeReal, err := filepath.EvalSymlinks(exe)
	if err != nil {
		exeReal = filepath.Clean(exe)
	}
	candReal, err := filepath.EvalSymlinks(candidatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("staged candidate %s does not exist", candidatePath)
		}
		candReal = filepath.Clean(candidatePath)
	}
	return exeReal == candReal, nil
}

func candidateIdentity(ctx context.Context, candidatePath, dir string, env []string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, candidatePath, "version") //nolint:gosec // verified staged candidate binary
	c.Dir = dir
	c.Env = env
	out, err := c.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("candidate version: %w: %s", err, tail(out))
	}
	text := strings.TrimSpace(string(out))
	var v struct {
		Version   string `json:"version"`
		GitCommit string `json:"gitCommit"`
	}
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return "", "", fmt.Errorf("parsing candidate version: %w", err)
		}
	} else {
		fields := strings.Fields(text)
		if len(fields) >= 2 && fields[0] == "beekeeper" {
			v.Version = fields[1]
		} else if len(fields) >= 1 {
			v.Version = fields[0]
		}
		if _, after, ok := strings.Cut(text, "commit "); ok {
			sha, _, _ := strings.Cut(after, ",")
			sha, _, _ = strings.Cut(sha, ")")
			v.GitCommit = strings.TrimSpace(sha)
		}
	}
	ver := strings.TrimSpace(v.Version)
	if ver == "" || ver == "dev" {
		return "", "", fmt.Errorf("staged candidate reports non-release version %q", ver)
	}
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	return ver, strings.TrimSpace(v.GitCommit), nil
}

func runRecordedCanary(ctx context.Context, candidatePath string, cfg CanaryConfig) (string, error) {
	server := strings.TrimSpace(cfg.Server)
	if server == "" {
		server = candidatePath
	}
	dir, err := os.MkdirTemp(cfg.Scratch, "canary-*")
	if err != nil {
		return "", fmt.Errorf("creating canary scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	p := probe{ctx: ctx, dir: dir}
	if err := p.prepareCanary(server, cfg.Scenario); err != nil {
		return "", err
	}
	api := httptest.NewServer(http.HandlerFunc(p.serveAPI))
	defer api.Close()
	p.env = append(p.env, "BEEKEEPER_GITHUB_API="+api.URL, "FIXTURE_CANDIDATE="+candidatePath)

	var logs strings.Builder
	runCandidate := func(dir string, args ...string) error {
		c := exec.CommandContext(ctx, candidatePath, args...) //nolint:gosec // staged candidate under canary
		c.Env, c.Dir = p.env, dir
		out, err := c.CombinedOutput()
		fmt.Fprintf(&logs, "[%s] %s\n", strings.Join(args, " "), strings.TrimSpace(string(out)))
		if err != nil {
			return fmt.Errorf("beekeeper %s: %w: %s", strings.Join(args, " "), err, tail(out))
		}
		return nil
	}

	// Pass 1: supervisor loop scans the board and starts the deterministic worker.
	if err := runCandidate(dir, "supervisor", "loop", "--once"); err != nil {
		return "", err
	}

	// Worker turn: push the change branch, open pull.json, and park on the PR.
	checkout := p.path(filepath.Join("checkouts", "project"))
	if err := p.workerPublishAndPark(checkout, runCandidate); err != nil {
		return "", err
	}
	if cfg.Scenario == "no-checkout" {
		if _, err := p.run(filepath.Join("checkouts", "project"), gitRemote, "set-url", originRemote, p.path("missing.git")); err != nil {
			return "", err
		}
	}

	// Pass 2: supervisor loop dispatches the independent exact-head reviewer,
	// which posts the approval review for the parked head.
	// Pass 3 & 4: supervisor loop validates the merge worktree and merges the PR.
	for range 3 {
		if err := runCandidate(dir, "supervisor", "loop", "--once"); err != nil {
			return "", err
		}
	}

	oid, err := p.canaryProven()
	if err != nil {
		return "", fmt.Errorf("%w (logs: %s)", err, oneLineReason(logs.String()))
	}
	return oid, nil
}

func (p *probe) prepareCanary(server, scenario string) (err error) {
	if p.git, err = exec.LookPath(gitCmd); err != nil {
		return err
	}
	checkout := filepath.Join("checkouts", "project")
	for _, d := range []string{"bin", serverDir, checkout} {
		if err := os.MkdirAll(p.path(d), 0o700); err != nil {
			return err
		}
	}
	for _, name := range []string{ghCmd, gitCmd, claudeCmd} {
		if err := os.Symlink(server, filepath.Join(p.path("bin"), name)); err != nil {
			return err
		}
	}
	if scenario == "" {
		scenario = "happy"
	}
	home := p.path("home")
	p.env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "PATH=" + p.path("bin"), envLang, envTZ,
		"BEEKEEPER_CONFIG=" + p.path("config.yaml"), "FIXTURE_DIR=" + p.dir, "FIXTURE_GIT=" + p.git, "FIXTURE_CASE=" + scenario,
		envGitGlobal, envGitSystem, "GIT_ALLOW_PROTOCOL=file", envGitPrompt,
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1",
	}
	readme := filepath.Join(serverDir, readmeFile)
	write := func(name, text string) error { return os.WriteFile(p.path(name), []byte(text), 0o600) }
	if err := p.runAll(serverDir, []string{gitInit, "--bare", p.path("origin.git")}, []string{gitInit, "-b", mainBranch}, []string{gitRemote, gitAdd, originRemote, p.path("origin.git")}); err != nil {
		return err
	}
	if err := write(readme, "fixture base\n"); err != nil {
		return err
	}
	if err := p.runAll(serverDir, []string{gitAdd, "."}, []string{gitCommit, "-m", "fixture base"}, []string{gitPush, originRemote, mainBranch}); err != nil {
		return err
	}
	if p.base, err = p.run(serverDir, gitRevParse, gitHead); err != nil {
		return err
	}
	if err := p.runAll(checkout,
		[]string{gitInit, "-b", mainBranch},
		[]string{gitRemote, gitAdd, originRemote, p.path("origin.git")},
		[]string{"fetch", originRemote, mainBranch},
		[]string{"reset", "--hard", "FETCH_HEAD"},
	); err != nil {
		return err
	}
	p.now = time.Now().UTC()
	briefPath := p.path("worker-brief.md")
	if err := write("worker-brief.md", "Work {{.URL}} in {{.Repo}} (#{{.Number}}).\n"); err != nil {
		return err
	}
	cfgText := fmt.Sprintf(
		"agents:\n  mode: background\n  dir: %s\nboard:\n  owner: fixture\n  project: 1\n  tidyEvery: -1s\n  order:\n  - name: ready\n    status: [Ready]\ncapacity:\n  ceiling: 1\n  reviewHeadroom: 1\n  availMinMiB: 1\n  swapGrowthMaxMiB: 100000\nwatch:\n  loadMax: 1000\n  availMinMiB: 1\nidentity:\n  host: fixture\nstateDir: %s\ndisk:\n  checkouts: [%s]\ngithub:\n  floor: -1\n  allowedOrgs: [fixture]\nsupervisor:\n  workerBrief: %s\n  ownership:\n    authors: [fixture-bot]\n  reviewRounds: 1\n",
		p.path(checkout), p.path("state"), p.path(checkout), briefPath,
	)
	if err := write("config.yaml", cfgText); err != nil {
		return err
	}
	raw, err := pullPayload(p.base, []any{})
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.path(pullJSONFile), raw, 0o600); err != nil {
		return err
	}
	store, err := state.Open(p.path("state"))
	if err != nil {
		return err
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.AutoInstallPaused = true
		st.Budget = &state.Budget{
			Remaining: 5000,
			Limit:     5000,
			Reset:     p.now.Add(time.Hour),
			At:        p.now,
			GraphQL:   &state.GraphQL{Remaining: 5000, Limit: 5000, Reset: p.now.Add(time.Hour), At: p.now},
		}
		return nil, nil
	}); err != nil {
		return err
	}
	doneAt := p.now.Add(-30 * time.Second).UTC()
	byLoop := state.Party{Name: partyLoop}
	byPrior := state.Party{Name: "prior-worker"}
	for _, e := range []state.Event{
		{At: p.now.Add(-time.Minute), By: byLoop, Verb: verbLoop, Detail: detailStarted},
		{At: p.now.Add(-time.Minute), By: byLoop, Verb: verbBudgetState, Detail: detailFalse},
		{At: doneAt, By: byPrior, Verb: "agents.done", Detail: "prior-worker done: setup"},
		{At: doneAt, By: byPrior, Verb: "agents.report", Detail: "prior-worker: https://github.com/fixture/project/issues/1"},
		{At: doneAt, By: byLoop, Verb: "supervisor.done", Detail: "[done prior-worker@" + doneAt.Format(time.RFC3339Nano) + "] off the roster"},
	} {
		if err := store.Record(e); err != nil {
			return err
		}
	}
	return nil
}

func (p *probe) workerPublishAndPark(checkout string, runCandidate func(dir string, args ...string) error) error {
	relCheckout := filepath.Join("checkouts", "project")
	if err := p.runAll(relCheckout, []string{gitCheckout, "-b", changeBranch}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(checkout, readmeFile), []byte(fixtureChangeMsg+"\n"), 0o600); err != nil {
		return err
	}
	if err := p.runAll(relCheckout, []string{gitAdd, readmeFile}, []string{gitCommit, "-m", fixtureChangeMsg}); err != nil {
		return err
	}
	head, err := p.run(relCheckout, gitRevParse, gitHead)
	if err != nil {
		return err
	}
	if err := p.runAll(relCheckout, []string{gitPush, originRemote, changeBranch}, []string{gitPush, originRemote, head + ":refs/pull/1/head"}); err != nil {
		return err
	}
	raw, err := pullPayload(head, []any{})
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.path(pullJSONFile), raw, 0o600); err != nil {
		return err
	}
	if err := runCandidate(checkout, "--as", "project 2", "agents", "park", "--on", "fixture/project#1", "desk review"); err != nil {
		return err
	}
	return p.runAll(relCheckout, []string{gitCheckout, mainBranch})
}

func (p *probe) canaryProven() (string, error) {
	traceBytes, _ := os.ReadFile(p.path("trace.jsonl"))
	if err := p.proven(); err != nil {
		return "", fmt.Errorf("%w (trace: %s)", err, tail(traceBytes))
	}
	traceText := string(traceBytes)
	for _, required := range []string{
		`"project 2"`,
		`"Review project 1 r1"`,
		`"gh","pr","review"`,
		`"gh","pr","merge"`,
	} {
		if !strings.Contains(traceText, required) {
			return "", fmt.Errorf("canary trace missing required workflow step %s", required)
		}
	}
	raw, err := os.ReadFile(p.path(pullJSONFile))
	if err != nil {
		return "", err
	}
	var result struct {
		State       string `json:"state"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.State != mergedState || result.MergeCommit.OID == "" {
		return "", errors.New("pull request is not merged in origin")
	}
	return result.MergeCommit.OID, nil
}

func canaryBranchPrefix(runID string) string {
	return "canary/" + strings.TrimSpace(runID) + "/"
}

func canaryBranchName(runID, candidateDigest string) string {
	short := strings.TrimSpace(candidateDigest)
	if len(short) > 12 {
		short = short[:12]
	}
	if short == "" {
		short = "candidate"
	}
	return canaryBranchPrefix(runID) + "cand-" + short
}

func canaryOwnershipMarker(repo, runID, branch string) string {
	return fmt.Sprintf("beekeeper-canary-owner: repo=%s run=%s branch=%s", strings.TrimSpace(repo), strings.TrimSpace(runID), strings.TrimSpace(branch))
}

func containsExactLine(text, want string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func isOwnedCanaryPR(pr sandboxOpenPR, repo, runID string) bool {
	if pr.Head.Repo == nil || pr.Head.Repo.Fork || !strings.EqualFold(strings.TrimSpace(pr.Head.Repo.FullName), strings.TrimSpace(repo)) {
		return false
	}
	headRefName := strings.TrimSpace(pr.headRef())
	if !isOwnedCanaryBranch(runID, headRefName) {
		return false
	}
	return containsExactLine(pr.Body, canaryOwnershipMarker(repo, runID, headRefName))
}

func isOwnedCanaryBranch(runID, branch string) bool {
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	prefix := canaryBranchPrefix(runID)
	if !strings.HasPrefix(branch, prefix) {
		return false
	}
	rest := strings.TrimPrefix(branch, prefix)
	hexSuffix, ok := strings.CutPrefix(rest, "cand-")
	if !ok || len(hexSuffix) < 12 || strings.Contains(hexSuffix, "/") {
		return false
	}
	for i := 0; i < len(hexSuffix); i++ {
		c := hexSuffix[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type canaryPRMeta struct {
	PRNum         int    `json:"prNum"`
	HeadSHA       string `json:"headSHA"`
	Branch        string `json:"branch"`
	WorkerName    string `json:"workerName"`
	WorkerSession string `json:"workerSession"`
}

type canaryReviewerMeta struct {
	ReviewerName    string `json:"reviewerName"`
	ReviewerSession string `json:"reviewerSession"`
	PRNum           int    `json:"prNum"`
	HeadSHA         string `json:"headSHA"`
}

func writeIsolatedGitAuth(dir, binDir string, env []string) ([]string, error) {
	credHelper := filepath.Join(binDir, "git-credential-canary")
	credScript := "#!/bin/sh\nif [ \"${1:-}\" = \"get\" ]; then\n  echo \"username=x-access-token\"\n  printf 'password=%s\\n' \"${GH_TOKEN:-}\"\nfi\n" //nolint:gosec // G101: shell script template reading runtime GH_TOKEN env var
	if err := os.WriteFile(credHelper, []byte(credScript), 0o700); err != nil {                                                                         //nolint:gosec // isolated sandbox credential helper
		return nil, err
	}
	askPass := filepath.Join(binDir, "git-askpass")
	askScript := "#!/bin/sh\ncase \"${1:-}\" in\n  *Username*|*username*) echo \"x-access-token\" ;;\n  *) printf '%s\\n' \"${GH_TOKEN:-}\" ;;\nesac\n"
	if err := os.WriteFile(askPass, []byte(askScript), 0o700); err != nil { //nolint:gosec // isolated sandbox askpass helper
		return nil, err
	}
	gitConfigPath := filepath.Join(dir, "home", ".gitconfig")
	var gitCfg strings.Builder
	fmt.Fprintf(&gitCfg, "[credential]\n\thelper = !%s\n\tuseHttpPath = true\n", credHelper)
	if caInfo := strings.TrimSpace(os.Getenv("BEEKEEPER_CANARY_SSL_CAINFO")); caInfo != "" {
		fmt.Fprintf(&gitCfg, "[http]\n\tsslCAInfo = %s\n", caInfo)
	}
	if err := os.WriteFile(gitConfigPath, []byte(gitCfg.String()), 0o600); err != nil {
		return nil, err
	}
	env = slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL=") || strings.HasPrefix(kv, "GIT_ASKPASS=")
	})
	env = append(env,
		"GIT_CONFIG_GLOBAL="+gitConfigPath,
		"GIT_ASKPASS="+askPass,
	)
	if caInfo := strings.TrimSpace(os.Getenv("BEEKEEPER_CANARY_SSL_CAINFO")); caInfo != "" {
		env = append(env, "GIT_SSL_CAINFO="+caInfo)
	}
	return env, nil
}

func runLiveSandboxCanary(ctx context.Context, candidatePath, candidateDigest string, cfg CanaryConfig, runID string) (mergeOID string, retErr error) {
	ghBin, err := exec.LookPath(ghCmd)
	if err != nil {
		return "", fmt.Errorf("locating gh CLI for sandbox canary: %w", err)
	}
	gitBin, err := exec.LookPath(gitCmd)
	if err != nil {
		return "", fmt.Errorf("locating git CLI for sandbox canary: %w", err)
	}
	serverBin := strings.TrimSpace(cfg.Server)
	if serverBin == "" {
		serverBin, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("locating runner executable for sandbox claude adapter: %w", err)
		}
	}

	dir, env, err := newIsolatedCanaryEnv(cfg.Scratch)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	for name, target := range map[string]string{"real-gh": ghBin, gitCmd: serverBin, claudeCmd: serverBin} {
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			return "", err
		}
	}
	env, err = writeIsolatedGitAuth(dir, binDir, env)
	if err != nil {
		return "", err
	}

	// Budget probes intentionally strip ambient GH_TOKEN and ask the stored
	// login for a credential. Supply only this canary's explicit credential
	// through its isolated CLI; never fall back to the host's stored login.
	ghWrapper := `#!/bin/sh
set -eu
if [ "$#" -eq 4 ] && [ "$1" = auth ] && [ "$2" = token ] && [ "$3" = --hostname ] && [ "$4" = github.com ]; then
  printf '%s\n' "${BEEKEEPER_CANARY_GITHUB_TOKEN:?}"
  exit 0
fi
exec "$(dirname "$0")/real-gh" "$@"
`
	if err := os.WriteFile(filepath.Join(binDir, ghCmd), []byte(ghWrapper), 0o700); err != nil {
		return "", err
	}

	sandboxToken := strings.TrimSpace(os.Getenv("BEEKEEPER_CANARY_GITHUB_TOKEN"))
	if sandboxToken == "" {
		return "", errors.New("live sandbox canary requires explicit BEEKEEPER_CANARY_GITHUB_TOKEN")
	}

	viewerLogin := "canary-bot"
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4900")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(time.Hour).Unix()))
		switch r.URL.Path {
		case "/repos/giantswarm/beekeeper":
			_, _ = fmt.Fprint(w, `{}`)
		case "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":%q},"rateLimit":{"limit":5000,"remaining":4900,"used":100,"resetAt":%q}}}`, viewerLogin, now.Add(time.Hour).Format(time.RFC3339))
		case "/rate_limit":
			_, _ = fmt.Fprint(w, `{"resources":{"search":{"limit":30,"remaining":30},"code_search":{"limit":10,"remaining":10}}}`)
		default:
			http.Error(w, "unknown sandbox API call", http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	apiURL := strings.TrimSpace(os.Getenv("BEEKEEPER_GITHUB_API"))
	if apiURL == "" {
		apiURL = apiServer.URL
	}

	checkout := filepath.Join(dir, "checkouts", filepath.Base(cfg.Repo))
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return "", err
	}
	branch := canaryBranchName(runID, candidateDigest)
	marker := canaryOwnershipMarker(cfg.Repo, runID, branch)
	digestLine := "candidate-sha256: " + candidateDigest
	projectLine := fmt.Sprintf("sandbox-project: %d", cfg.Project)

	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	env = append(env,
		"PATH="+binDir+":/usr/bin:/bin",
		"BEEKEEPER_CONFIG="+filepath.Join(dir, "config.yaml"),
		"GH_TOKEN="+sandboxToken,
		"BEEKEEPER_CANARY_GITHUB_TOKEN="+sandboxToken,
		"BEEKEEPER_GITHUB_API="+apiURL,
		"GIT_ALLOW_PROTOCOL=file:https",
		"GIT_AUTHOR_NAME=Beekeeper Canary",
		"GIT_AUTHOR_EMAIL=canary@example.invalid",
		"GIT_COMMITTER_NAME=Beekeeper Canary",
		"GIT_COMMITTER_EMAIL=canary@example.invalid",
		"CANARY_SANDBOX_DIR="+dir,
		"CANARY_CHECKOUT_DIR="+checkout,
		"CANARY_CANDIDATE_BIN="+candidatePath,
		"CANARY_CANDIDATE_SHA256="+candidateDigest,
		"CANARY_REAL_GIT="+gitBin,
		"CANARY_REPO="+cfg.Repo,
		"CANARY_PROJECT="+strconv.Itoa(cfg.Project),
		"CANARY_RUN_ID="+runID,
		"CANARY_BRANCH="+branch,
	)
	for _, passthrough := range []string{"BEEKEEPER_CANARY_ORIGIN", "SANDBOX_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if val := os.Getenv(passthrough); val != "" {
			env = append(env, passthrough+"="+val)
		}
	}

	// Entry cleanup: must succeed before any sandbox workflow step runs.
	if err := cleanupSandboxRunWithEnv(ctx, ghBin, env, cfg.Repo, cfg.AllowOrg, runID); err != nil {
		return "", fmt.Errorf("entry sandbox cleanup: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cerr := cleanupSandboxRunWithEnv(cleanupCtx, ghBin, env, cfg.Repo, cfg.AllowOrg, runID); cerr != nil {
			mergeOID = ""
			retErr = errors.Join(retErr, fmt.Errorf("exit sandbox cleanup: %w", cerr))
		}
	}()

	runCmdWithEnv := func(workDir string, cmdEnv []string, bin string, args ...string) (string, error) {
		c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // isolated sandbox command
		c.Dir = workDir
		c.Env = cmdEnv
		out, cerr := c.CombinedOutput()
		if cerr != nil {
			return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), cerr, tail(out))
		}
		return strings.TrimSpace(string(out)), nil
	}
	runCmd := func(workDir, bin string, args ...string) (string, error) {
		return runCmdWithEnv(workDir, env, bin, args...)
	}

	if loginOut, lerr := runCmd(dir, ghBin, ghAPI, "user", "--jq", ".login"); lerr == nil && loginOut != "" {
		viewerLogin = loginOut
	}

	stateDir := filepath.Join(dir, "state")
	briefPath := filepath.Join(dir, "worker-brief.md")
	if err := os.WriteFile(briefPath, []byte("Work {{.URL}} in {{.Repo}} (#{{.Number}}).\n"), 0o600); err != nil {
		return "", err
	}
	cfgText := fmt.Sprintf(
		"agents:\n  mode: background\n  dir: %s\nboard:\n  owner: %s\n  project: %d\n  tidyEvery: -1s\n  order:\n  - name: ready\ncapacity:\n  ceiling: 1\n  reviewHeadroom: 1\n  availMinMiB: 1\n  swapGrowthMaxMiB: 100000\nwatch:\n  loadMax: 1000\n  availMinMiB: 1\nidentity:\n  host: canary\nstateDir: %s\ndisk:\n  checkouts: [%s]\ngithub:\n  floor: -1\n  allowedOrgs: [%s]\nsupervisor:\n  workerBrief: %s\n  ownership:\n    authors: [%s]\n  reviewRounds: 1\n",
		checkout, cfg.AllowOrg, cfg.Project, stateDir, checkout, cfg.AllowOrg, briefPath, viewerLogin,
	)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgText), 0o600); err != nil {
		return "", err
	}

	now := time.Now().UTC()
	store, err := state.Open(stateDir)
	if err != nil {
		return "", err
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.AutoInstallPaused = true
		gql := &state.GraphQL{Identity: "gh", Remaining: 5000, Limit: 5000, Reset: now.Add(time.Hour), At: now}
		entry := state.Budget{
			Identity:  "gh",
			Remaining: 5000,
			Limit:     5000,
			Reset:     now.Add(time.Hour),
			At:        now,
			GraphQL:   gql,
		}
		st.Budget = &state.Budget{
			Identity:   "gh",
			Remaining:  5000,
			Limit:      5000,
			Reset:      now.Add(time.Hour),
			At:         now,
			GraphQL:    gql,
			Identities: map[string]state.Budget{"gh": entry},
		}
		return nil, nil
	}); err != nil {
		return "", err
	}
	doneAt := now.Add(-30 * time.Second).UTC()
	byLoop := state.Party{Name: partyLoop}
	byPrior := state.Party{Name: "prior-worker"}
	for _, ev := range []state.Event{
		{At: now.Add(-time.Minute), By: byLoop, Verb: verbLoop, Detail: detailStarted},
		{At: now.Add(-time.Minute), By: byLoop, Verb: verbBudgetState, Detail: detailFalse},
		{At: doneAt, By: byPrior, Verb: "agents.done", Detail: "prior-worker done: setup"},
		{At: doneAt, By: byPrior, Verb: "agents.report", Detail: fmt.Sprintf("prior-worker: https://github.com/%s/issues/1", cfg.Repo)},
		{At: doneAt, By: byLoop, Verb: "supervisor.done", Detail: "[done prior-worker@" + doneAt.Format(time.RFC3339Nano) + "] off the roster"},
	} {
		if err := store.Record(ev); err != nil {
			return "", err
		}
	}

	// Verify the candidate passes isolated command checks without touching live config/state.
	for _, args := range [][]string{{"version"}, {"supervisor", statusField}} {
		if _, err := runCmd(dir, candidatePath, args...); err != nil {
			return "", fmt.Errorf("candidate %s: %w", strings.Join(args, " "), err)
		}
	}

	originURL := strings.TrimSpace(os.Getenv("BEEKEEPER_CANARY_ORIGIN"))
	if originURL == "" {
		originURL = "https://github.com/" + cfg.Repo + ".git"
	}
	for _, gargs := range [][]string{
		{gitInit, "-b", mainBranch},
		{gitRemote, gitAdd, originRemote, originURL},
		{"fetch", originRemote, mainBranch},
		{"reset", "--hard", "FETCH_HEAD"},
	} {
		if _, err := runCmd(checkout, gitBin, gargs...); err != nil {
			return "", err
		}
	}
	baseSHA, err := runCmd(checkout, gitBin, gitRevParse, gitHead)
	if err != nil {
		return "", err
	}

	// Pass 1: candidate's supervisor loop must admit the ready board item and
	// start the deterministic worker via `agents start` -> `claude --bg`.
	pass1Out, err := runCmd(dir, candidatePath, "supervisor", "loop", "--once")
	if err != nil {
		if turnErr, rerr := os.ReadFile(filepath.Join(dir, "worker-turn-error.txt")); rerr == nil && len(turnErr) > 0 { //nolint:gosec // isolated sandbox error log
			return "", fmt.Errorf("starting deterministic worker: %s (%w)", strings.TrimSpace(string(turnErr)), err)
		}
		return "", fmt.Errorf("running candidate supervisor loop pass 1 (worker start): %w", err)
	}
	prMetaRaw, err := os.ReadFile(filepath.Join(dir, "canary-pr.json")) //nolint:gosec // isolated sandbox worker metadata
	if err != nil {
		if turnErr, rerr := os.ReadFile(filepath.Join(dir, "worker-turn-error.txt")); rerr == nil && len(turnErr) > 0 { //nolint:gosec // isolated sandbox error log
			return "", fmt.Errorf("deterministic worker turn failed: %s", strings.TrimSpace(string(turnErr)))
		}
		return "", fmt.Errorf("candidate supervisor loop did not start deterministic worker (loop output: %s)", pass1Out)
	}
	var prMeta canaryPRMeta
	if err := json.Unmarshal(prMetaRaw, &prMeta); err != nil {
		return "", fmt.Errorf("decoding deterministic worker metadata: %w", err)
	}
	if prMeta.PRNum <= 0 || prMeta.HeadSHA == "" || prMeta.Branch != branch || prMeta.WorkerName == "" || prMeta.WorkerSession == "" {
		return "", fmt.Errorf("incomplete deterministic worker metadata: %+v", prMeta)
	}
	prRef := fmt.Sprintf("%s#%d", cfg.Repo, prMeta.PRNum)

	// Verify the candidate recorded the worker start before the worker parks.
	stAfterWorker, err := store.Read()
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(stAfterWorker.Starts, func(s state.Start) bool {
		return s.Background && s.Session == prMeta.WorkerSession && s.Name == prMeta.WorkerName
	}) {
		return "", fmt.Errorf("candidate did not record background worker start for %s (%s)", prMeta.WorkerName, prMeta.WorkerSession)
	}

	// The started worker session parks itself on the newly created sandbox PR.
	workerEnv := append(slices.Clone(env),
		"CLAUDE_CODE_SESSION_ID="+prMeta.WorkerSession,
		"CLAUDE_CODE_SESSION_NAME="+prMeta.WorkerName,
	)
	if _, err := runCmdWithEnv(checkout, workerEnv, candidatePath, "--as", prMeta.WorkerName, "agents", "park", "--on", prRef, "desk review"); err != nil {
		return "", fmt.Errorf("parking deterministic worker %s on %s: %w", prMeta.WorkerName, prRef, err)
	}
	if _, err := runCmd(checkout, gitBin, gitCheckout, mainBranch); err != nil {
		return "", err
	}

	// Pass 2: candidate's supervisor loop must detect the parked PR, dispatch a
	// separate independent reviewer via `agents start --review` -> `claude --bg`,
	// and record `supervisor.review`.
	if _, err := runCmd(dir, candidatePath, "supervisor", "loop", "--once"); err != nil {
		return "", fmt.Errorf("running candidate supervisor loop pass 2 (reviewer dispatch): %w", err)
	}
	revMetaRaw, err := os.ReadFile(filepath.Join(dir, "reviewer-ran.json")) //nolint:gosec // isolated sandbox reviewer metadata
	if err != nil {
		return "", errors.New("candidate supervisor loop did not dispatch independent reviewer")
	}
	var revMeta canaryReviewerMeta
	if err := json.Unmarshal(revMetaRaw, &revMeta); err != nil {
		return "", fmt.Errorf("decoding independent reviewer metadata: %w", err)
	}
	if revMeta.PRNum != prMeta.PRNum || revMeta.HeadSHA != prMeta.HeadSHA || !strings.HasPrefix(revMeta.ReviewerName, "Review ") || revMeta.ReviewerSession == "" || revMeta.ReviewerSession == prMeta.WorkerSession {
		return "", fmt.Errorf("invalid independent reviewer metadata: %+v (worker=%+v)", revMeta, prMeta)
	}

	// Passes 3 & 4: candidate's supervisor loop validates the merge worktree,
	// checks statusCheckRollup and the exact-head review, and merges the PR.
	var mergeOut []string
	for range 3 {
		out, err := runCmd(dir, candidatePath, "supervisor", "loop", "--once")
		if err != nil {
			return "", fmt.Errorf("running candidate supervisor loop (merge): %w", err)
		}
		if out != "" {
			mergeOut = append(mergeOut, out)
		}
	}

	// Verify durable candidate state records for worker start, park, independent
	// reviewer start, supervisor.review, and merge before emitting evidence.
	finalState, err := store.Read()
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(finalState.Starts, func(s state.Start) bool {
		return s.Background && s.Session == revMeta.ReviewerSession && s.Name == revMeta.ReviewerName
	}) {
		return "", fmt.Errorf("candidate did not record background reviewer start for %s (%s)", revMeta.ReviewerName, revMeta.ReviewerSession)
	}
	events, err := store.Events(0, nil)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(events, func(e state.Event) bool {
		return e.Verb == "agents.start" && strings.Contains(e.Detail, prMeta.WorkerName)
	}) {
		return "", fmt.Errorf("candidate state lacks agents.start event for worker %s", prMeta.WorkerName)
	}
	if !slices.ContainsFunc(events, func(e state.Event) bool {
		return e.Verb == "agents.park" && (e.Subject == prRef || strings.Contains(e.Detail, prRef))
	}) {
		return "", fmt.Errorf("candidate state lacks agents.park event for %s", prRef)
	}
	if !slices.ContainsFunc(events, func(e state.Event) bool {
		return e.Verb == "supervisor.review" && (e.Subject == prRef || strings.Contains(e.Detail, prRef)) && strings.Contains(e.Detail, prMeta.HeadSHA)
	}) {
		return "", fmt.Errorf("candidate state lacks exact-head supervisor.review event for %s@%s", prRef, prMeta.HeadSHA)
	}
	if !slices.ContainsFunc(events, func(e state.Event) bool {
		return (e.Verb == "supervisor.merge" || e.Verb == "merged") && (e.Subject == prRef || strings.Contains(e.Detail, prRef) || strings.Contains(e.Detail, prMeta.HeadSHA))
	}) {
		return "", fmt.Errorf("candidate state lacks merge event for %s (loop outputs: %s)", prRef, strings.Join(mergeOut, " | "))
	}

	// Verify the fresh PR created by this run was reviewed on headSHA, passed
	// checks, and merged into origin.
	viewOut, err := runCmd(dir, ghBin, "pr", "view", strconv.Itoa(prMeta.PRNum), "--repo", cfg.Repo, "--json", "number,state,headRefName,headRefOid,body,reviews,statusCheckRollup,mergeCommit")
	if err != nil {
		return "", fmt.Errorf("viewing sandbox PR %s: %w", prRef, err)
	}
	var observed struct {
		Number      int    `json:"number"`
		State       string `json:"state"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		Body        string `json:"body"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		Reviews []struct {
			Body string `json:"body"`
		} `json:"reviews"`
		Rollup []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(viewOut), &observed); err != nil {
		return "", fmt.Errorf("decoding sandbox PR %s: %w", prRef, err)
	}
	if observed.Number != prMeta.PRNum || observed.HeadRefName != branch || observed.HeadRefOid != prMeta.HeadSHA {
		return "", fmt.Errorf("sandbox PR %s head mismatch: got #%d ref=%s oid=%s, want #%d ref=%s oid=%s", prRef, observed.Number, observed.HeadRefName, observed.HeadRefOid, prMeta.PRNum, branch, prMeta.HeadSHA)
	}
	if !isOwnedCanaryBranch(runID, observed.HeadRefName) || !containsExactLine(observed.Body, digestLine) || !containsExactLine(observed.Body, projectLine) || !containsExactLine(observed.Body, marker) {
		return "", fmt.Errorf("sandbox PR %s ownership or candidate digest marker mismatch", prRef)
	}
	hasExactReview := slices.ContainsFunc(observed.Reviews, func(r struct {
		Body string `json:"body"`
	}) bool {
		return (strings.Contains(r.Body, "Review: approved") || strings.Contains(r.Body, "Desk review (opus): approved")) && strings.Contains(r.Body, "Head reviewed: `"+prMeta.HeadSHA+"`")
	})
	if !hasExactReview {
		return "", fmt.Errorf("sandbox PR %s lacks exact-head approval for %s", prRef, prMeta.HeadSHA)
	}
	if len(observed.Rollup) == 0 {
		return "", fmt.Errorf("sandbox PR %s has empty statusCheckRollup", prRef)
	}
	for _, chk := range observed.Rollup {
		green := chk.State == successStatus || (chk.Status == completedStatus && slices.Contains([]string{successStatus, "NEUTRAL", "SKIPPED"}, chk.Conclusion))
		if !green {
			return "", fmt.Errorf("sandbox PR %s check not green: %+v", prRef, chk)
		}
	}
	if observed.State != mergedState || observed.MergeCommit.OID == "" || observed.MergeCommit.OID == baseSHA {
		return "", fmt.Errorf("sandbox PR %s was not merged (state=%s mergeCommit=%s)", prRef, observed.State, observed.MergeCommit.OID)
	}
	return observed.MergeCommit.OID, nil
}

func serveCanaryGit() int {
	args := os.Args[1:]
	if slices.Contains(args, "get-url") && slices.Contains(args, originRemote) {
		fmt.Printf("https://github.com/%s.git\n", os.Getenv("CANARY_REPO"))
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Getenv("CANARY_REAL_GIT"), args...) //nolint:gosec // isolated sandbox git wrapper
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

func serveCanaryClaude() int {
	args := os.Args[1:]
	sandboxDir := os.Getenv("CANARY_SANDBOX_DIR")
	sessionsPath := filepath.Join(sandboxDir, "sessions.json")
	readSessions := func() []map[string]any {
		raw, err := os.ReadFile(sessionsPath) //nolint:gosec // isolated sandbox session state
		if err != nil {
			return []map[string]any{}
		}
		var rows []map[string]any
		_ = json.Unmarshal(raw, &rows)
		return rows
	}
	writeSessions := func(rows []map[string]any) error {
		raw, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		return os.WriteFile(sessionsPath, raw, 0o600) //nolint:gosec // isolated sandbox session state
	}

	switch {
	case len(args) > 0 && args[0] == "plugin":
		fmt.Println("[]")
		return 0
	case slices.Equal(args, []string{"-p", "/usage", "--output-format", "json"}):
		reset := time.Now().UTC().Add(time.Hour).Format("Jan 2, 3:04pm")
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"result": fmt.Sprintf("Current session: 0%% used · resets %s (UTC)\nCurrent week (all models): 0%% used · resets %s (UTC)", reset, reset),
		})
		return 0
	case len(args) > 0 && args[0] == "agents":
		if err := json.NewEncoder(os.Stdout).Encode(readSessions()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case len(args) > 0 && args[0] == "stop":
		if len(args) > 1 {
			short := args[1]
			rows := slices.DeleteFunc(readSessions(), func(r map[string]any) bool {
				id, _ := r["id"].(string)
				sid, _ := r["sessionId"].(string)
				return id == short || strings.HasPrefix(sid, short)
			})
			if err := writeSessions(rows); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		return 0
	case slices.Contains(args, "--bg"):
		name := "worker"
		if idx := slices.Index(args, "-n"); idx >= 0 && idx+1 < len(args) {
			name = args[idx+1]
		}
		rows := readSessions()
		short := fmt.Sprintf("a1b2c3%02x", len(rows)+1)
		sessionID := short + "-0000-0000-0000-000000000001"
		rows = append(rows, map[string]any{
			"id":           short,
			sessionIDField: sessionID,
			nameField:      name,
			"kind":         "bg",
			"pid":          1,
		})
		if err := writeSessions(rows); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		if strings.HasPrefix(name, "Review ") {
			if err := runCanaryReviewerTurn(sandboxDir, name, sessionID); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		} else {
			if err := runCanaryWorkerPublish(sandboxDir, name, sessionID); err != nil {
				_ = os.WriteFile(filepath.Join(sandboxDir, "worker-turn-error.txt"), []byte(err.Error()+"\n"), 0o600) //nolint:gosec // isolated sandbox error log
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		fmt.Printf("backgrounded · %s · %s\n", short, name)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown canary claude command: %v\n", args)
		return 1
	}
}

func runCanaryWorkerPublish(sandboxDir, workerName, workerSession string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	checkout := os.Getenv("CANARY_CHECKOUT_DIR")
	repo := os.Getenv("CANARY_REPO")
	runID := os.Getenv("CANARY_RUN_ID")
	branch := os.Getenv("CANARY_BRANCH")
	digest := os.Getenv("CANARY_CANDIDATE_SHA256")
	project := os.Getenv("CANARY_PROJECT")
	binDir := filepath.Join(sandboxDir, "bin")
	gitBin := filepath.Join(binDir, gitCmd)
	ghBin := filepath.Join(binDir, ghCmd)

	run := func(bin string, a ...string) (string, error) {
		c := exec.CommandContext(ctx, bin, a...) //nolint:gosec // isolated sandbox worker command
		c.Dir = checkout
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(a, " "), err, tail(out))
		}
		return strings.TrimSpace(string(out)), nil
	}

	marker := canaryOwnershipMarker(repo, runID, branch)
	body := strings.Join([]string{
		"Automated staged release-candidate canary run.",
		marker,
		"candidate-sha256: " + digest,
		"sandbox-project: " + project,
	}, "\n")

	if _, err := run(gitBin, gitCheckout, "-b", branch); err != nil {
		return err
	}
	canaryFile := filepath.Join(filepath.Clean(checkout), ".beekeeper-canary")
	if err := os.WriteFile(canaryFile, []byte(body+"\n"), 0o600); err != nil { //nolint:gosec // G703: isolated sandbox checkout path
		return err
	}
	if _, err := run(gitBin, gitAdd, ".beekeeper-canary"); err != nil {
		return err
	}
	if _, err := run(gitBin, gitCommit, "-m", "chore(canary): stage "+runID); err != nil {
		return err
	}
	headSHA, err := run(gitBin, gitRevParse, gitHead)
	if err != nil {
		return err
	}
	if _, err := run(gitBin, gitPush, originRemote, branch); err != nil {
		return err
	}
	createOut, err := run(ghBin, "pr", "create", "--repo", repo, "--base", mainBranch, "--head", branch, "--title", "chore(canary): stage "+runID, "--body", body)
	if err != nil {
		return fmt.Errorf("creating sandbox canary PR: %w", err)
	}
	prNum, err := parseCreatedPRNumber(createOut)
	if err != nil {
		return fmt.Errorf("parsing created sandbox PR number from %q: %w", createOut, err)
	}
	meta := canaryPRMeta{
		PRNum:         prNum,
		HeadSHA:       headSHA,
		Branch:        branch,
		WorkerName:    workerName,
		WorkerSession: workerSession,
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sandboxDir, "canary-pr.json"), raw, 0o600) //nolint:gosec // isolated sandbox worker metadata
}

func runCanaryReviewerTurn(sandboxDir, reviewerName, reviewerSession string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	raw, err := os.ReadFile(filepath.Join(sandboxDir, "canary-pr.json")) //nolint:gosec // isolated sandbox worker metadata
	if err != nil {
		return fmt.Errorf("reading canary PR metadata for reviewer: %w", err)
	}
	var prMeta canaryPRMeta
	if err := json.Unmarshal(raw, &prMeta); err != nil {
		return err
	}
	repo := os.Getenv("CANARY_REPO")
	ghBin := filepath.Join(sandboxDir, "bin", ghCmd)
	body := "Desk review (opus): approved\nHead reviewed: `" + prMeta.HeadSHA + "`"
	c := exec.CommandContext(ctx, ghBin, "pr", "review", strconv.Itoa(prMeta.PRNum), "--repo", repo, "--comment", "--body", body) //nolint:gosec // isolated sandbox reviewer command
	c.Dir = sandboxDir
	c.Env = os.Environ()
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("reviewer gh pr review: %w: %s", err, tail(out))
	}
	revMeta := canaryReviewerMeta{
		ReviewerName:    reviewerName,
		ReviewerSession: reviewerSession,
		PRNum:           prMeta.PRNum,
		HeadSHA:         prMeta.HeadSHA,
	}
	revRaw, err := json.Marshal(revMeta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sandboxDir, "reviewer-ran.json"), revRaw, 0o600) //nolint:gosec // isolated sandbox reviewer metadata
}

func parseCreatedPRNumber(output string) (int, error) {
	for _, token := range strings.Fields(output) {
		if _, after, ok := strings.Cut(token, "/pull/"); ok {
			numStr, _, _ := strings.Cut(after, "/")
			numStr = strings.TrimSpace(numStr)
			if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
				return n, nil
			}
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(output)); err == nil && n > 0 {
		return n, nil
	}
	return 0, fmt.Errorf("no pull request number in %q", output)
}

// CleanupSandboxRun idempotently closes open pull requests and deletes branches
// owned by runID in the allowlisted sandbox repository using paginated queries,
// leaving prefix-colliding runs and copied markers untouched.
func CleanupSandboxRun(ctx context.Context, ghBin, repo, allowOrg, runID string) error {
	return cleanupSandboxRunWithEnv(ctx, ghBin, nil, repo, allowOrg, runID)
}

func cleanupSandboxRunWithEnv(ctx context.Context, ghBin string, env []string, repo, allowOrg, runID string) error {
	if err := validateSandboxConfig(CanaryConfig{Repo: repo, AllowOrg: allowOrg, Project: 1}); err != nil {
		return err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("sandbox cleanup requires a non-empty run ID")
	}
	if len(env) == 0 {
		token := strings.TrimSpace(os.Getenv("BEEKEEPER_CANARY_GITHUB_TOKEN"))
		if token == "" {
			return errors.New("sandbox cleanup requires explicit BEEKEEPER_CANARY_GITHUB_TOKEN")
		}
		env = []string{
			"PATH=" + filepath.Dir(ghBin) + ":/usr/local/bin:/usr/bin:/bin",
			"GH_TOKEN=" + token,
			"GITHUB_TOKEN=" + token,
		}
	}

	runGH := func(args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, ghBin, args...) //nolint:gosec // allowlisted sandbox cleanup
		c.Env = env
		return c.CombinedOutput()
	}

	listOpenPRs := func() ([]sandboxOpenPR, error) {
		out, err := runGH(ghAPI, "--paginate", fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", repo))
		if err != nil {
			return nil, fmt.Errorf("listing open sandbox PRs: %w: %s", err, tail(out))
		}
		return decodePaginatedPRs(out)
	}

	prs, err := listOpenPRs()
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if !isOwnedCanaryPR(pr, repo, runID) {
			continue
		}
		if closeOut, cerr := runGH("pr", "close", strconv.Itoa(pr.Number), "--repo", repo, "--delete-branch"); cerr != nil {
			return fmt.Errorf("closing sandbox PR #%d: %w: %s", pr.Number, cerr, tail(closeOut))
		}
	}

	// Verify no owned open PR remains after cleanup.
	remaining, err := listOpenPRs()
	if err != nil {
		return fmt.Errorf("verifying sandbox PR cleanup: %w", err)
	}
	for _, pr := range remaining {
		if isOwnedCanaryPR(pr, repo, runID) {
			return fmt.Errorf("sandbox PR #%d for run %s remained open after cleanup", pr.Number, runID)
		}
	}

	// Enumerate and delete owned remote branches independently of PR existence so
	// branches left behind when push succeeds and PR creation fails (or when a
	// prior attempt was interrupted before PR creation) are cleaned up safely.
	listOwnedBranches := func() ([]string, error) {
		out, err := runGH(ghAPI, "--paginate", fmt.Sprintf("repos/%s/git/matching-refs/heads/%s", repo, canaryBranchPrefix(runID)))
		if err != nil {
			return nil, fmt.Errorf("listing sandbox branches for run %s: %w: %s", runID, err, tail(out))
		}
		refs, err := decodePaginatedRefs(out)
		if err != nil {
			return nil, err
		}
		var branches []string
		for _, r := range refs {
			branch := strings.TrimPrefix(strings.TrimSpace(r.Ref), "refs/heads/")
			if isOwnedCanaryBranch(runID, branch) {
				branches = append(branches, branch)
			}
		}
		return branches, nil
	}

	branches, err := listOwnedBranches()
	if err != nil {
		return err
	}
	for _, branch := range branches {
		if delOut, derr := runGH(ghAPI, "-X", "DELETE", fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, branch)); derr != nil {
			return fmt.Errorf("deleting sandbox branch %s: %w: %s", branch, derr, tail(delOut))
		}
	}

	remainingBranches, err := listOwnedBranches()
	if err != nil {
		return fmt.Errorf("verifying sandbox branch cleanup: %w", err)
	}
	if len(remainingBranches) > 0 {
		return fmt.Errorf("sandbox branch %s for run %s remained after cleanup", remainingBranches[0], runID)
	}
	return nil
}

type sandboxOpenPR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	Body        string `json:"body"`
	Head        struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
			Fork     bool   `json:"fork"`
		} `json:"repo"`
	} `json:"head"`
}

func (p sandboxOpenPR) headRef() string {
	if p.Head.Ref != "" {
		return p.Head.Ref
	}
	return p.HeadRefName
}

func decodePaginatedPRs(raw []byte) ([]sandboxOpenPR, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var all []sandboxOpenPR
	for dec.More() {
		var page []sandboxOpenPR
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("decoding open sandbox PRs: %w", err)
		}
		all = append(all, page...)
	}
	return all, nil
}

type sandboxGitRef struct {
	Ref string `json:"ref"`
}

func decodePaginatedRefs(raw []byte) ([]sandboxGitRef, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	var all []sandboxGitRef
	for dec.More() {
		var page []sandboxGitRef
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("decoding sandbox git refs: %w", err)
		}
		all = append(all, page...)
	}
	return all, nil
}

func writeSiblingEvidence(siblingPath string, base update.CanaryEvidence) error {
	data, err := os.ReadFile(siblingPath) //nolint:gosec // explicit sibling release binary path
	if err != nil {
		return fmt.Errorf("reading sibling binary %s: %w", siblingPath, err)
	}
	sibBundle, err := os.ReadFile(siblingPath + ".bundle") //nolint:gosec // explicit sibling release bundle path
	if err != nil {
		return fmt.Errorf("reading sibling bundle %s.bundle: %w", siblingPath, err)
	}
	if err := verifyCandidateBundle(filepath.Base(siblingPath), data, sibBundle); err != nil {
		return fmt.Errorf("verifying sibling bundle %s.bundle: %w", siblingPath, err)
	}
	goos, goarch, err := parsePlatformAssetName(filepath.Base(siblingPath))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	sib := base
	sib.SHA256 = hex.EncodeToString(sum[:])
	sib.OS = goos
	sib.Arch = goarch
	raw, err := json.MarshalIndent(sib, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := update.ValidateEvidenceFor(raw, sib.Version, sib.SHA256, sib.OS, sib.Arch); err != nil {
		return fmt.Errorf("validating sibling evidence for %s: %w", siblingPath, err)
	}
	return os.WriteFile(siblingPath+".canary.json", raw, 0o600)
}

func parsePlatformAssetName(name string) (string, string, error) {
	trimmed := strings.TrimSuffix(name, ".exe")
	parts := strings.Split(trimmed, "-")
	if len(parts) != 3 || parts[0] != "beekeeper" || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("unrecognized release binary asset name %q: want beekeeper-<os>-<arch>", name)
	}
	return parts[1], parts[2], nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func oneLineReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if s == "" {
		return "canary failed"
	}
	return s
}

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
		"agents:\n  mode: background\n  dir: %s\nboard:\n  owner: fixture\n  project: 1\n  tidyEvery: -1s\n  order:\n  - name: ready\n    status: [Ready]\ncapacity:\n  ceiling: 2\n  availMinMiB: -1\n  swapGrowthMaxMiB: -1\nwatch:\n  loadMax: 1000\nidentity:\n  host: fixture\nstateDir: %s\ndisk:\n  checkouts: [%s]\ngithub:\n  floor: -1\n  allowedOrgs: [fixture]\nsupervisor:\n  workerBrief: %s\n  ownership:\n    authors: [fixture-bot]\n  reviewRounds: 1\n",
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

func isOwnedCanaryPR(repo, runID, headRefName, body string) bool {
	headRefName = strings.TrimSpace(headRefName)
	if !strings.HasPrefix(headRefName, canaryBranchPrefix(runID)) {
		return false
	}
	return containsExactLine(body, canaryOwnershipMarker(repo, runID, headRefName))
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

	dir, env, err := newIsolatedCanaryEnv(cfg.Scratch)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	for name, target := range map[string]string{ghCmd: ghBin, gitCmd: gitBin} {
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			return "", err
		}
	}
	claudeStub := filepath.Join(binDir, claudeCmd)
	if err := os.WriteFile(claudeStub, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // isolated sandbox stub executable
		return "", err
	}

	sandboxToken := strings.TrimSpace(os.Getenv("BEEKEEPER_CANARY_GITHUB_TOKEN"))
	if sandboxToken == "" {
		tokenCmd := exec.CommandContext(ctx, ghBin, "auth", "token") //nolint:gosec // verifies sandbox credentials
		out, terr := tokenCmd.CombinedOutput()
		if terr != nil || strings.TrimSpace(string(out)) == "" {
			return "", fmt.Errorf("sandbox credentials unavailable: gh auth token failed: %w (%s)", terr, tail(out))
		}
		sandboxToken = strings.TrimSpace(string(out))
	}

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4900")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(time.Hour).Unix()))
		switch r.URL.Path {
		case "/repos/giantswarm/beekeeper":
			_, _ = fmt.Fprint(w, `{}`)
		case "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":"canary-bot"},"rateLimit":{"limit":5000,"remaining":4900,"used":100,"resetAt":%q}}}`, now.Add(time.Hour).Format(time.RFC3339))
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

	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	env = append(env,
		"PATH="+binDir+":/usr/bin:/bin",
		"GH_TOKEN="+sandboxToken,
		"BEEKEEPER_GITHUB_API="+apiURL,
		"GIT_ALLOW_PROTOCOL=file:https",
		"GIT_AUTHOR_NAME=Beekeeper Canary",
		"GIT_AUTHOR_EMAIL=canary@example.invalid",
		"GIT_COMMITTER_NAME=Beekeeper Canary",
		"GIT_COMMITTER_EMAIL=canary@example.invalid",
	)
	for _, passthrough := range []string{"BEEKEEPER_CANARY_ORIGIN", "SANDBOX_DIR"} {
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

	checkout := filepath.Join(dir, "checkouts", filepath.Base(cfg.Repo))
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return "", err
	}
	stateDir := filepath.Join(dir, "state")
	briefPath := filepath.Join(dir, "worker-brief.md")
	if err := os.WriteFile(briefPath, []byte("Work {{.URL}} in {{.Repo}} (#{{.Number}}).\n"), 0o600); err != nil {
		return "", err
	}
	cfgText := fmt.Sprintf(
		"agents:\n  mode: background\n  dir: %s\nboard:\n  owner: %s\n  project: %d\n  tidyEvery: -1s\n  order:\n  - name: ready\n    status: [Ready]\ncapacity:\n  ceiling: 2\n  availMinMiB: -1\n  swapGrowthMaxMiB: -1\nwatch:\n  loadMax: 1000\nidentity:\n  host: canary\nstateDir: %s\ndisk:\n  checkouts: [%s]\ngithub:\n  floor: -1\n  allowedOrgs: [%s]\nsupervisor:\n  workerBrief: %s\n  ownership:\n    authors: [canary-bot]\n  reviewRounds: 1\n",
		checkout, cfg.AllowOrg, cfg.Project, stateDir, checkout, cfg.AllowOrg, briefPath,
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
		st.Budget = &state.Budget{
			Remaining: 5000,
			Limit:     5000,
			Reset:     now.Add(time.Hour),
			At:        now,
			GraphQL:   &state.GraphQL{Remaining: 5000, Limit: 5000, Reset: now.Add(time.Hour), At: now},
		}
		return nil, nil
	}); err != nil {
		return "", err
	}

	runCmd := func(workDir, bin string, args ...string) (string, error) {
		c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // isolated sandbox command
		c.Dir = workDir
		c.Env = env
		out, cerr := c.CombinedOutput()
		if cerr != nil {
			return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(args, " "), cerr, tail(out))
		}
		return strings.TrimSpace(string(out)), nil
	}

	// Verify the candidate passes isolated command checks without touching live config/state.
	for _, args := range [][]string{{"version"}, {"supervisor", "status"}} {
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

	branch := canaryBranchName(runID, candidateDigest)
	marker := canaryOwnershipMarker(cfg.Repo, runID, branch)
	digestLine := "candidate-sha256: " + candidateDigest
	projectLine := fmt.Sprintf("sandbox-project: %d", cfg.Project)
	body := strings.Join([]string{
		"Automated staged release-candidate canary run.",
		marker,
		digestLine,
		projectLine,
	}, "\n")

	if _, err := runCmd(checkout, gitBin, gitCheckout, "-b", branch); err != nil {
		return "", err
	}
	canaryFile := filepath.Join(checkout, ".beekeeper-canary")
	if err := os.WriteFile(canaryFile, []byte(body+"\n"), 0o600); err != nil {
		return "", err
	}
	if _, err := runCmd(checkout, gitBin, gitAdd, ".beekeeper-canary"); err != nil {
		return "", err
	}
	if _, err := runCmd(checkout, gitBin, gitCommit, "-m", "chore(canary): stage "+runID); err != nil {
		return "", err
	}
	headSHA, err := runCmd(checkout, gitBin, gitRevParse, gitHead)
	if err != nil {
		return "", err
	}
	if _, err := runCmd(checkout, gitBin, gitPush, originRemote, branch); err != nil {
		return "", err
	}

	createOut, err := runCmd(checkout, ghBin, "pr", "create", "--repo", cfg.Repo, "--base", mainBranch, "--head", branch, "--title", "chore(canary): stage "+runID, "--body", body)
	if err != nil {
		return "", fmt.Errorf("creating sandbox canary PR: %w", err)
	}
	prNum, err := parseCreatedPRNumber(createOut)
	if err != nil {
		return "", fmt.Errorf("parsing created sandbox PR number from %q: %w", createOut, err)
	}
	prRef := fmt.Sprintf("%s#%d", cfg.Repo, prNum)

	// Park the candidate on the newly created sandbox PR so the candidate records
	// its own exact-head review request in the isolated state store.
	if _, err := runCmd(checkout, candidatePath, "--as", "canary worker", "agents", "park", "--on", prRef, "desk review"); err != nil {
		return "", fmt.Errorf("parking candidate on %s: %w", prRef, err)
	}
	if _, err := runCmd(checkout, gitBin, gitCheckout, mainBranch); err != nil {
		return "", err
	}

	reviewBody := "Review: approved\nHead reviewed: `" + headSHA + "`"
	if _, err := runCmd(dir, ghBin, "pr", "review", strconv.Itoa(prNum), "--repo", cfg.Repo, "--comment", "--body", reviewBody); err != nil {
		return "", fmt.Errorf("posting exact-head review on %s: %w", prRef, err)
	}
	reviewAt := time.Now().UTC()
	byLoop := state.Party{Name: partyLoop}
	for _, ev := range []state.Event{
		{At: reviewAt.Add(-time.Minute), By: byLoop, Verb: verbLoop, Detail: detailStarted},
		{At: reviewAt.Add(-time.Minute), By: byLoop, Verb: verbBudgetState, Detail: detailFalse},
		{At: reviewAt, By: byLoop, Verb: "supervisor.review", Subject: prRef, Detail: "[review " + prRef + "@" + headSHA + "] canary-reviewer"},
	} {
		if err := store.Record(ev); err != nil {
			return "", err
		}
	}

	// Drive the candidate's supervisor loop to validate the reviewed PR, merge
	// worktree, check rollup, and execute exact-head merge.
	for range 2 {
		if _, err := runCmd(dir, candidatePath, "supervisor", "loop", "--once"); err != nil {
			return "", fmt.Errorf("running candidate supervisor loop: %w", err)
		}
	}

	// Verify the fresh PR created by this run was reviewed on headSHA, passed
	// checks, and merged into origin.
	viewOut, err := runCmd(dir, ghBin, "pr", "view", strconv.Itoa(prNum), "--repo", cfg.Repo, "--json", "number,state,headRefName,headRefOid,body,reviews,statusCheckRollup,mergeCommit")
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
	if observed.Number != prNum || observed.HeadRefName != branch || observed.HeadRefOid != headSHA {
		return "", fmt.Errorf("sandbox PR %s head mismatch: got #%d ref=%s oid=%s, want #%d ref=%s oid=%s", prRef, observed.Number, observed.HeadRefName, observed.HeadRefOid, prNum, branch, headSHA)
	}
	if !isOwnedCanaryPR(cfg.Repo, runID, observed.HeadRefName, observed.Body) || !containsExactLine(observed.Body, digestLine) || !containsExactLine(observed.Body, projectLine) {
		return "", fmt.Errorf("sandbox PR %s ownership or candidate digest marker mismatch", prRef)
	}
	hasExactReview := slices.ContainsFunc(observed.Reviews, func(r struct {
		Body string `json:"body"`
	}) bool {
		return strings.Contains(r.Body, "Review: approved") && strings.Contains(r.Body, "Head reviewed: `"+headSHA+"`")
	})
	if !hasExactReview {
		return "", fmt.Errorf("sandbox PR %s lacks exact-head approval for %s", prRef, headSHA)
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

	runGH := func(args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, ghBin, args...) //nolint:gosec // allowlisted sandbox cleanup
		if len(env) > 0 {
			c.Env = env
		}
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
		ref := pr.headRef()
		if !isOwnedCanaryPR(repo, runID, ref, pr.Body) {
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
		if isOwnedCanaryPR(repo, runID, pr.headRef(), pr.Body) {
			return fmt.Errorf("sandbox PR #%d for run %s remained open after cleanup", pr.Number, runID)
		}
	}
	return nil
}

type sandboxOpenPR struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	Body        string `json:"body"`
	Head        struct {
		Ref string `json:"ref"`
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

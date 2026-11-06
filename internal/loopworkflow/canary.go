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
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// CanaryTimeout bounds a staged canary run so hung commands fail closed.
const CanaryTimeout = 90 * time.Second

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
// Any failure, timeout, missing permission, or unobserved merge returns a
// non-success evidence record and an error.
func StageCanary(parentCtx context.Context, cfg CanaryConfig) (update.CanaryEvidence, error) {
	ctx, cancel := context.WithTimeout(parentCtx, CanaryTimeout)
	defer cancel()

	runID := strings.TrimSpace(cfg.Run)
	if runID == "" {
		runID = fmt.Sprintf("canary-%d", time.Now().UTC().UnixNano())
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

	if bundlePath := strings.TrimSpace(cfg.Bundle); bundlePath != "" {
		bundleBytes, berr := os.ReadFile(bundlePath) //nolint:gosec // explicit candidate bundle path
		if berr != nil {
			return fail(fmt.Errorf("reading candidate bundle: %w", berr))
		}
		if verr := update.VerifyCandidateBundle(filepath.Base(candidatePath), binaryBytes, bundleBytes); verr != nil {
			return fail(fmt.Errorf("verifying candidate bundle: %w", verr))
		}
	}

	if err := validateSandboxConfig(cfg); err != nil {
		return fail(err)
	}

	ver, commit, err := candidateIdentity(ctx, candidatePath)
	if err != nil {
		return fail(err)
	}
	ev.Version = ver
	ev.Commit = commit

	var mergeOID string
	if cfg.Recorded {
		mergeOID, err = runRecordedCanary(ctx, candidatePath, cfg)
	} else {
		mergeOID, err = runLiveSandboxCanary(ctx, candidatePath, cfg, runID)
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
	if strings.EqualFold(repo, update.Repository) {
		return fmt.Errorf("sandbox repository %q must not be the production release repository", repo)
	}
	return nil
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

func candidateIdentity(ctx context.Context, candidatePath string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, candidatePath, "version") //nolint:gosec // verified staged candidate binary
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
		if _, err := p.run(filepath.Join("checkouts", "project"), "remote", "set-url", originRemote, p.path("missing.git")); err != nil {
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
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "PATH=" + p.path("bin"), "LANG=C.UTF-8", "TZ=UTC",
		"BEEKEEPER_CONFIG=" + p.path("config.yaml"), "FIXTURE_DIR=" + p.dir, "FIXTURE_GIT=" + p.git, "FIXTURE_CASE=" + scenario,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=file", "GIT_TERMINAL_PROMPT=0",
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
	byLoop := state.Party{Name: "supervisor loop"}
	byPrior := state.Party{Name: "prior-worker"}
	for _, e := range []state.Event{
		{At: p.now.Add(-time.Minute), By: byLoop, Verb: "supervisor.loop", Detail: "started"},
		{At: p.now.Add(-time.Minute), By: byLoop, Verb: "watch.budget-state", Detail: "false"},
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

func runLiveSandboxCanary(ctx context.Context, candidatePath string, cfg CanaryConfig, runID string) (string, error) {
	ghBin, err := exec.LookPath(ghCmd)
	if err != nil {
		return "", fmt.Errorf("locating gh CLI for sandbox canary: %w", err)
	}
	tokenCmd := exec.CommandContext(ctx, ghBin, "auth", "token") //nolint:gosec // verifies sandbox credentials
	if out, err := tokenCmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("sandbox credentials unavailable: gh auth token failed: %w (%s)", err, tail(out))
	}

	// Sweep only resources owned by this canary run ID on entry and exit.
	_ = CleanupSandboxRun(ctx, ghBin, cfg.Repo, cfg.AllowOrg, runID)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = CleanupSandboxRun(cleanupCtx, ghBin, cfg.Repo, cfg.AllowOrg, runID)
	}()

	dir, err := os.MkdirTemp(cfg.Scratch, "sandbox-canary-*")
	if err != nil {
		return "", fmt.Errorf("creating sandbox scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Verify the candidate passes isolated command checks before any sandbox mutation.
	for _, args := range [][]string{{"version"}, {"supervisor", "status"}} {
		c := exec.CommandContext(ctx, candidatePath, args...) //nolint:gosec // verified staged candidate
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			return "", fmt.Errorf("candidate %s: %w: %s", strings.Join(args, " "), err, tail(out))
		}
	}

	// Observe merged PR state for this canary run from the allowlisted sandbox repo.
	viewCmd := exec.CommandContext(ctx, ghBin, "pr", "list", "--repo", cfg.Repo, "--state", "merged", "--search", "canary-run:"+runID, "--json", "number,state,mergeCommit") //nolint:gosec // allowlisted sandbox repo query
	out, err := viewCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("querying sandbox repository %s: %w: %s", cfg.Repo, err, tail(out))
	}
	var merged []struct {
		State       string `json:"state"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(out, &merged); err != nil {
		return "", fmt.Errorf("parsing sandbox PR list: %w", err)
	}
	for _, pr := range merged {
		if pr.State == mergedState && pr.MergeCommit.OID != "" {
			return pr.MergeCommit.OID, nil
		}
	}
	return "", fmt.Errorf("no merged pull request observed in sandbox %s for run %s", cfg.Repo, runID)
}

// CleanupSandboxRun idempotently closes open pull requests and deletes branches
// owned by runID in the allowlisted sandbox repository, leaving unrelated
// resources untouched.
func CleanupSandboxRun(ctx context.Context, ghBin, repo, allowOrg, runID string) error {
	if err := validateSandboxConfig(CanaryConfig{Repo: repo, AllowOrg: allowOrg, Project: 1}); err != nil {
		return err
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("sandbox cleanup requires a non-empty run ID")
	}
	prefix := "canary/" + runID
	marker := "canary-run: " + runID

	listCmd := exec.CommandContext(ctx, ghBin, "pr", "list", "--repo", repo, "--state", "open", "--json", "number,headRefName,body") //nolint:gosec // allowlisted sandbox cleanup
	out, err := listCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing open sandbox PRs: %w: %s", err, tail(out))
	}
	var prs []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return fmt.Errorf("decoding open sandbox PRs: %w", err)
	}
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, prefix) && !strings.Contains(pr.Body, marker) {
			continue
		}
		closeCmd := exec.CommandContext(ctx, ghBin, "pr", "close", fmt.Sprint(pr.Number), "--repo", repo, "--delete-branch") //nolint:gosec // owned sandbox PR cleanup
		if closeOut, cerr := closeCmd.CombinedOutput(); cerr != nil {
			return fmt.Errorf("closing sandbox PR #%d: %w: %s", pr.Number, cerr, tail(closeOut))
		}
	}
	return nil
}

func writeSiblingEvidence(siblingPath string, base update.CanaryEvidence) error {
	data, err := os.ReadFile(siblingPath) //nolint:gosec // explicit sibling release binary path
	if err != nil {
		return fmt.Errorf("reading sibling binary %s: %w", siblingPath, err)
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

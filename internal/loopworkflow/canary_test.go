package loopworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	testOrg         = "giantswarm"
	testSandboxRepo = "giantswarm/beekeeper-Stage-Canary"
)

func acceptTestBundles(t *testing.T) {
	t.Helper()
	prev := verifyCandidateBundle
	verifyCandidateBundle = func(assetName string, binary, bundle []byte) error {
		if len(binary) == 0 {
			return errors.New("empty candidate binary")
		}
		if string(bundle) != "valid-bundle:"+assetName {
			return fmt.Errorf("invalid bundle for %s", assetName)
		}
		return nil
	}
	t.Cleanup(func() { verifyCandidateBundle = prev })
}

func writeBundle(t *testing.T, binaryPath string) string {
	t.Helper()
	bundlePath := binaryPath + ".bundle"
	content := "valid-bundle:" + filepath.Base(binaryPath)
	if err := os.WriteFile(bundlePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return bundlePath
}

func TestValidateSandboxConfigRefusesUnsafeTargets(t *testing.T) {
	cases := []struct {
		name    string
		cfg     CanaryConfig
		wantErr string
	}{
		{
			name:    "missing flags without recorded",
			cfg:     CanaryConfig{},
			wantErr: "requires explicit --canary-repo, --canary-allow-org and --canary-project",
		},
		{
			name: "recorded combined with repo",
			cfg: CanaryConfig{
				Recorded: true,
				Repo:     "giantswarm/sandbox-canary",
			},
			wantErr: "cannot be combined",
		},
		{
			name: "recorded combined with siblings",
			cfg: CanaryConfig{
				Recorded: true,
				Siblings: []string{"beekeeper-darwin-arm64"},
			},
			wantErr: "cannot emit sibling release evidence",
		},
		{
			name: "invalid repo slug",
			cfg: CanaryConfig{
				Repo:     "invalid-slug",
				AllowOrg: testOrg,
				Project:  1,
			},
			wantErr: "invalid sandbox repository",
		},
		{
			name: "outside allowlisted org",
			cfg: CanaryConfig{
				Repo:     "other-org/sandbox-canary",
				AllowOrg: testOrg,
				Project:  1,
			},
			wantErr: "outside allowlisted organization",
		},
		{
			name: "production release repository refused",
			cfg: CanaryConfig{
				Repo:     "giantswarm/beekeeper",
				AllowOrg: testOrg,
				Project:  1,
			},
			wantErr: "must not be a production repository",
		},
		{
			name: "production devctl repository refused",
			cfg: CanaryConfig{
				Repo:     "giantswarm/devctl",
				AllowOrg: testOrg,
				Project:  1,
			},
			wantErr: "must not be a production repository",
		},
		{
			name: "valid dedicated sandbox repository",
			cfg: CanaryConfig{
				Repo:     testSandboxRepo,
				AllowOrg: testOrg,
				Project:  7,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSandboxConfig(tc.cfg)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("got %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestStageCanaryRefusesRunningExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ev, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: exe,
		Recorded:  true,
		Run:       "self-refusal",
	})
	if err == nil || !strings.Contains(err.Error(), "refuses to run against the live installed executable") {
		t.Fatalf("got err=%v, want live executable refusal", err)
	}
	if ev.Status != update.CanaryStatusFailed || ev.MergeCommit != "" || ev.Reason == "" {
		t.Fatalf("unexpected evidence on refusal: %+v", ev)
	}
}

// Finding 4 & 5: Bundle verification is mandatory before candidate execution in
// live mode, invalid bundles prevent candidate execution, and recorded mode
// cannot grant release eligibility or emit sibling evidence.
func TestCanaryRequiresAndVerifiesCandidateAndSiblingBundlesBeforeExecution(t *testing.T) {
	acceptTestBundles(t)
	dir := t.TempDir()
	executedMarker := filepath.Join(dir, "candidate-executed")
	candidate := filepath.Join(dir, "beekeeper-linux-amd64")
	script := fmt.Sprintf("#!/bin/sh\necho ran > %q\necho 'beekeeper v1.2.3 (commit 0123456789abcdef0123456789abcdef01234567)'\n", executedMarker)
	if err := os.WriteFile(candidate, []byte(script), 0o700); err != nil { //nolint:gosec // test script executable
		t.Fatal(err)
	}

	// 1. Missing --canary-bundle in live mode fails before executing candidate.
	ev, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: candidate,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "no-bundle",
	})
	if err == nil || !strings.Contains(err.Error(), "requires --canary-bundle") || ev.Status != update.CanaryStatusFailed {
		t.Fatalf("missing bundle: ev=%+v err=%v", ev, err)
	}
	if _, statErr := os.Stat(executedMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("candidate was executed despite missing bundle")
	}

	// 2. Tampered --canary-bundle fails before executing candidate.
	badBundle := filepath.Join(dir, "bad.bundle")
	if err := os.WriteFile(badBundle, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev, err = StageCanary(t.Context(), CanaryConfig{
		Candidate: candidate,
		Bundle:    badBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "bad-bundle",
	})
	if err == nil || !strings.Contains(err.Error(), "verifying candidate bundle") || ev.Status != update.CanaryStatusFailed {
		t.Fatalf("bad bundle: ev=%+v err=%v", ev, err)
	}
	if _, statErr := os.Stat(executedMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("candidate was executed despite invalid bundle")
	}

	// 3. Recorded evidence is rejected by update.ValidateEvidence so fixture runs
	// can never grant release installation eligibility.
	candBytes, err := os.ReadFile(candidate) //nolint:gosec // test candidate fixture
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(candBytes)
	digest := hex.EncodeToString(sum[:])
	recRaw := mustJSON(update.CanaryEvidence{
		Schema:      1,
		Version:     "v1.2.3",
		SHA256:      digest,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Status:      update.CanaryStatusSuccess,
		Run:         update.RecordedRunPrefix + "fixture-1",
		MergeCommit: "0123456789abcdef0123456789abcdef01234567",
	})
	if _, verr := update.ValidateEvidence(recRaw, "v1.2.3", digest); verr == nil || !strings.Contains(verr.Error(), "recorded fixture canary evidence cannot grant release eligibility") {
		t.Fatalf("ValidateEvidence accepted recorded evidence: %v", verr)
	}

	// 4. writeSiblingEvidence requires and verifies <sibling>.bundle.
	sibling := filepath.Join(dir, "beekeeper-darwin-arm64")
	if err := os.WriteFile(sibling, []byte("darwin-arm64-bytes"), 0o700); err != nil { //nolint:gosec // test binary fixture
		t.Fatal(err)
	}
	baseEv := update.CanaryEvidence{
		Schema:      1,
		Version:     "v1.2.3",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		SHA256:      digest,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Status:      update.CanaryStatusSuccess,
		Run:         "live-run-1",
		MergeCommit: "fedcba9876543210fedcba9876543210fedcba98",
	}
	if serr := writeSiblingEvidence(sibling, baseEv); serr == nil || !strings.Contains(serr.Error(), "reading sibling bundle") {
		t.Fatalf("missing sibling bundle: %v", serr)
	}
	if err := os.WriteFile(sibling+".bundle", []byte("wrong-bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if serr := writeSiblingEvidence(sibling, baseEv); serr == nil || !strings.Contains(serr.Error(), "verifying sibling bundle") {
		t.Fatalf("invalid sibling bundle: %v", serr)
	}
	writeBundle(t, sibling)
	if serr := writeSiblingEvidence(sibling, baseEv); serr != nil {
		t.Fatalf("valid sibling bundle: %v", serr)
	}
	sibRaw, err := os.ReadFile(sibling + ".canary.json") //nolint:gosec // test sibling evidence fixture
	if err != nil {
		t.Fatal(err)
	}
	sibSum := sha256.Sum256([]byte("darwin-arm64-bytes"))
	if _, err := update.ValidateEvidenceFor(sibRaw, "v1.2.3", hex.EncodeToString(sibSum[:]), "darwin", "arm64"); err != nil {
		t.Fatalf("ValidateEvidenceFor(sibling) failed: %v", err)
	}
}

// Finding 6: Cleanup ownership matching is delimited and bound to (repo, runID,
// branch), leaving prefix-colliding runs (run-42 vs run-420) and copied markers
// untouched, while paginating through open PRs.
func TestCleanupSandboxRunDelimitedOwnership(t *testing.T) {
	dir := t.TempDir()
	closedFile := filepath.Join(dir, "closed.txt")
	ghStub := filepath.Join(dir, "gh")

	ownedBranch := canaryBranchName("run-42", "abcdef1234567890")
	ownedMarker := canaryOwnershipMarker(testSandboxRepo, "run-42", ownedBranch)
	collidingBranch := canaryBranchName("run-420", "abcdef1234567890")
	collidingMarker := canaryOwnershipMarker(testSandboxRepo, "run-420", collidingBranch)

	// Emit two concatenated JSON pages as `gh api --paginate` does, then after
	// PR #11 is closed, omit #11 on the verification call.
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$1" = "api" ] && [ "$2" = "--paginate" ]; then
  if [ -f %q ]; then
    printf '[
      {"number":12,"head":{"ref":%q},"body":%q},
      {"number":13,"head":{"ref":"feat/unrelated"},"body":%q}
    ]'
    exit 0
  fi
  printf '[
    {"number":11,"head":{"ref":%q},"body":%q},
    {"number":12,"head":{"ref":%q},"body":%q}
  ][
    {"number":13,"head":{"ref":"feat/unrelated"},"body":%q},
    {"number":14,"head":{"ref":"canary/run-42/cand-other"},"body":%q}
  ]'
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "close" ]; then
  echo "$*" >> %q
  exit 0
fi
exit 1
`, closedFile, collidingBranch, collidingMarker, ownedMarker,
		ownedBranch, ownedMarker, collidingBranch, collidingMarker,
		ownedMarker, ownedMarker, closedFile)

	if err := os.WriteFile(ghStub, []byte(script), 0o700); err != nil { //nolint:gosec // test stub executable
		t.Fatal(err)
	}

	if err := CleanupSandboxRun(t.Context(), ghStub, testSandboxRepo, testOrg, "run-42"); err != nil {
		t.Fatalf("CleanupSandboxRun failed: %v", err)
	}
	closedBytes, err := os.ReadFile(closedFile) //nolint:gosec // test output file
	if err != nil {
		t.Fatalf("expected PR #11 to be closed: %v", err)
	}
	got := strings.TrimSpace(string(closedBytes))
	want := "pr close 11 --repo " + testSandboxRepo + " --delete-branch"
	if got != want {
		t.Fatalf("closed commands = %q, want only %q", got, want)
	}
}

// sandboxHarness sets up a local bare git origin and stateful gh CLI stub so
// live sandbox canary runs can be tested end-to-end without network access.
type sandboxHarness struct {
	dir        string
	originPath string
	statePath  string
}

func newSandboxHarness(t *testing.T, repo string) *sandboxHarness {
	t.Helper()
	dir := t.TempDir()
	originPath := filepath.Join(dir, "origin.git")
	workPath := filepath.Join(dir, "seed")
	statePath := filepath.Join(dir, "prs.json")
	if err := os.MkdirAll(workPath, 0o700); err != nil {
		t.Fatal(err)
	}

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitEnv := []string{
		"HOME=" + dir,
		"PATH=" + filepath.Dir(gitBin) + ":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Seed",
		"GIT_AUTHOR_EMAIL=seed@example.invalid",
		"GIT_COMMITTER_NAME=Seed",
		"GIT_COMMITTER_EMAIL=seed@example.invalid",
	}
	runGit := func(cwd string, args ...string) {
		c := exec.CommandContext(t.Context(), gitBin, args...) //nolint:gosec // test git fixture
		c.Dir = cwd
		c.Env = gitEnv
		if out, gerr := c.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v: %s", args, gerr, out)
		}
	}
	runGit(dir, "init", "--bare", originPath)
	runGit(workPath, "init", "-b", "main")
	runGit(workPath, "remote", "add", "origin", originPath)
	if err := os.WriteFile(filepath.Join(workPath, "README.md"), []byte("sandbox base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(workPath, "add", "README.md")
	runGit(workPath, "commit", "-m", "initial")
	runGit(workPath, "push", "origin", "main")

	// Seed a historical merged PR and a spoofed open PR to prove fresh runs never
	// reuse historical search results or touch unrelated open PRs.
	initialPRs := []map[string]any{
		{
			"number":      1,
			"state":       "MERGED",
			"headRefName": "canary/run-fresh/cand-historical",
			"headRefOid":  "1111111111111111111111111111111111111111",
			"body":        "historical merged PR\ncanary-run: run-fresh\n",
			"mergeCommit": map[string]any{"oid": "2222222222222222222222222222222222222222"},
			"reviews":     []any{},
			"statusCheckRollup": []any{
				map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"},
			},
		},
		{
			"number":      2,
			"state":       "OPEN",
			"headRefName": "canary/run-420/cand-other",
			"headRefOid":  "3333333333333333333333333333333333333333",
			"body":        canaryOwnershipMarker(repo, "run-420", "canary/run-420/cand-other"),
			"mergeCommit": map[string]any{"oid": ""},
			"reviews":     []any{},
			"statusCheckRollup": []any{
				map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"},
			},
		},
	}
	raw, err := json.Marshal(initialPRs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	h := &sandboxHarness{
		dir:        dir,
		originPath: originPath,
		statePath:  statePath,
	}
	h.installTools(t, repo, gitBin)
	return h
}

func (h *sandboxHarness) installTools(t *testing.T, repo, gitBin string) {
	t.Helper()
	binDir := filepath.Join(h.dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitBin, filepath.Join(binDir, "git")); err != nil {
		t.Fatal(err)
	}

	ghScript := fmt.Sprintf(`#!/usr/bin/env python3
import json, os, subprocess, sys

state_path = %q
origin_path = %q
repo = %q
git_bin = %q
fail_close = os.environ.get("SANDBOX_FAIL_CLOSE") == "1"

with open(state_path, "r") as f:
    prs = json.load(f)

def save():
    with open(state_path, "w") as f:
        json.dump(prs, f)

args = sys.argv[1:]
if args[:2] == ["auth", "token"]:
    print("sandbox-gh-token")
    sys.exit(0)

if args[:2] == ["api", "--paginate"]:
    open_prs = []
    for p in prs:
        if p["state"] == "OPEN":
            open_prs.append({"number": p["number"], "head": {"ref": p["headRefName"]}, "body": p["body"]})
    print(json.dumps(open_prs))
    sys.exit(0)

if args[:2] == ["pr", "create"]:
    head = args[args.index("--head") + 1]
    body = args[args.index("--body") + 1]
    oid = subprocess.check_output([git_bin, "--git-dir=" + origin_path, "rev-parse", "refs/heads/" + head], text=True).strip()
    num = max(p["number"] for p in prs) + 1
    prs.append({
        "number": num,
        "state": "OPEN",
        "headRefName": head,
        "headRefOid": oid,
        "body": body,
        "mergeCommit": {"oid": ""},
        "reviews": [],
        "statusCheckRollup": [{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}],
    })
    save()
    print(f"https://github.com/{repo}/pull/{num}")
    sys.exit(0)

if args[:2] == ["pr", "review"]:
    num = int(args[2])
    body = args[args.index("--body") + 1]
    for p in prs:
        if p["number"] == num:
            p["reviews"].append({"body": body})
    save()
    sys.exit(0)

if args[:2] == ["pr", "merge"]:
    num = int(args[2])
    match_oid = args[args.index("--match-head-commit") + 1]
    for p in prs:
        if p["number"] == num:
            if p["headRefOid"] != match_oid:
                sys.exit(1)
            p["state"] = "MERGED"
            p["mergeCommit"] = {"oid": "9999999999999999999999999999999999999999"}
            if os.path.exists(state_path + ".leave-open"):
                prs.append({
                    "number": num + 100,
                    "state": "OPEN",
                    "headRefName": p["headRefName"],
                    "headRefOid": p["headRefOid"],
                    "body": p["body"],
                    "mergeCommit": {"oid": ""},
                    "reviews": [],
                    "statusCheckRollup": [],
                })
    save()
    sys.exit(0)

if args[:2] == ["pr", "view"]:
    num = int(args[2])
    for p in prs:
        if p["number"] == num:
            print(json.dumps(p))
            sys.exit(0)
    sys.exit(1)

if args[:2] == ["pr", "close"]:
    if fail_close or os.path.exists(state_path + ".fail-close"):
        sys.stderr.write("simulated close permission error\n")
        sys.exit(1)
    num = int(args[2])
    for p in prs:
        if p["number"] == num:
            p["state"] = "CLOSED"
    save()
    sys.exit(0)

sys.stderr.write("unhandled gh args: " + " ".join(args) + "\n")
sys.exit(1)
`, h.statePath, h.originPath, repo, gitBin)

	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghScript), 0o700); err != nil { //nolint:gosec // test gh script
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":/usr/bin:/bin")
	t.Setenv("BEEKEEPER_CANARY_ORIGIN", h.originPath)
}

func writeFakeCandidate(t *testing.T, dir, name string, mergeOnLoop bool, sentinelHome, sentinelCfg string) string {
	t.Helper()
	candPath := filepath.Join(dir, name)
	mergeFlag := "0"
	if mergeOnLoop {
		mergeFlag = "1"
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${SECRET_SENTINEL:-}" != "" ]; then
  echo "leaked SECRET_SENTINEL" >&2
  exit 1
fi
if [ "${HOME:-}" = %q ] || [ "${BEEKEEPER_CONFIG:-}" = %q ]; then
  echo "inherited live HOME or BEEKEEPER_CONFIG" >&2
  exit 1
fi
case "$*" in
  version)
    echo "beekeeper v1.2.3 (commit 0123456789abcdef0123456789abcdef01234567)"
    exit 0
    ;;
  "supervisor status")
    exit 0
    ;;
  *"agents park --on "*)
    ref=""
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--on" ]; then
        ref="$arg"
      fi
      prev="$arg"
    done
    head="$(git rev-parse HEAD)"
    printf '%%s\n%%s\n' "$ref" "$head" > "${HOME}/parked-pr"
    exit 0
    ;;
  "supervisor loop --once")
    if [ %q = "1" ] && [ -f "${HOME}/parked-pr" ]; then
      ref="$(sed -n '1p' "${HOME}/parked-pr")"
      head="$(sed -n '2p' "${HOME}/parked-pr")"
      repo="${ref%%%%#*}"
      num="${ref##*#}"
      gh pr merge "$num" --repo "$repo" --squash --delete-branch --match-head-commit "$head"
      rm -f "${HOME}/parked-pr"
    fi
    exit 0
    ;;
esac
exit 0
`, sentinelHome, sentinelCfg, mergeFlag)

	if err := os.WriteFile(candPath, []byte(script), 0o700); err != nil { //nolint:gosec // test candidate executable
		t.Fatal(err)
	}
	return candPath
}

// Findings 2, 3, & 7: Live sandbox canary executes a fresh candidate-bound
// workflow in an isolated environment, rejects historical/spoofed merged PRs
// when the fresh PR does not merge, and fails closed if exit cleanup fails.
func TestLiveSandboxCanaryFreshWorkflowIsolationAndCleanupGate(t *testing.T) {
	acceptTestBundles(t)
	h := newSandboxHarness(t, testSandboxRepo)

	// Set sentinel live desk environment variables to prove the candidate never
	// inherits live HOME, BEEKEEPER_CONFIG, or secret env vars (Finding 3).
	sentinelHome := filepath.Join(t.TempDir(), "live-home")
	sentinelCfg := filepath.Join(sentinelHome, "live-config.yaml")
	t.Setenv("HOME", sentinelHome)
	t.Setenv("XDG_CONFIG_HOME", sentinelHome)
	t.Setenv("XDG_STATE_HOME", sentinelHome)
	t.Setenv("BEEKEEPER_CONFIG", sentinelCfg)
	t.Setenv("SECRET_SENTINEL", "top-secret-live-value")

	dir := t.TempDir()
	goodCand := writeFakeCandidate(t, dir, "beekeeper-linux-amd64", true, sentinelHome, sentinelCfg)
	goodBundle := writeBundle(t, goodCand)

	// 1. Fresh run completes worker park -> review -> loop merge -> observed merge commit.
	ev, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: goodCand,
		Bundle:    goodBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "run-fresh",
	})
	if err != nil {
		t.Fatalf("StageCanary(live fresh) failed: %v", err)
	}
	if ev.Status != update.CanaryStatusSuccess || ev.MergeCommit != "9999999999999999999999999999999999999999" {
		t.Fatalf("unexpected fresh evidence (should not use historical 2222...): %+v", ev)
	}

	// 2. A candidate that does not merge its fresh PR fails even though PR #1 in
	// the repo is already MERGED with "canary-run: run-fresh" in its body (Finding 2),
	// and exit cleanup closes the unmerged fresh PR.
	noMergeCand := writeFakeCandidate(t, dir, "beekeeper-nomerge", false, sentinelHome, sentinelCfg)
	noMergeBundle := writeBundle(t, noMergeCand)
	failedEv, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: noMergeCand,
		Bundle:    noMergeBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "run-fresh",
	})
	if err == nil || failedEv.Status != update.CanaryStatusFailed || failedEv.MergeCommit != "" {
		t.Fatalf("expected non-merging candidate to fail despite historical merged PR: ev=%+v err=%v", failedEv, err)
	}

	// 3. When exit cleanup fails even after a successful merge (Finding 7),
	// StageCanary fails closed and never returns Status == "success".
	if err := os.WriteFile(h.statePath+".leave-open", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.statePath+".fail-close", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupFailEv, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: goodCand,
		Bundle:    goodBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "run-cleanup-fail",
	})
	if err == nil || !strings.Contains(err.Error(), "exit sandbox cleanup") || cleanupFailEv.Status != update.CanaryStatusFailed || cleanupFailEv.MergeCommit != "" {
		t.Fatalf("expected cleanup failure to prevent success: ev=%+v err=%v", cleanupFailEv, err)
	}
}

// Finding 1 & Finding 5: .circleci/publish-canary-evidence.sh fails closed when
// sandbox variables or bundles are missing, and copies the runner executable so
// staging ./beekeeper-linux-amd64 succeeds without self-execution refusal.
func TestPublishCanaryEvidenceScript(t *testing.T) {
	scriptPath, err := filepath.Abs("../../.circleci/publish-canary-evidence.sh")
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	candPath := filepath.Join(workDir, "beekeeper-linux-amd64")
	sibPath := filepath.Join(workDir, "beekeeper-darwin-arm64")

	// Fake binary that refuses if invoked directly as ./beekeeper-linux-amd64
	// with --stage-canary ./beekeeper-linux-amd64 (matching sameExecutable),
	// and succeeds when invoked from the copied runner executable.
	candScript := `#!/bin/sh
set -eu
self_real="$(readlink -f "$0")"
cand=""
output=""
sib=""
bundle=""
repo=""
prev=""
for arg in "$@"; do
  case "$prev" in
    --stage-canary) cand="$arg" ;;
    --canary-output) output="$arg" ;;
    --canary-sibling) sib="$arg" ;;
    --canary-bundle) bundle="$arg" ;;
    --canary-repo) repo="$arg" ;;
  esac
  prev="$arg"
done
cand_real="$(readlink -f "$cand")"
if [ "$self_real" = "$cand_real" ]; then
  echo "canary refuses to run against the live installed executable" >&2
  exit 1
fi
if [ -z "$bundle" ] || [ -z "$repo" ]; then
  echo "missing required --canary-bundle or --canary-repo" >&2
  exit 1
fi
printf '{"schema":1,"version":"v1.2.3","sha256":"abc","os":"linux","arch":"amd64","status":"success","run":"r1","merge_commit":"m1"}\n' > "$output"
if [ -n "$sib" ]; then
  printf '{"schema":1,"version":"v1.2.3","sha256":"def","os":"darwin","arch":"arm64","status":"success","run":"r1","merge_commit":"m1"}\n' > "${sib}.canary.json"
fi
`
	if err := os.WriteFile(candPath, []byte(candScript), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	if err := os.WriteFile(sibPath, []byte("sibling-binary"), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}

	runPublisher := func(extraEnv ...string) (string, error) {
		c := exec.CommandContext(t.Context(), "bash", scriptPath) //nolint:gosec // test publisher script execution
		c.Dir = workDir
		c.Env = append([]string{
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"HOME=" + workDir,
			"TMPDIR=" + workDir,
			"BEEKEEPER_CANARY_ARTIFACTS_DIR=" + filepath.Join(workDir, "artifacts"),
			"BEEKEEPER_CANARY_BLOBS_FILE=" + filepath.Join(workDir, "blobs.txt"),
		}, extraEnv...)
		out, cerr := c.CombinedOutput()
		return string(out), cerr
	}

	// 1. Incomplete sandbox provisioning fails closed (no fallback to recorded mode).
	if out, cerr := runPublisher("BEEKEEPER_CANARY_REPO=" + testSandboxRepo); cerr == nil || !strings.Contains(out, "refusing to fall back to recorded fixture mode") {
		t.Fatalf("incomplete provisioning should fail closed: err=%v out=%s", cerr, out)
	}

	// 2. Missing bundle fails closed before staging.
	fullEnv := []string{
		"BEEKEEPER_CANARY_REPO=" + testSandboxRepo,
		"BEEKEEPER_CANARY_ALLOW_ORG=" + testOrg,
		"BEEKEEPER_CANARY_PROJECT=7",
	}
	if out, cerr := runPublisher(fullEnv...); cerr == nil || !strings.Contains(out, "signature bundle") {
		t.Fatalf("missing candidate bundle should fail closed: err=%v out=%s", cerr, out)
	}

	// 3. With candidate and sibling bundles present, the script copies the runner
	// binary, stages ./beekeeper-linux-amd64 and its sibling, and records blobs.
	if err := os.WriteFile(candPath+".bundle", []byte("cand-bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibPath+".bundle", []byte("sib-bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, cerr := runPublisher(fullEnv...); cerr != nil {
		t.Fatalf("publish-canary-evidence.sh failed: %v\n%s", cerr, out)
	}
	blobs, err := os.ReadFile(filepath.Join(workDir, "blobs.txt")) //nolint:gosec // test blobs file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blobs), "beekeeper-linux-amd64.canary.json") || !strings.Contains(string(blobs), "beekeeper-darwin-arm64.canary.json") {
		t.Fatalf("unexpected blobs file: %s", blobs)
	}
}

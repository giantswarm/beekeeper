package loopworkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: test-only local git http-backend harness
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
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

// Finding 6 & Re-review Finding 3: Cleanup ownership matching is delimited and
// bound to (repo, runID, branch), leaving prefix-colliding runs (run-42 vs
// run-420) and copied markers untouched, while paginating through open PRs and
// deleting orphaned owned branches independently of PR existence.
func TestCleanupSandboxRunDelimitedOwnership(t *testing.T) {
	dir := t.TempDir()
	closedFile := filepath.Join(dir, "closed.txt")
	deletedBranchesFile := filepath.Join(dir, "deleted-branches.txt")
	ghStub := filepath.Join(dir, "gh")

	ownedBranch := canaryBranchName("run-42", "abcdef1234567890")
	orphanBranch := canaryBranchName("run-42", "9999999999999999")
	ownedMarker := canaryOwnershipMarker(testSandboxRepo, "run-42", ownedBranch)
	collidingBranch := canaryBranchName("run-420", "abcdef1234567890")
	collidingMarker := canaryOwnershipMarker(testSandboxRepo, "run-420", collidingBranch)

	// Emit two concatenated JSON pages as `gh api --paginate` does, then after
	// PR #11 is closed and orphanBranch is deleted, omit them on verification.
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$1" = "api" ] && [ "$2" = "--paginate" ]; then
  case "$3" in
    */pulls*)
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
      ;;
    */git/matching-refs/heads/*)
      if [ -f %q ]; then
        printf '[{"ref":"refs/heads/%s"},{"ref":"refs/heads/canary/run-42/not-cand"}]'
        exit 0
      fi
      printf '[{"ref":"refs/heads/%s"}][{"ref":"refs/heads/%s"},{"ref":"refs/heads/canary/run-42/not-cand"}]'
      exit 0
      ;;
  esac
fi
if [ "$1" = "pr" ] && [ "$2" = "close" ]; then
  echo "$*" >> %q
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "-X" ] && [ "$3" = "DELETE" ]; then
  echo "$4" >> %q
  exit 0
fi
exit 1
`, closedFile, collidingBranch, collidingMarker, ownedMarker,
		ownedBranch, ownedMarker, collidingBranch, collidingMarker,
		ownedMarker, ownedMarker,
		deletedBranchesFile, collidingBranch,
		orphanBranch, collidingBranch,
		closedFile, deletedBranchesFile)

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
	deletedBytes, err := os.ReadFile(deletedBranchesFile) //nolint:gosec // test output file
	if err != nil {
		t.Fatalf("expected orphan branch %s to be deleted: %v", orphanBranch, err)
	}
	gotBranchDel := strings.TrimSpace(string(deletedBytes))
	wantBranchDel := "repos/" + testSandboxRepo + "/git/refs/heads/" + orphanBranch
	if gotBranchDel != wantBranchDel {
		t.Fatalf("deleted branch endpoints = %q, want only %q", gotBranchDel, wantBranchDel)
	}
}

// sandboxHarness sets up a local bare git origin served over an authenticated
// HTTPS Git Smart HTTP server (git http-backend) and a stateful gh CLI stub so
// live sandbox canary runs exercise HTTPS Git authentication, worker start,
// independent reviewer dispatch, merge, and branch/PR cleanup without external
// network access.
type sandboxHarness struct {
	dir             string
	originPath      string
	statePath       string
	expectedToken   atomic.Value
	authUploadPack  atomic.Int32
	authReceivePack atomic.Int32
	unauthCount     atomic.Int32
}

func newSandboxHarness(t *testing.T, repo string) *sandboxHarness {
	t.Helper()
	dir := t.TempDir()
	originPath := filepath.Join(dir, filepath.FromSlash(repo)+".git")
	workPath := filepath.Join(dir, "seed")
	statePath := filepath.Join(dir, "prs.json")
	if err := os.MkdirAll(filepath.Dir(originPath), 0o700); err != nil {
		t.Fatal(err)
	}
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
	runGit(originPath, "config", "http.receivepack", "true")
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
	h.expectedToken.Store("sandbox-gh-token")
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

	// Serve origin.git over HTTPS via git http-backend, requiring Basic Auth
	// ("x-access-token:<expectedToken>") on every fetch and push request.
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantToken, _ := h.expectedToken.Load().(string)
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass == "" || pass != wantToken {
			h.unauthCount.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub Sandbox"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.RawQuery, "git-upload-pack") || strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			h.authUploadPack.Add(1)
		}
		if strings.Contains(r.URL.RawQuery, "git-receive-pack") || strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			h.authReceivePack.Add(1)
		}
		handler := &cgi.Handler{
			Path: gitBin,
			Args: []string{"http-backend"},
			Env: []string{
				"GIT_PROJECT_ROOT=" + h.dir,
				"GIT_HTTP_EXPORT_ALL=1",
				"REMOTE_USER=" + user,
			},
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(tlsServer.Close)

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: tlsServer.Certificate().Raw,
	})
	caPath := filepath.Join(h.dir, "ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	ghScript := fmt.Sprintf(`#!/usr/bin/env python3
import json, os, subprocess, sys

state_path = %q
origin_path = %q
repo = %q
git_bin = %q
fail_close = os.environ.get("SANDBOX_FAIL_CLOSE") == "1"
fail_pr_create = os.environ.get("SANDBOX_FAIL_PR_CREATE") == "1"
fail_branch_delete = os.environ.get("SANDBOX_FAIL_BRANCH_DELETE") == "1"

with open(state_path, "r") as f:
    prs = json.load(f)

def save():
    with open(state_path, "w") as f:
        json.dump(prs, f)

args = sys.argv[1:]
if args[:2] == ["auth", "token"]:
    print("sandbox-gh-token")
    sys.exit(0)

if args[:2] == ["api", "user"]:
    print("canary-bot")
    sys.exit(0)

if args[:2] == ["api", "--paginate"]:
    endpoint = args[2]
    if "/pulls" in endpoint:
        open_prs = []
        for p in prs:
            if p["state"] == "OPEN":
                open_prs.append({"number": p["number"], "head": {"ref": p["headRefName"]}, "body": p["body"]})
        print(json.dumps(open_prs))
        sys.exit(0)
    if "/git/matching-refs/heads/" in endpoint:
        prefix = endpoint.split("/git/matching-refs/heads/", 1)[1]
        out = subprocess.check_output(
            [git_bin, "--git-dir=" + origin_path, "for-each-ref", "--format=%%(refname)", "refs/heads/" + prefix],
            text=True,
        ).strip()
        refs = [{"ref": line} for line in out.splitlines() if line.strip()]
        print(json.dumps(refs))
        sys.exit(0)

if len(args) >= 4 and args[:3] == ["api", "-X", "DELETE"] and "/git/refs/heads/" in args[3]:
    if fail_branch_delete or os.path.exists(state_path + ".fail-branch-delete"):
        sys.stderr.write("simulated branch delete permission error\n")
        sys.exit(1)
    branch = args[3].split("/git/refs/heads/", 1)[1]
    subprocess.check_call([git_bin, "--git-dir=" + origin_path, "update-ref", "-d", "refs/heads/" + branch])
    sys.exit(0)

if args[:2] == ["pr", "create"]:
    if fail_pr_create or os.path.exists(state_path + ".fail-pr-create"):
        sys.stderr.write("simulated pr create failure after branch push\n")
        sys.exit(1)
    head = args[args.index("--head") + 1]
    body = args[args.index("--body") + 1]
    oid = subprocess.check_output([git_bin, "--git-dir=" + origin_path, "rev-parse", "refs/heads/" + head], text=True).strip()
    num = max(p["number"] for p in prs) + 1
    subprocess.check_call([git_bin, "--git-dir=" + origin_path, "update-ref", f"refs/pull/{num}/head", oid])
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
            if "--delete-branch" in args:
                subprocess.call([git_bin, "--git-dir=" + origin_path, "update-ref", "-d", "refs/heads/" + p["headRefName"]])
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
            if "--delete-branch" in args:
                subprocess.call([git_bin, "--git-dir=" + origin_path, "update-ref", "-d", "refs/heads/" + p["headRefName"]])
    save()
    sys.exit(0)

sys.stderr.write("unhandled gh args: " + " ".join(args) + "\n")
sys.exit(1)
`, h.statePath, h.originPath, repo, gitBin)

	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghScript), 0o700); err != nil { //nolint:gosec // test gh script
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":/usr/bin:/bin")
	t.Setenv("BEEKEEPER_CANARY_ORIGIN", tlsServer.URL+"/"+repo+".git")
	t.Setenv("BEEKEEPER_CANARY_SSL_CAINFO", caPath)
}

func (h *sandboxHarness) hasRemoteBranch(t *testing.T, branch string) bool {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	c := exec.CommandContext(t.Context(), gitBin, "--git-dir="+h.originPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) //nolint:gosec // test git check
	return c.Run() == nil
}

func serveTestCandidate() (int, bool) {
	mode := os.Getenv("TEST_CANDIDATE_MODE")
	if mode == "" {
		return 0, false
	}
	if os.Getenv("SECRET_SENTINEL") != "" {
		fmt.Fprintln(os.Stderr, "leaked SECRET_SENTINEL")
		return 1, true
	}
	if sentinelHome := os.Getenv("TEST_SENTINEL_HOME"); sentinelHome != "" && os.Getenv("HOME") == sentinelHome {
		fmt.Fprintln(os.Stderr, "inherited live HOME")
		return 1, true
	}
	if sentinelCfg := os.Getenv("TEST_SENTINEL_CFG"); sentinelCfg != "" && os.Getenv("BEEKEEPER_CONFIG") == sentinelCfg {
		fmt.Fprintln(os.Stderr, "inherited live BEEKEEPER_CONFIG")
		return 1, true
	}

	args := os.Args[1:]
	joined := strings.Join(args, " ")
	sandboxDir := os.Getenv("CANARY_SANDBOX_DIR")
	stateDir := filepath.Join(sandboxDir, "state")

	switch {
	case joined == "version":
		fmt.Println("beekeeper v1.2.3 (commit 0123456789abcdef0123456789abcdef01234567)")
		return 0, true
	case joined == "supervisor status":
		return 0, true
	case slices.Contains(args, "park") && slices.Contains(args, "--on"):
		idx := slices.Index(args, "--on")
		asIdx := slices.Index(args, "--as")
		if idx < 0 || idx+1 >= len(args) || asIdx < 0 || asIdx+1 >= len(args) {
			fmt.Fprintln(os.Stderr, "missing --on or --as")
			return 1, true
		}
		ref := args[idx+1]
		worker := args[asIdx+1]
		c := exec.CommandContext(context.Background(), "git", "rev-parse", "HEAD")
		out, err := c.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "git rev-parse HEAD: %v: %s\n", err, out)
			return 1, true
		}
		head := strings.TrimSpace(string(out))
		store, err := state.Open(stateDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		now := time.Now().UTC()
		if err := store.Update(func(st *state.State) ([]state.Event, error) {
			st.ReviewParks = append(st.ReviewParks, state.ReviewPark{
				On:         ref,
				At:         now,
				ReviewHead: head,
				Agent:      worker,
			})
			return []state.Event{{
				At:      now,
				By:      state.Party{Name: worker, Session: os.Getenv("CLAUDE_CODE_SESSION_ID")},
				Verb:    "agents.park",
				Subject: ref,
				Detail:  fmt.Sprintf("%s parked on %s at %s", worker, ref, head),
			}}, nil
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	case joined == "supervisor loop --once":
		store, err := state.Open(stateDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		st, err := store.Read()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		events, err := store.Events(0, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}

		// Pass 1: admit board work and start deterministic worker via claude --bg.
		if len(st.Starts) == 0 {
			if mode == "skip-worker" {
				return 0, true
			}
			workerName := filepath.Base(os.Getenv("CANARY_REPO")) + " 2"
			c := exec.CommandContext(context.Background(), "claude", "--bg", "-n", workerName, "--", "work") //nolint:gosec // test candidate invoking sandbox claude
			if out, cerr := c.CombinedOutput(); cerr != nil {
				fmt.Fprintf(os.Stderr, "claude --bg worker: %v: %s\n", cerr, out)
				return 1, true
			}
			sessionID := "a1b2c301-0000-0000-0000-000000000001"
			now := time.Now().UTC()
			if err := store.Update(func(s *state.State) ([]state.Event, error) {
				s.Starts = append(s.Starts, state.Start{
					Party:      state.Party{Name: workerName, Session: sessionID},
					At:         now,
					Background: true,
				})
				s.Agents = append(s.Agents, state.Agent{
					Party: state.Party{Name: workerName, Session: sessionID},
					Task:  "canary worker task",
				})
				return []state.Event{{
					At:     now,
					By:     state.Party{Name: partyLoop},
					Verb:   "agents.start",
					Detail: workerName + ": started deterministic worker",
				}}, nil
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1, true
			}
			return 0, true
		}

		if len(st.ReviewParks) == 0 {
			return 0, true
		}
		park := st.ReviewParks[len(st.ReviewParks)-1]
		hasReviewEvent := slices.ContainsFunc(events, func(e state.Event) bool {
			return e.Verb == "supervisor.review" && e.Subject == park.On && strings.Contains(e.Detail, park.ReviewHead)
		})

		// Pass 2: dispatch independent exact-head reviewer via claude --bg.
		if !hasReviewEvent {
			if mode == "skip-reviewer" {
				return 0, true
			}
			_, numStr, _ := strings.Cut(park.On, "#")
			revName := fmt.Sprintf("Review %s %s r1", filepath.Base(os.Getenv("CANARY_REPO")), numStr)
			c := exec.CommandContext(context.Background(), "claude", "--bg", "-n", revName, "--", "review") //nolint:gosec // test candidate invoking sandbox claude
			if out, cerr := c.CombinedOutput(); cerr != nil {
				fmt.Fprintf(os.Stderr, "claude --bg reviewer: %v: %s\n", cerr, out)
				return 1, true
			}
			revSession := "a1b2c302-0000-0000-0000-000000000001"
			now := time.Now().UTC()
			if err := store.Update(func(s *state.State) ([]state.Event, error) {
				s.Starts = append(s.Starts, state.Start{
					Party:      state.Party{Name: revName, Session: revSession},
					At:         now,
					Background: true,
				})
				s.Agents = append(s.Agents, state.Agent{
					Party: state.Party{Name: revName, Session: revSession},
					Task:  "canary review task",
				})
				return []state.Event{
					{
						At:     now,
						By:     state.Party{Name: partyLoop},
						Verb:   "agents.start",
						Detail: revName + ": started independent reviewer",
					},
					{
						At:      now,
						By:      state.Party{Name: partyLoop},
						Verb:    "supervisor.review",
						Subject: park.On,
						Detail:  fmt.Sprintf("[review %s@%s] %s", park.On, park.ReviewHead, revName),
					},
				}, nil
			}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1, true
			}
			return 0, true
		}

		// Pass 3+: validate and merge exact head.
		if mode == "no-merge" {
			return 0, true
		}
		hasMergeEvent := slices.ContainsFunc(events, func(e state.Event) bool {
			return e.Verb == "supervisor.merge" && e.Subject == park.On
		})
		if hasMergeEvent {
			return 0, true
		}
		repo, numStr, _ := strings.Cut(park.On, "#")
		c := exec.CommandContext(context.Background(), "gh", "pr", "merge", numStr, "--repo", repo, "--squash", "--delete-branch", "--match-head-commit", park.ReviewHead) //nolint:gosec // test candidate merge command
		if out, cerr := c.CombinedOutput(); cerr != nil {
			fmt.Fprintf(os.Stderr, "gh pr merge: %v: %s\n", cerr, out)
			return 1, true
		}
		now := time.Now().UTC()
		if err := store.Record(state.Event{
			At:      now,
			By:      state.Party{Name: partyLoop},
			Verb:    "supervisor.merge",
			Subject: park.On,
			Detail:  fmt.Sprintf("[merge %s@%s] merged", park.On, park.ReviewHead),
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	}
	return 0, true
}

func writeFakeCandidate(t *testing.T, dir, name, mode, sentinelHome, sentinelCfg string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	candPath := filepath.Join(dir, name)
	script := fmt.Sprintf(`#!/bin/sh
set -eu
export TEST_CANDIDATE_MODE=%q
export TEST_SENTINEL_HOME=%q
export TEST_SENTINEL_CFG=%q
exec %q "$@"
`, mode, sentinelHome, sentinelCfg, exe)

	if err := os.WriteFile(candPath, []byte(script), 0o700); err != nil { //nolint:gosec // test candidate executable
		t.Fatal(err)
	}
	return candPath
}

// Findings 1, 2, 3, & 7 + Re-review Findings 1, 2, & 3:
// Live sandbox canary authenticates Git HTTPS via an isolated credential helper,
// drives worker start and a separately dispatched exact-head reviewer through
// the candidate's paths, rejects candidates that skip worker start or reviewer
// dispatch or merge, cleans up owned branches when PR creation fails after push
// or after an interrupted run, and fails closed if exit cleanup fails.
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
	goodCand := writeFakeCandidate(t, dir, "beekeeper-linux-amd64", "full", sentinelHome, sentinelCfg)
	goodBundle := writeBundle(t, goodCand)

	// 1. Fresh run authenticates Git HTTPS fetch & push via the isolated helper
	// and completes worker start -> PR park -> independent reviewer dispatch ->
	// exact-head review -> loop merge -> observed merge commit.
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
	if h.authUploadPack.Load() < 1 || h.authReceivePack.Load() < 1 {
		t.Fatalf("expected authenticated HTTPS git fetch and push, got uploadPack=%d receivePack=%d", h.authUploadPack.Load(), h.authReceivePack.Load())
	}

	// 2. HTTPS Git authentication boundary (Re-review Finding 1): if the isolated
	// token is rejected by the HTTPS Git origin, StageCanary fails closed.
	h.expectedToken.Store("different-token-that-rejects-sandbox-gh-token")
	authFailEv, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: goodCand,
		Bundle:    goodBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "run-https-auth-fail",
	})
	h.expectedToken.Store("sandbox-gh-token")
	if err == nil || authFailEv.Status != update.CanaryStatusFailed || authFailEv.MergeCommit != "" {
		t.Fatalf("expected HTTPS git auth failure to fail StageCanary: ev=%+v err=%v", authFailEv, err)
	}

	// 3. Candidate that skips worker start or independent reviewer dispatch is
	// rejected even when its loop command exits 0 (Re-review Finding 2).
	for _, tc := range []struct {
		mode    string
		wantSub string
	}{
		{mode: "skip-worker", wantSub: "did not start deterministic worker"},
		{mode: "skip-reviewer", wantSub: "did not dispatch independent reviewer"},
		{mode: "no-merge", wantSub: "candidate state lacks merge event"},
	} {
		cand := writeFakeCandidate(t, dir, "beekeeper-"+tc.mode, tc.mode, sentinelHome, sentinelCfg)
		bundle := writeBundle(t, cand)
		failedEv, cerr := StageCanary(t.Context(), CanaryConfig{
			Candidate: cand,
			Bundle:    bundle,
			Repo:      testSandboxRepo,
			AllowOrg:  testOrg,
			Project:   7,
			Run:       "run-fresh",
		})
		if cerr == nil || !strings.Contains(cerr.Error(), tc.wantSub) || failedEv.Status != update.CanaryStatusFailed || failedEv.MergeCommit != "" {
			t.Fatalf("mode %s: got ev=%+v err=%v, want failure containing %q", tc.mode, failedEv, cerr, tc.wantSub)
		}
	}

	// 4. Push-success / PR-create-failure branch cleanup and same-run retry
	// (Re-review Finding 3): when git push succeeds and gh pr create fails, no PR
	// exists, yet exit cleanup deletes the owned remote branch so a retry of the
	// same runID succeeds without branch collision.
	candBytes, err := os.ReadFile(goodCand) //nolint:gosec // test candidate
	if err != nil {
		t.Fatal(err)
	}
	candSum := sha256.Sum256(candBytes)
	candDigest := hex.EncodeToString(candSum[:])
	retryRunID := "run-pr-create-fail"
	expectedBranch := canaryBranchName(retryRunID, candDigest)

	failCreateMarker := h.statePath + ".fail-pr-create"
	if err := os.WriteFile(failCreateMarker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	prCreateFailEv, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: goodCand,
		Bundle:    goodBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       retryRunID,
	})
	_ = os.Remove(failCreateMarker)
	if err == nil || !strings.Contains(err.Error(), "creating sandbox canary PR") || prCreateFailEv.Status != update.CanaryStatusFailed {
		t.Fatalf("expected PR create failure: ev=%+v err=%v", prCreateFailEv, err)
	}
	if h.hasRemoteBranch(t, expectedBranch) {
		t.Fatalf("owned branch %s remained on remote after PR creation failure", expectedBranch)
	}

	// Simulate an interrupted run that left an owned branch on origin (plus a
	// prefix-colliding branch for run-pr-create-fail0), then retry retryRunID:
	// entry cleanup removes the leftover owned branch, leaves the colliding
	// branch intact, and the retry succeeds.
	collidingRunBranch := canaryBranchName(retryRunID+"0", candDigest)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{expectedBranch, collidingRunBranch} {
		c := exec.CommandContext(t.Context(), gitBin, "--git-dir="+h.originPath, "branch", b, "main") //nolint:gosec // test seed branch
		if out, berr := c.CombinedOutput(); berr != nil {
			t.Fatalf("seeding branch %s: %v: %s", b, berr, out)
		}
	}
	retryEv, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: goodCand,
		Bundle:    goodBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       retryRunID,
	})
	if err != nil || retryEv.Status != update.CanaryStatusSuccess {
		t.Fatalf("same-run retry after interrupted branch failed: ev=%+v err=%v", retryEv, err)
	}
	if h.hasRemoteBranch(t, expectedBranch) {
		t.Fatalf("owned branch %s remained after successful retry cleanup", expectedBranch)
	}
	if !h.hasRemoteBranch(t, collidingRunBranch) {
		t.Fatalf("prefix-colliding branch %s was wrongly deleted", collidingRunBranch)
	}

	// 5. When exit cleanup fails even after a successful merge (Finding 7),
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

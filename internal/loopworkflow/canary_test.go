package loopworkflow

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	testOrg           = "giantswarm"
	testSandboxRepo   = "giantswarm/beekeeper-Stage-Canary"
	gitUploadPackSvc  = "git-upload-pack"
	gitReceivePackSvc = "git-receive-pack"
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

// Finding 6 & Re-review Findings 1 & 3: Cleanup requires explicit sandbox
// credentials, ownership matching is delimited and bound to (repo, runID,
// branch) and head.repo (non-fork matching repository), leaving prefix-colliding
// runs (run-42 vs run-420), fork PRs, cross-repo PRs, null-repo PRs, non-hex
// cand- branches, and copied markers untouched, while paginating through open
// PRs and deleting orphaned owned branches independently of PR existence.
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
	nonHexBranch := "canary/run-42/cand-nothexzzzzzz"
	nonHexMarker := canaryOwnershipMarker(testSandboxRepo, "run-42", nonHexBranch)

	// Emit two concatenated JSON pages as `gh api --paginate` does, then after
	// PR #11 is closed and orphanBranch is deleted, omit them on verification.
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ -z "${GH_TOKEN:-}" ]; then
  echo "missing GH_TOKEN" >&2
  exit 1
fi
if [ "$1" = "api" ] && [ "$2" = "--paginate" ]; then
  case "$3" in
    */pulls*)
      if [ -f %q ]; then
        printf '[
          {"number":12,"head":{"ref":%q,"repo":{"full_name":%q,"fork":false}},"body":%q},
          {"number":13,"head":{"ref":"feat/unrelated","repo":{"full_name":%q,"fork":false}},"body":%q},
          {"number":14,"head":{"ref":%q,"repo":{"full_name":%q,"fork":false}},"body":%q},
          {"number":15,"head":{"ref":%q,"repo":{"full_name":"attacker/beekeeper-Stage-Canary","fork":true}},"body":%q},
          {"number":16,"head":{"ref":%q,"repo":{"full_name":"giantswarm/other-repo","fork":false}},"body":%q},
          {"number":17,"head":{"ref":%q,"repo":null},"body":%q}
        ]'
        exit 0
      fi
      printf '[
        {"number":11,"head":{"ref":%q,"repo":{"full_name":%q,"fork":false}},"body":%q},
        {"number":12,"head":{"ref":%q,"repo":{"full_name":%q,"fork":false}},"body":%q}
      ][
        {"number":13,"head":{"ref":"feat/unrelated","repo":{"full_name":%q,"fork":false}},"body":%q},
        {"number":14,"head":{"ref":%q,"repo":{"full_name":%q,"fork":false}},"body":%q},
        {"number":15,"head":{"ref":%q,"repo":{"full_name":"attacker/beekeeper-Stage-Canary","fork":true}},"body":%q},
        {"number":16,"head":{"ref":%q,"repo":{"full_name":"giantswarm/other-repo","fork":false}},"body":%q},
        {"number":17,"head":{"ref":%q,"repo":null},"body":%q}
      ]'
      exit 0
      ;;
    */git/matching-refs/heads/*)
      if [ -f %q ]; then
        printf '[{"ref":"refs/heads/%s"},{"ref":"refs/heads/%s"},{"ref":"refs/heads/canary/run-42/not-cand"}]'
        exit 0
      fi
      printf '[{"ref":"refs/heads/%s"}][{"ref":"refs/heads/%s"},{"ref":"refs/heads/%s"},{"ref":"refs/heads/canary/run-42/not-cand"}]'
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
`, closedFile,
		collidingBranch, testSandboxRepo, collidingMarker,
		testSandboxRepo, ownedMarker,
		nonHexBranch, testSandboxRepo, nonHexMarker,
		ownedBranch, ownedMarker,
		ownedBranch, ownedMarker,
		ownedBranch, ownedMarker,
		ownedBranch, testSandboxRepo, ownedMarker,
		collidingBranch, testSandboxRepo, collidingMarker,
		testSandboxRepo, ownedMarker,
		nonHexBranch, testSandboxRepo, nonHexMarker,
		ownedBranch, ownedMarker,
		ownedBranch, ownedMarker,
		ownedBranch, ownedMarker,
		deletedBranchesFile, collidingBranch, nonHexBranch,
		orphanBranch, collidingBranch, nonHexBranch,
		closedFile, deletedBranchesFile)

	if err := os.WriteFile(ghStub, []byte(script), 0o700); err != nil { //nolint:gosec // test stub executable
		t.Fatal(err)
	}

	// Missing BEEKEEPER_CANARY_GITHUB_TOKEN fails closed without invoking gh.
	t.Setenv("BEEKEEPER_CANARY_GITHUB_TOKEN", "")
	if err := CleanupSandboxRun(t.Context(), ghStub, testSandboxRepo, testOrg, "run-42"); err == nil || !strings.Contains(err.Error(), "requires explicit BEEKEEPER_CANARY_GITHUB_TOKEN") {
		t.Fatalf("expected missing token error, got: %v", err)
	}

	t.Setenv("BEEKEEPER_CANARY_GITHUB_TOKEN", "sandbox-cleanup-token")
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
			numberField:   1,
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
			numberField:   2,
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

	// Serve origin.git over Git Smart HTTPS (upload-pack / receive-pack
	// stateless-rpc), requiring Basic Auth ("x-access-token:<expectedToken>")
	// on every fetch and push request without relying on optional git-daemon CGI.
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
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			var svc, subcmd, pkt string
			switch r.URL.Query().Get("service") {
			case gitUploadPackSvc:
				svc, subcmd, pkt = gitUploadPackSvc, "upload-pack", "001e# service=git-upload-pack\n0000"
			case gitReceivePackSvc:
				svc, subcmd, pkt = gitReceivePackSvc, "receive-pack", "001f# service=git-receive-pack\n0000"
			default:
				http.Error(w, "unsupported git service", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = io.WriteString(w, pkt)
			c := exec.CommandContext(r.Context(), gitBin, subcmd, "--stateless-rpc", "--advertise-refs", h.originPath) //nolint:gosec // test git smart-http ref advertisement
			c.Stdout = w
			c.Stderr = os.Stderr
			_ = c.Run()
		case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/git-upload-pack") || strings.HasSuffix(r.URL.Path, "/git-receive-pack")):
			svc := gitUploadPackSvc
			if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
				svc = gitReceivePackSvc
			}
			subcmd := strings.TrimPrefix(svc, "git-")
			body := io.Reader(r.Body)
			if r.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				defer func() { _ = gz.Close() }()
				body = gz
			}
			w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", svc))
			w.Header().Set("Cache-Control", "no-cache")
			c := exec.CommandContext(r.Context(), gitBin, subcmd, "--stateless-rpc", h.originPath) //nolint:gosec // test git smart-http stateless RPC
			c.Stdin = body
			c.Stdout = w
			c.Stderr = os.Stderr
			_ = c.Run()
		default:
			http.NotFound(w, r)
		}
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
import datetime, json, os, re, subprocess, sys

state_path = %q
origin_path = %q
repo = %q
git_bin = %q
fail_close = os.environ.get("SANDBOX_FAIL_CLOSE") == "1"
fail_pr_create = os.environ.get("SANDBOX_FAIL_PR_CREATE") == "1"
fail_branch_delete = os.environ.get("SANDBOX_FAIL_BRANCH_DELETE") == "1"

if os.environ.get("SECRET_SENTINEL"):
    sys.stderr.write("leaked SECRET_SENTINEL into gh\n")
    sys.exit(1)
sentinel_home = os.environ.get("TEST_SENTINEL_HOME")
if sentinel_home and os.environ.get("HOME") == sentinel_home:
    sys.stderr.write("inherited live HOME in gh\n")
    sys.exit(1)
sentinel_cfg = os.environ.get("TEST_SENTINEL_CFG")
if sentinel_cfg and os.environ.get("BEEKEEPER_CONFIG") == sentinel_cfg:
    sys.stderr.write("inherited live BEEKEEPER_CONFIG in gh\n")
    sys.exit(1)

with open(state_path, "r") as f:
    prs = json.load(f)

def save():
    with open(state_path, "w") as f:
        json.dump(prs, f)

def now_iso(offset_sec=0):
    dt = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=offset_sec)
    return dt.strftime("%%Y-%%m-%%dT%%H:%%M:%%SZ")

args = sys.argv[1:]
if args[:2] == ["auth", "token"]:
    with open(state_path + ".gh-auth-token-called", "w") as f:
        f.write("1")
    sys.stderr.write("unexpected call to gh auth token\n")
    sys.exit(1)

if not os.environ.get("GH_TOKEN"):
    sys.stderr.write("missing GH_TOKEN in gh invocation\n")
    sys.exit(1)

if args[:2] == ["api", "user"]:
    if "--jq" in args:
        print("canary-bot")
    else:
        print(json.dumps({"login": "canary-bot"}))
    sys.exit(0)

if args[:2] == ["search", "prs"]:
    print("[]")
    sys.exit(0)

if args[:2] == ["api", "--include"] and len(args) >= 3 and args[2] == "repos/" + repo + "/pulls?state=all&sort=updated&direction=desc&per_page=1":
    sys.stdout.write("HTTP/2.0 200 OK\r\nETag: fixture\r\nContent-Type: application/json\r\n\r\n[]\n")
    sys.exit(0)

if args[:2] == ["api", "-i"]:
    sys.stdout.write("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n[]\n")
    sys.exit(0)

if args[:2] == ["api", "--paginate"]:
    endpoint = args[2]
    if "/pulls" in endpoint and "/reviews" not in endpoint and "/comments" not in endpoint:
        open_prs = []
        for p in prs:
            if p["state"] == "OPEN":
                head_repo = p.get("headRepo", {"full_name": repo, "fork": False})
                open_prs.append({"number": p["number"], "head": {"ref": p["headRefName"], "repo": head_repo}, "body": p["body"]})
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
    if "/reviews" in endpoint or "/comments" in endpoint:
        print("[]")
        sys.exit(0)

if len(args) >= 4 and args[:3] == ["api", "-X", "DELETE"] and "/git/refs/heads/" in args[3]:
    if fail_branch_delete or os.path.exists(state_path + ".fail-branch-delete"):
        sys.stderr.write("simulated branch delete permission error\n")
        sys.exit(1)
    branch = args[3].split("/git/refs/heads/", 1)[1]
    subprocess.check_call([git_bin, "--git-dir=" + origin_path, "update-ref", "-d", "refs/heads/" + branch])
    sys.exit(0)

if args[:2] == ["api", "graphql"]:
    joined_args = " ".join(args)
    if "pullRequests(states:OPEN," in joined_args:
        print(json.dumps({"data": {"repository": {"pullRequests": {"nodes": [], "pageInfo": {"hasNextPage": False}}}}}))
        sys.exit(0)
    if "fields(first:50)" in joined_args:
        project_obj = {
            "id": "PVT_canary",
            "fields": {"nodes": [{
                "id": "PVTF_status",
                "name": "Status",
                "options": [
                    {"id": "OPT_todo", "name": "Todo"},
                    {"id": "OPT_in_progress", "name": "In Progress"},
                    {"id": "OPT_done", "name": "Done"},
                ],
            }]},
        }
        print(json.dumps({"data": {
            "organization": {"projectV2": project_obj},
            "repositoryOwner": {"projectV2": project_obj},
        }}))
        sys.exit(0)
    if "items(first:" in joined_args:
        if os.path.exists(state_path + ".empty-board"):
            empty_items = {"pageInfo": {"hasNextPage": False, "endCursor": ""}, "nodes": []}
            print(json.dumps({"data": {
                "organization": {"projectV2": {"items": empty_items}},
                "repositoryOwner": {"projectV2": {"items": empty_items}},
            }}))
            sys.exit(0)
        nodes = [{
            "id": "PVTI_1",
            "fieldValueByName": {"name": "Todo", "optionId": "OPT_todo"},
            "status": {"name": "Todo", "updatedAt": "2026-01-01T00:00:00Z"},
            "content": {
                "typename": "Issue",
                "id": "I_1",
                "number": 1,
                "title": "Canary sandbox validation issue",
                "state": "OPEN",
                "createdAt": "2026-01-01T00:00:00Z",
                "updatedAt": "2026-01-01T00:00:00Z",
                "url": f"https://github.com/{repo}/issues/1",
                "repository": {"nameWithOwner": repo},
                "labels": {"nodes": [], "pageInfo": {"hasNextPage": False, "endCursor": ""}},
                "assignees": {"nodes": []},
                "SubIssues": {"nodes": []},
                "subIssuesSummary": {"total": 0, "completed": 0},
                "blockedBy": {"totalCount": 0, "nodes": []},
                "parent": None,
            },
        }]
        items_obj = {"pageInfo": {"hasNextPage": False, "endCursor": ""}, "nodes": nodes}
        print(json.dumps({"data": {
            "organization": {"projectV2": {"items": items_obj}},
            "repositoryOwner": {"projectV2": {"items": items_obj}},
        }}))
        sys.exit(0)
    if "projectItems(first:50)" in joined_args:
        owner_login = repo.split("/")[0]
        print(json.dumps({"data": {"repository": {"issue": {
            "id": "I_1",
            "body": "Canary sandbox validation issue",
            "parent": None,
            "projectItems": {"nodes": [{
                "id": "PVTI_1",
                "isArchived": False,
                "project": {"id": "PVT_canary", "number": 7, "owner": {"login": owner_login}},
                "status": {"name": "Todo", "updatedAt": "2026-01-01T00:00:00Z"},
            }]},
        }}}}))
        sys.exit(0)
    if "updateProjectV2ItemFieldValue" in joined_args:
        print(json.dumps({"data": {"updateProjectV2ItemFieldValue": {"projectV2Item": {"id": "PVTI_1"}}}}))
        sys.exit(0)
    if "issueOrPullRequest(number:" in joined_args:
        data = {}
        for m in re.finditer(r'(i\d+|r\d+):repository\([^)]*\)\{issueOrPullRequest\(number:(\d+)\)', joined_args):
            alias, num_s = m.group(1), int(m.group(2))
            state = "OPEN"
            typename = "Issue"
            for p in prs:
                if p["number"] == num_s:
                    state = p["state"]
                    typename = "PullRequest"
            data[alias] = {
                "issueOrPullRequest": {
                    "__typename": typename,
                    "state": state,
                    "closingIssuesReferences": {"nodes": []},
                    "timelineItems": {"nodes": []},
                    "author": {"login": "canary-bot"},
                    "reviews": {"nodes": []},
                    "comments": {"nodes": []},
                },
            }
        if not data:
            data["repository"] = {"issueOrPullRequest": {"__typename": "Issue", "state": "OPEN"}}
        print(json.dumps({"data": data}))
        sys.exit(0)
    if "mergeQueue(" in joined_args:
        if os.path.exists(state_path + ".unreadable-queue"):
            sys.stderr.write("simulated unreadable merge queue\n")
            sys.exit(1)
        print(json.dumps({"data": {"repository": {"mergeQueue": None}}}))
        sys.exit(0)
    if "issue(number:" in joined_args:
        print(json.dumps({"data": {"repository": {"issue": {"parent": None}}}}))
        sys.exit(0)
    print(json.dumps({"data": {}}))
    sys.exit(0)

if args[0] == "api" and len(args) >= 2:
    ep = args[1]
    if ep.startswith("repos/" + repo + "/pulls/"):
        num = int(ep.rsplit("/", 1)[1])
        for p in prs:
            if p["number"] == num:
                head_repo = p.get("headRepo", {"full_name": repo, "fork": False})
                print(json.dumps({
                    "head": {"sha": p["headRefOid"], "ref": p["headRefName"], "repo": head_repo},
                    "user": {"login": "canary-bot"},
                }))
                sys.exit(0)
        sys.exit(1)
    if "/commits/" in ep and "--jq" in args:
        print("2026-01-01T00:00:00Z")
        sys.exit(0)
    if "/compare/" in ep and "--jq" in args:
        print("0")
        sys.exit(0)
    if "/rules/branches/" in ep:
        if os.path.exists(state_path + ".unreadable-queue"):
            sys.exit(1)
        print("[]")
        sys.exit(0)
    if ep == "repos/" + repo:
        if os.path.exists(state_path + ".unreadable-queue"):
            sys.exit(1)
        if "--jq" in args and ".default_branch" in args:
            print("main")
        else:
            print(json.dumps({"default_branch": "main", "allow_squash_merge": True, "owner": {"type": "Organization"}}))
        sys.exit(0)

if args[:2] == ["pr", "list"]:
    print("[]")
    sys.exit(0)

if args[:2] == ["pr", "create"]:
    if fail_pr_create or os.path.exists(state_path + ".fail-pr-create"):
        sys.stderr.write("simulated pr create failure after branch push\n")
        sys.exit(1)
    head = args[args.index("--head") + 1]
    body = args[args.index("--body") + 1]
    title = args[args.index("--title") + 1] if "--title" in args else "chore(canary): stage"
    oid = subprocess.check_output([git_bin, "--git-dir=" + origin_path, "rev-parse", "refs/heads/" + head], text=True).strip()
    num = max(p["number"] for p in prs) + 1
    subprocess.check_call([git_bin, "--git-dir=" + origin_path, "update-ref", f"refs/pull/{num}/head", oid])
    is_draft = os.path.exists(state_path + ".draft-pr")
    rollup = (
        [{"name": "test", "status": "IN_PROGRESS", "conclusion": ""}]
        if is_draft
        else [{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}]
    )
    prs.append({
        "number": num,
        "url": f"https://github.com/{repo}/pull/{num}",
        "state": "OPEN",
        "isDraft": is_draft,
        "mergeable": "MERGEABLE",
        "mergeStateStatus": "CLEAN",
        "baseRefName": "main",
        "headRefName": head,
        "headRefOid": oid,
        "headRepository": {"name": repo.split("/")[1]},
        "headRepositoryOwner": {"login": repo.split("/")[0]},
        "headRepo": {"full_name": repo, "fork": False},
        "title": title,
        "body": body,
        "author": {"login": "canary-bot"},
        "commits": [{"oid": oid, "parents": [{"oid": "0000000000000000000000000000000000000000"}]}],
        "files": [{"path": ".beekeeper-canary"}],
        "mergeCommit": {"oid": ""},
        "reviews": [],
        "comments": [],
        "statusCheckRollup": rollup,
    })
    save()
    print(f"https://github.com/{repo}/pull/{num}")
    sys.exit(0)

if args[:2] == ["pr", "review"]:
    num = int(args[2])
    body = args[args.index("--body") + 1]
    for p in prs:
        if p["number"] == num:
            p["reviews"].append({
                "id": f"PRR_{num}_{len(p['reviews']) + 1}",
                "state": "COMMENTED",
                "body": body,
                "submittedAt": now_iso(2),
                "author": {"login": "canary-bot"},
                "authorAssociation": "MEMBER",
                "commit": {"oid": p["headRefOid"]},
            })
    save()
    sys.exit(0)

if args[:2] == ["pr", "merge"]:
    num = int(args[2])
    match_oid = args[args.index("--match-head-commit") + 1]
    for p in prs:
        if p["number"] == num:
            if p["headRefOid"] != match_oid:
                sys.exit(1)
            tree_oid = subprocess.check_output(
                [git_bin, "--git-dir=" + origin_path, "rev-parse", match_oid + "^{tree}"],
                text=True,
            ).strip()
            main_oid = subprocess.check_output(
                [git_bin, "--git-dir=" + origin_path, "rev-parse", "refs/heads/main"],
                text=True,
            ).strip()
            env = dict(os.environ)
            env.update({
                "GIT_AUTHOR_NAME": "Canary Merger",
                "GIT_AUTHOR_EMAIL": "canary@example.invalid",
                "GIT_COMMITTER_NAME": "Canary Merger",
                "GIT_COMMITTER_EMAIL": "canary@example.invalid",
            })
            merge_oid = subprocess.check_output(
                [git_bin, "--git-dir=" + origin_path, "commit-tree", tree_oid, "-p", main_oid, "-m", f"Merge PR #{num}"],
                text=True,
                env=env,
            ).strip()
            subprocess.check_call([git_bin, "--git-dir=" + origin_path, "update-ref", "refs/heads/main", merge_oid])
            p["state"] = "MERGED"
            p["mergeCommit"] = {"oid": merge_oid}
            if "--delete-branch" in args:
                subprocess.call([git_bin, "--git-dir=" + origin_path, "update-ref", "-d", "refs/heads/" + p["headRefName"]])
            if os.path.exists(state_path + ".leave-open"):
                prs.append({
                    "number": num + 100,
                    "state": "OPEN",
                    "headRefName": p["headRefName"],
                    "headRefOid": p["headRefOid"],
                    "headRepo": {"full_name": repo, "fork": False},
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
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
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

var (
	compiledCandidateOnce  sync.Once
	compiledCandidateBytes []byte
	compiledCandidateErr   error
)

// buildActualCandidate compiles the real beekeeper binary from the repository
// root with release version/gitSHA ldflags so live canary tests exercise the
// actual candidate supervisor, worker, park, reviewer, and merge code paths.
func buildActualCandidate(t *testing.T, dir, name string) string {
	t.Helper()
	compiledCandidateOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "beekeeper-canary-cand-*")
		if err != nil {
			compiledCandidateErr = err
			return
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()
		outBin := filepath.Join(tmpDir, "beekeeper")
		goBin, err := exec.LookPath("go")
		if err != nil {
			compiledCandidateErr = err
			return
		}
		ldflags := "-X github.com/giantswarm/beekeeper/pkg/project.version=v1.2.3 -X github.com/giantswarm/beekeeper/pkg/project.gitSHA=0123456789abcdef0123456789abcdef01234567"
		c := exec.CommandContext(context.Background(), goBin, "build", "-ldflags", ldflags, "-o", outBin, "../..") //nolint:gosec // test compilation of actual candidate binary
		if out, berr := c.CombinedOutput(); berr != nil {
			compiledCandidateErr = fmt.Errorf("go build candidate: %w: %s", berr, out)
			return
		}
		compiledCandidateBytes, compiledCandidateErr = os.ReadFile(outBin) //nolint:gosec // reading compiled test candidate
	})
	if compiledCandidateErr != nil {
		t.Fatal(compiledCandidateErr)
	}
	candPath := filepath.Join(dir, name)
	if err := os.WriteFile(candPath, compiledCandidateBytes, 0o700); err != nil { //nolint:gosec // writing compiled test candidate
		t.Fatal(err)
	}
	return candPath
}

// Findings 1, 2, 3, & 7 + Re-review Findings 1, 2, & 3:
// Live sandbox canary requires explicit BEEKEEPER_CANARY_GITHUB_TOKEN without
// falling back to host gh auth token, authenticates Git HTTPS via an isolated
// credential helper, drives the actual compiled candidate binary through worker
// admission/start, PR park, independent exact-head reviewer dispatch, review,
// and merge, fails closed when admission/review/merge boundaries do not
// advance, cleans up owned branches when PR creation fails after push or after
// an interrupted run, and fails closed if exit cleanup fails.
func TestLiveSandboxCanaryFreshWorkflowIsolationAndCleanupGate(t *testing.T) {
	if testing.Short() {
		t.Skip("candidate build and live workflow canary run in full CI")
	}
	acceptTestBundles(t)
	dir := t.TempDir()
	goodCand := buildActualCandidate(t, dir, "beekeeper-linux-amd64")
	goodBundle := writeBundle(t, goodCand)

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
	t.Setenv("TEST_SENTINEL_HOME", sentinelHome)
	t.Setenv("TEST_SENTINEL_CFG", sentinelCfg)

	// 0. Missing BEEKEEPER_CANARY_GITHUB_TOKEN fails closed without invoking
	// host `gh auth token` (R3 Finding 1).
	t.Setenv("BEEKEEPER_CANARY_GITHUB_TOKEN", "")
	noTokenEv, err := StageCanary(t.Context(), CanaryConfig{
		Candidate: goodCand,
		Bundle:    goodBundle,
		Repo:      testSandboxRepo,
		AllowOrg:  testOrg,
		Project:   7,
		Run:       "run-no-token",
	})
	if err == nil || !strings.Contains(err.Error(), "requires explicit BEEKEEPER_CANARY_GITHUB_TOKEN") || noTokenEv.Status != update.CanaryStatusFailed {
		t.Fatalf("missing BEEKEEPER_CANARY_GITHUB_TOKEN: got ev=%+v err=%v", noTokenEv, err)
	}
	if _, statErr := os.Stat(h.statePath + ".gh-auth-token-called"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("StageCanary invoked host `gh auth token` fallback")
	}

	t.Setenv("BEEKEEPER_CANARY_GITHUB_TOKEN", "sandbox-gh-token")

	// 1. Fresh run executes the actual compiled candidate binary, authenticates
	// Git HTTPS fetch & push via the isolated helper, and completes worker
	// start -> PR park -> independent reviewer dispatch -> exact-head review ->
	// loop merge -> observed merge commit on origin/main.
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
	if ev.Status != update.CanaryStatusSuccess || ev.MergeCommit == "" || ev.MergeCommit == "2222222222222222222222222222222222222222" {
		t.Fatalf("unexpected fresh evidence (should not use historical 2222...): %+v", ev)
	}
	if h.authUploadPack.Load() < 1 || h.authReceivePack.Load() < 1 {
		t.Fatalf("expected authenticated HTTPS git fetch and push, got uploadPack=%d receivePack=%d", h.authUploadPack.Load(), h.authReceivePack.Load())
	}
	if _, statErr := os.Stat(h.statePath + ".gh-auth-token-called"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("StageCanary invoked `gh auth token` during live run")
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

	// 3. Lifecycle boundary failures against the actual compiled candidate binary
	// when board admission, reviewer dispatch, or merge validation cannot advance
	// (R3 Finding 2).
	for _, tc := range []struct {
		marker  string
		runID   string
		wantSub string
	}{
		{marker: ".empty-board", runID: "run-empty-board", wantSub: "did not start deterministic worker"},
		{marker: ".draft-pr", runID: "run-draft-pr", wantSub: "did not dispatch independent reviewer"},
		{marker: ".unreadable-queue", runID: "run-unreadable-queue", wantSub: "candidate state lacks merge event"},
	} {
		markerPath := h.statePath + tc.marker
		if err := os.WriteFile(markerPath, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		failedEv, cerr := StageCanary(t.Context(), CanaryConfig{
			Candidate: goodCand,
			Bundle:    goodBundle,
			Repo:      testSandboxRepo,
			AllowOrg:  testOrg,
			Project:   7,
			Run:       tc.runID,
		})
		_ = os.Remove(markerPath)
		if cerr == nil || !strings.Contains(cerr.Error(), tc.wantSub) || failedEv.Status != update.CanaryStatusFailed || failedEv.MergeCommit != "" {
			t.Fatalf("boundary %s: got ev=%+v err=%v, want failure containing %q", tc.marker, failedEv, cerr, tc.wantSub)
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

	// The temporary policy skips publication without provisioning or fake evidence.
	policyDir := filepath.Join(workDir, "internal", "update")
	if err := os.MkdirAll(policyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(policyDir, "sandbox-proof-required.txt")
	if err := os.WriteFile(policyPath, []byte("false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, cerr := runPublisher(); cerr != nil || !strings.Contains(out, "temporarily disabled") {
		t.Fatalf("disabled policy: err=%v out=%s", cerr, out)
	}
	if _, err := os.Stat(filepath.Join(workDir, "blobs.txt")); !os.IsNotExist(err) {
		t.Fatalf("disabled publisher created evidence blobs: %v", err)
	}
	if err := os.WriteFile(policyPath, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
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
		"BEEKEEPER_CANARY_GITHUB_TOKEN=sandbox-token",
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

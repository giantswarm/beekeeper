package loopworkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/update"
)

func TestStageCanaryRefusesUnsafeConfigurations(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	staged := filepath.Join(dir, "beekeeper-linux-amd64")
	const fakeScript = "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then\n  echo '{\"version\":\"0.99.0-rc.1\",\"gitCommit\":\"0123456789abcdef0123456789abcdef01234567\"}'\n  exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(staged, []byte(fakeScript), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cfg  CanaryConfig
		want string
	}{
		{
			name: "missing candidate",
			cfg:  CanaryConfig{Recorded: true},
			want: "staged candidate binary path is required",
		},
		{
			name: "live executable",
			cfg:  CanaryConfig{Candidate: exe, Recorded: true},
			want: "canary refuses to run against the live installed executable",
		},
		{
			name: "missing sandbox config without recorded",
			cfg:  CanaryConfig{Candidate: staged},
			want: "sandbox canary requires explicit --canary-repo, --canary-allow-org and --canary-project",
		},
		{
			name: "outside allowlisted org",
			cfg:  CanaryConfig{Candidate: staged, Repo: "other-org/sandbox", AllowOrg: "giantswarm-sandbox", Project: 1},
			want: "outside allowlisted organization",
		},
		{
			name: "production repository refused",
			cfg:  CanaryConfig{Candidate: staged, Repo: update.Repository, AllowOrg: "giantswarm", Project: 1},
			want: "must not be the production release repository",
		},
		{
			name: "recorded combined with live sandbox repo",
			cfg:  CanaryConfig{Candidate: staged, Recorded: true, Repo: "giantswarm-sandbox/canary"},
			want: "--canary-recorded cannot be combined with live sandbox repository flags",
		},
		{
			name: "tampered bundle",
			cfg: func() CanaryConfig {
				bundlePath := filepath.Join(dir, "tampered.bundle")
				_ = os.WriteFile(bundlePath, []byte("{}"), 0o600)
				return CanaryConfig{Candidate: staged, Bundle: bundlePath, Recorded: true}
			}(),
			want: "verifying candidate bundle",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := StageCanary(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StageCanary() error = %v, want %q", err, tc.want)
			}
			if ev.Status == "success" || ev.MergeCommit != "" {
				t.Fatalf("unsafe StageCanary() emitted success evidence: %+v", ev)
			}
		})
	}
}

func TestCleanupSandboxRunClosesOnlyOwnedPullRequests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture is Unix only")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "closed.log")
	ghPath := filepath.Join(dir, "gh")
	script := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  cat <<'EOF'
[
  {"number": 11, "headRefName": "canary/run-42-worker", "body": "automated test"},
  {"number": 12, "headRefName": "feature/other", "body": "canary-run: run-42"},
  {"number": 13, "headRefName": "canary/run-99-worker", "body": "canary-run: run-99"},
  {"number": 14, "headRefName": "main-work", "body": "unrelated PR"}
]
EOF
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "close" ]; then
  echo "$3" >> "` + logPath + `"
  exit 0
fi
exit 1
`
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	if err := CleanupSandboxRun(context.Background(), ghPath, "giantswarm-sandbox/canary", "giantswarm-sandbox", "run-42"); err != nil {
		t.Fatal(err)
	}
	closed, err := os.ReadFile(logPath) //nolint:gosec // test log path
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(closed)); got != "11\n12" {
		t.Fatalf("closed PRs = %q, want 11 and 12 only", got)
	}
}

func TestWriteSiblingEvidenceBindsPlatformAndDigest(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "beekeeper-darwin-arm64")
	payload := []byte("darwin-arm64-binary-payload")
	if err := os.WriteFile(sibling, payload, 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	base := update.CanaryEvidence{
		Schema:      1,
		Version:     "v0.99.0-rc.1",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		SHA256:      strings.Repeat("a", 64),
		OS:          "linux",
		Arch:        "amd64",
		Status:      "success",
		Run:         "run-1",
		MergeCommit: strings.Repeat("b", 40),
	}
	if err := writeSiblingEvidence(sibling, base); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sibling + ".canary.json") //nolint:gosec // test evidence path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	got, err := update.ValidateEvidenceFor(raw, "v0.99.0-rc.1", hex.EncodeToString(sum[:]), "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != base.Commit || got.MergeCommit != base.MergeCommit || got.Run != base.Run {
		t.Fatalf("unexpected sibling evidence: %+v", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["os"] != "darwin" || parsed["arch"] != "arm64" {
		t.Fatalf("sibling platform = %v/%v, want darwin/arm64", parsed["os"], parsed["arch"])
	}
}

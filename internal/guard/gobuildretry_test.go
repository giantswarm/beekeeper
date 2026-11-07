package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGoBuildRetry(t *testing.T) {
	const killedExitCode = "137"
	for _, tt := range []struct {
		name, command, first, second string
		wantRC, attempts             int
	}{
		{"killed build recovers", ClassBuild, killedExitCode, "0", 0, 2},
		{"retry is bounded", ClassBuild, killedExitCode, killedExitCode, 137, 2},
		{"ordinary failure", "build", "1", "0", 1, 1},
		{"successful build", "build", "0", "0", 0, 1},
		{"other go command", testCommand, killedExitCode, "0", 137, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCapper{}
			saved := plat.Capper
			plat.Capper = f
			t.Cleanup(func() { plat.Capper = saved })
			t.Setenv("CLAUDE_CODE_SESSION_ID", "retry-test")
			flags := ""
			if tt.name == "retry is bounded" {
				flags = "-mod=readonly -p=8"
			}
			t.Setenv("GOFLAGS", flags)
			t.Setenv("GOMAXPROCS", "8")
			dir := t.TempDir()
			t.Setenv("RETRY_TEST_DIR", dir)
			script := `#!/bin/sh
if [ ! -f "$RETRY_TEST_DIR/attempts" ]; then
 printf 'first\n' > "$RETRY_TEST_DIR/attempts"
 exit ` + tt.first + `
fi
printf 'retry\n' >> "$RETRY_TEST_DIR/attempts"
printf '%s\n%s\n' "$GOFLAGS" "$GOMAXPROCS" > "$RETRY_TEST_DIR/env"
exit ` + tt.second + "\n"
			goPath := filepath.Join(dir, "go")
			if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil { //nolint:gosec // a test fixture
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, CPUQuota: eightCores, Stderr: &stderr}
			if rc := Run(o, []string{goPath, tt.command, "./..."}); rc != tt.wantRC {
				t.Fatalf("exit %d, want %d: %s", rc, tt.wantRC, &stderr)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "attempts")) //nolint:gosec // a test fixture
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(raw), "\n"); got != tt.attempts {
				t.Fatalf("attempts %d, want %d", got, tt.attempts)
			}
			if len(f.ran) != tt.attempts {
				t.Fatalf("scopes %d, want %d", len(f.ran), tt.attempts)
			}
			if tt.attempts == 2 {
				raw, err := os.ReadFile(filepath.Join(dir, "env")) //nolint:gosec // a test fixture
				if err != nil {
					t.Fatal(err)
				}
				if string(raw) != strings.TrimSpace(flags+" -p=2")+"\n4\n" {
					t.Errorf("retry environment %q", raw)
				}
				if !strings.Contains(stderr.String(), "retrying go build once after exit 137 with GOFLAGS=-p=2 and GOMAXPROCS=4") {
					t.Errorf("missing bounds in log: %s", &stderr)
				}
			} else if strings.Contains(stderr.String(), "retrying") {
				t.Errorf("unexpected retry log: %s", &stderr)
			}
			if os.Getenv("GOMAXPROCS") != "8" || os.Getenv("GOFLAGS") != flags {
				t.Error("retry changed caller environment")
			}
		})
	}
}

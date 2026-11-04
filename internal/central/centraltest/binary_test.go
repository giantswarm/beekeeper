//go:build unix

package centraltest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryLiteralEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:1/mcp",
		"http://127.0.0.1:1/gateway/a%2Fb/mcp?scope=read%20write",
		"http://127.0.0.1:1/owner's/mcp",
	} {
		t.Run(endpoint, func(t *testing.T) {
			bin, _ := Binary(t, endpoint, "synthetic-token")
			out, err := exec.CommandContext(t.Context(), bin, "context", "show", "lab", "-o", "json").CombinedOutput() //nolint:gosec // the script is created by Binary in this test's temporary directory
			if err != nil {
				t.Fatalf("context show: %v: %s", err, out)
			}
			var got struct{ Endpoint string }
			if err := json.Unmarshal(out, &got); err != nil || got.Endpoint != endpoint {
				t.Fatalf("context show = %q, %v; want endpoint %q", out, err, endpoint)
			}
		})
	}
}

func TestBinaryQuotedTempDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "person's tmp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	t.Setenv("GOTMPDIR", dir)
	t.Run("custom temporary root", func(t *testing.T) {
		bin, tokenFile := Binary(t, "http://127.0.0.1:1/mcp", "first-synthetic-token")
		if !strings.Contains(tokenFile, "person's tmp") {
			t.Fatal("fixture did not use the quoted temporary root")
		}
		// Each invocation must read the current file, including after renewal.
		for _, token := range []string{"first-synthetic-token", "renewed-synthetic-token"} {
			if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(t.Context(), bin, "auth", "token", "--context", "lab").CombinedOutput() //nolint:gosec // the script is created by Binary in this test's temporary directory
			if err != nil || string(out) != token {
				t.Fatalf("auth token = %q, %v; want the current synthetic token", out, err)
			}
		}
	})
}

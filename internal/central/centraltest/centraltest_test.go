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
	for name, endpoint := range map[string]string{
		"ordinary":   "http://127.0.0.1:1/mcp",
		"encoded":    "http://127.0.0.1:1/gateway/a%2Fb/mcp?scope=read%20write",
		"apostrophe": "http://127.0.0.1:1/owner's/mcp",
		"shell":      "http://127.0.0.1:1/'$(exit 42)`exit 42`/mcp",
		"json":       "http://127.0.0.1:1/mcp?value=\"quoted\"\\suffix",
	} {
		t.Run(name, func(t *testing.T) {
			bin, _ := Binary(t, endpoint, "synthetic-token")
			out, err := exec.Command(bin, "context", "show", "lab", "-o", "json").CombinedOutput()
			if err != nil {
				t.Fatalf("context show: %v: %s", err, out)
			}
			var got struct {
				Name     string
				Endpoint string
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("decode context: %v: %s", err, out)
			}
			if got.Name != "lab" || got.Endpoint != endpoint {
				t.Fatalf("context = %+v, want name lab and endpoint %q", got, endpoint)
			}
		})
	}
}

func TestBinaryQuotedTempDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fixture's tmp")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	t.Setenv("GOTMPDIR", root)
	t.Run("token updates", func(t *testing.T) {
		bin, tokenFile := Binary(t, "http://127.0.0.1:1/mcp", "synthetic-token")
		if !strings.HasPrefix(tokenFile, root+string(os.PathSeparator)) {
			t.Fatalf("token file is outside the quoted temporary root")
		}
		for _, token := range []string{"synthetic-token", "replacement token\nwith another line\n", ""} {
			if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin, "auth", "token", "--context", "lab").CombinedOutput()
			if err != nil {
				t.Fatalf("auth token: %v: %s", err, out)
			}
			if string(out) != token {
				t.Fatalf("auth token = %q, want %q", out, token)
			}
		}
	})
}

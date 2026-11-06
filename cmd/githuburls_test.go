package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const urlDefaultsMode = "defaults"
const urlOverrideMode = "override"

func TestGitHubURL(t *testing.T) {
	for _, value := range []string{"", "https://example.test", "https://example.test///"} {
		t.Setenv("BEEKEEPER_TEST_URL", value)
		want := strings.TrimRight(value, "/")
		if want == "" {
			want = paramDefault
		}
		if got := githubURL("BEEKEEPER_TEST_URL", paramDefault); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// A fresh process verifies environment initialization, without assigning the
// package globals that the in-process flow tests replace.
func TestGitHubURLsFromEnvironment(t *testing.T) {
	if mode := os.Getenv("BEEKEEPER_TEST_URL_PROCESS"); mode != "" {
		if mode == urlDefaultsMode {
			if githubWeb != "https://github.com" || githubAPI != "https://api.github.com" {
				t.Fatal("public defaults changed")
			}
			return
		}
		flow, err := serveManifest(appOwner{login: fixtureOrg}, fixtureOrg, "manifest.json", map[string]any{"name": fixtureOrg})
		if err != nil {
			t.Fatal(err)
		}
		defer flow.stop()
		if !strings.Contains(flow.page, githubWeb+"/organizations/example/settings/apps/new?") {
			t.Fatal("manifest form ignored web URL")
		}
		if _, err := convertManifest(context.Background(), "example-code"); err != nil {
			t.Fatal(err)
		}
		if _, err := deviceFlow(context.Background(), "example-client"); err == nil {
			t.Fatal("expected fake device endpoint refusal")
		}
		return
	}
	for _, mode := range []string{urlDefaultsMode, urlOverrideMode} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Method + " " + r.URL.Path
				if r.URL.Path == "/app-manifests/example-code/conversions" {
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":42,"slug":fixtureOrg,"client_id":"example-client","client_secret":"fake-secret","pem":"fake-key"}`))
					return
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()
			t.Setenv("BEEKEEPER_TEST_URL_PROCESS", mode)
			t.Setenv(githubAPIEnv, "")
			t.Setenv(githubWebEnv, "")
			if mode == urlOverrideMode {
				t.Setenv(githubAPIEnv, srv.URL+"/")
				t.Setenv(githubWebEnv, srv.URL+"/")
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), self, "-test.run=^TestGitHubURLsFromEnvironment$") //nolint:gosec // this test executable
			if out, err := child.CombinedOutput(); err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
			if mode == urlOverrideMode {
				for _, want := range []string{"POST /app-manifests/example-code/conversions", "POST /login/device/code"} {
					select {
					case got := <-requests:
						if got != want {
							t.Fatalf("got %q, want %q", got, want)
						}
					default:
						t.Fatalf("missing %s", want)
					}
				}
			}
		})
	}
}

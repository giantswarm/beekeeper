package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// Exercise the CLI boundary, including Main's exit code, with transcript
// replay in place of Claude. No browser, network, model or shared state is used.
func TestBrowseDisconnectCLI(t *testing.T) {
	if runtime.GOOS == testGOOSWindows {
		t.Skip("the replay host uses a POSIX shell")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("testdata/browse-disconnect")
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.ReadFile(filepath.Join(fixture, "claude.sh")) //nolint:gosec // repository-owned replay fixture
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		exit    int
		outcome string
	}{
		{"recovered", 0, "found the browser and went on with the steps not done"},
		{"unavailable", 1, "found no browser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "claude"), host, 0o700); err != nil { //nolint:gosec // executable replay host in this test's private directory
				t.Fatal(err)
			}
			leases := filepath.Join(root, "leases")
			if _, err := lease.Dir(leases).Claim(browserResource, lease.Holder{Name: "Browser replay"}); err != nil {
				t.Fatal(err)
			}
			projects := filepath.Join(root, "projects")
			cfg := filepath.Join(root, "config.yaml")
			// Quote paths as YAML strings, including on systems with spaces in temp paths.
			config := "leaseDir: '" + strings.ReplaceAll(leases, "'", "''") + "'\nstateDir: '" + strings.ReplaceAll(filepath.Join(root, "state"), "'", "''") + "'\nclaude:\n  projectsDir: '" + strings.ReplaceAll(projects, "'", "''") + "'\n"
			if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			c := exec.CommandContext(ctx, self, "--config", cfg, "--as", "Browser replay", "browse", "--dir", root, "--timeout", "5s", "Open https://example.test/. Read the heading. Report it.") //nolint:gosec // this test executable, isolated configuration
			c.Env = append(os.Environ(), "BEEKEEPER_TEST_MAIN=1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"BROWSE_REPLAY_ROOT="+root, "BROWSE_REPLAY_FIXTURE="+fixture, "BROWSE_REPLAY_PROJECT="+projects, "BROWSE_REPLAY_OUTCOME="+tc.name)
			var out, stderr bytes.Buffer
			c.Stdout, c.Stderr = &out, &stderr
			err := c.Run()
			rc := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("browse: %v", err)
				}
				rc = exit.ExitCode()
			}
			if rc != tc.exit {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", rc, tc.exit, &out, &stderr)
			}
			text := out.String()
			for _, want := range []string{"Opened the page before the connection dropped.", "the first get_page_text", tc.outcome, "transcript: "} {
				if !strings.Contains(text, want) {
					t.Errorf("stdout missing %q:\n%s", want, text)
				}
			}
			lostCalls := "2"
			if tc.exit != 0 {
				lostCalls = "3"
			}
			if !strings.Contains(text, "connection lost: "+lostCalls+" calls found Claude in Chrome not connected, the first get_page_text; the reconnect turn, a fresh connection, "+tc.outcome) {
				t.Errorf("connection diagnostic: %s", text)
			}
			if tc.exit == 0 {
				if !strings.Contains(text, "The heading is Example Domain.") || stderr.Len() != 0 {
					t.Errorf("recovered report: %s\nstderr: %s", text, &stderr)
				}
			} else {
				for _, want := range []string{"browser connection lost at get_page_text", "the reconnect turn found no browser", "Read the heading; Report it"} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("stderr missing %q: %s", want, &stderr)
					}
				}
			}
			read := func(name string) string {
				t.Helper()
				b, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // files created by the isolated replay host
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			if n := strings.TrimSpace(read("count")); n != "2" {
				t.Fatalf("Claude turns = %s, want exactly 2", n)
			}
			first := strings.Split(strings.TrimSuffix(read("argv.1"), "\x00"), "\x00")
			second := strings.Split(strings.TrimSuffix(read("argv.2"), "\x00"), "\x00")
			i, j := slices.Index(first, "--session-id"), slices.Index(second, "--resume")
			if i < 0 || j < 0 || i+1 >= len(first) || j+1 >= len(second) {
				t.Fatalf("missing session/resume flags: %q / %q", first, second)
			}
			if first[i+1] != second[j+1] || slices.Contains(second, "--session-id") || !slices.Equal(first[:i], second[:j]) {
				t.Errorf("reconnect changed session or tool rules: %q / %q", first, second)
			}
			if !strings.Contains(second[len(second)-1], "connection was lost at your call get_page_text") {
				t.Errorf("resume prompt = %q", second[len(second)-1])
			}
			// The same discovered transcript must contain both turns, in order.
			transcript := read(filepath.Join("projects", "replay", first[i+1]+".jsonl"))
			if !strings.Contains(transcript, "Claude in Chrome is not connected") || !strings.Contains(transcript, "reconnect-list") {
				t.Errorf("incomplete replay transcript: %s", transcript)
			}
		})
	}
}

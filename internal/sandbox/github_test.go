package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// testToken is a token of the App's length, never a real one.
const testToken = "ghu_" + "0123456789abcdefghijklmnopqrstuvwxyz"

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestWriteGitHubRewritesInPlace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "beekeeper", "github")
	if err := WriteGitHub(dir, testToken); err != nil {
		t.Fatal(err)
	}
	hosts, git := filepath.Join(dir, GitHubHosts), filepath.Join(dir, GitHubGit)
	before := []uint64{inode(t, hosts), inode(t, git)}
	renewed := strings.Replace(testToken, "0", "9", 1)
	if err := WriteGitHub(dir, renewed); err != nil {
		t.Fatal(err)
	}
	if after := []uint64{inode(t, hosts), inode(t, git)}; after[0] != before[0] || after[1] != before[1] {
		t.Errorf("a renewal replaced the files (inodes %v, then %v): a mask holds the path's file", before, after)
	}
	raw, err := os.ReadFile(hosts) //nolint:gosec // the test's file
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com:\n    oauth_token: " + renewed + "\n    git_protocol: https\n"; string(raw) != want {
		t.Errorf("hosts.yml = %q, want %q", raw, want)
	}
	basic, err := GitBasic(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pair, _ := base64.StdEncoding.DecodeString(basic); string(pair) != "x-access-token:"+renewed {
		t.Error("git's credential is not the renewed token's Basic pair")
	}
	for _, p := range []string{dir, hosts, git} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v, want the user's only", p, fi.Mode().Perm())
		}
	}
}

func TestWriteGitHubRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := WriteGitHub(dir, "two words"); err == nil {
		t.Error("a token with a space: want a refusal")
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, GitHubHosts)); err != nil {
		t.Fatal(err)
	}
	if err := WriteGitHub(dir, testToken); err == nil {
		t.Error("a symlink at hosts.yml: want a refusal")
	}
}

func TestGitCredential(t *testing.T) {
	dir := t.TempDir()
	if err := WriteGitHub(dir, testToken); err != nil {
		t.Fatal(err)
	}
	basic, _ := GitBasic(dir)
	for _, tc := range []struct {
		name, op, in, want string
		err                bool
	}{
		{"github", "get", "capability[]=authtype\nprotocol=https\nhost=github.com\n\n", "capability[]=authtype\nauthtype=Basic\ncredential=" + basic + "\n", false},
		{"other host", "get", "capability[]=authtype\nprotocol=https\nhost=gitlab.com\n\n", "", false},
		{"plain http", "get", "capability[]=authtype\nprotocol=http\nhost=github.com\n\n", "", false},
		{"old git", "get", "protocol=https\nhost=github.com\n\n", "", true},
		{"store", "store", "protocol=https\nhost=github.com\nusername=x\npassword=y\n\n", "", false},
	} {
		var out bytes.Buffer
		err := GitCredential(tc.op, strings.NewReader(tc.in), &out, dir)
		if out.String() != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: out %q, err %v; want %q, error %v", tc.name, out.String(), err, tc.want, tc.err)
		}
	}
}

func TestKeepGitHubWritesWhatChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "github")
	ctx, cancel := context.WithCancel(context.Background())
	tokens := []string{testToken, testToken, ""}
	var said []string
	n := 0
	KeepGitHub(ctx, dir, time.Millisecond, func(context.Context) (string, error) {
		n++
		if n > len(tokens) {
			cancel()
			return testToken, nil
		}
		if tokens[n-1] == "" {
			return "", errors.New("no login")
		}
		return tokens[n-1], nil
	}, func(s string) { said = append(said, s) })
	written := 0
	for _, s := range said {
		if strings.Contains(s, testToken) {
			t.Fatalf("a line names the token: %q", s)
		}
		if strings.HasPrefix(s, "github token: written") {
			written++
		}
	}
	if written != 1 || !strings.Contains(strings.Join(said, "\n"), "not renewed (no login)") {
		t.Errorf("lines %q: want one write and the failed renewal", said)
	}
}

func TestSettingsMaskTheGitHubToken(t *testing.T) {
	p, home := policy(t)
	p.GitHub = GitHubDir("/run/user/1000")
	p.Vars = githubVars(p.GitHub, filepath.Join(home, ".go/bin/beekeeper"))
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Env     map[string]string
		Sandbox struct {
			Filesystem  struct{ DenyRead, AllowRead []string }
			Credentials struct {
				Files []struct {
					Path, Mode, Extract string
					InjectHosts         []string
				}
			}
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	files := s.Sandbox.Credentials.Files
	if len(files) != 2 || files[0].Path != "/run/user/1000/beekeeper/github/hosts.yml" || files[0].Mode != "mask" ||
		len(files[0].InjectHosts) != len(config.GitHubHosts) || files[1].Path != "/run/user/1000/beekeeper/github/git-credential" ||
		len(files[1].InjectHosts) != 1 || files[1].InjectHosts[0] != "github.com" {
		t.Errorf("credential files = %+v", files)
	}
	for _, f := range files {
		if f.Extract == "" {
			t.Errorf("%s masks the whole file: gh and git would read no structure", f.Path)
		}
	}
	// a mask under a re-allowed path is mounted without injection
	for _, r := range s.Sandbox.Filesystem.AllowRead {
		if strings.Contains(r, "github") {
			t.Errorf("allowRead re-opens %s", r)
		}
	}
	if s.Env["GH_CONFIG_DIR"] != p.GitHub || s.Env["GIT_CONFIG_COUNT"] != "4" || s.Env["GIT_CONFIG_VALUE_0"] != "git@github.com:" ||
		s.Env["GIT_CONFIG_VALUE_2"] != "" || s.Env["GIT_CONFIG_VALUE_3"] != "!'"+filepath.Join(home, ".go/bin/beekeeper")+"' sandbox git-credential" {
		t.Errorf("env = %v", s.Env)
	}
	if p.Readable(filepath.Join(p.GitHub, GitHubHosts), home) || p.Writable(filepath.Join(p.GitHub, GitHubGit), home) {
		t.Error("the file tools reach the token's files")
	}
}

func TestNoRuntimeDirNoGitHubFiles(t *testing.T) {
	p, _ := policy(t)
	if p.GitHub != "" {
		t.Fatalf("GitHub = %q without a runtime directory", p.GitHub)
	}
	if _, ok := p.Settings()["sandbox"].(map[string]any)["credentials"].(map[string]any)["files"]; ok {
		t.Error("credential files without a runtime directory")
	}
}

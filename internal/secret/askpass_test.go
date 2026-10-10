//go:build unix

package secret_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// TestMain makes the test binary the askpass helper too, as the beekeeper
// binary is: the helper copy --git-askpass writes runs this executable.
func TestMain(m *testing.M) {
	n := len(secret.AskpassArgs)
	if len(os.Args) > n && slices.Equal(os.Args[1:1+n], secret.AskpassArgs) {
		os.Exit(secret.AskpassMain(os.Args[1+n:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

const gitPush = "push"

const railsPod = "kubectl exec -i --context kind-lab -n gitlab toolbox-0 -- "

func TestConsumerGitlabRailsRunner(t *testing.T) {
	pod := strings.Fields(railsPod)
	for _, c := range []struct {
		argv []string
		ok   bool
	}{
		{append(slices.Clone(pod), "gitlab-rails", "runner", "t = PersonalAccessToken.new; t.set_token(STDIN.read.strip); t.save!"), true},
		{append(slices.Clone(pod), "/usr/bin/gitlab-rails", "runner", "-e", "production", "$stdin.read"), true},
		{append(slices.Clone(pod), "gitlab-rails", "runner", "--environment=production", "STDIN.read"), true},
		{[]string{"gitlab-rails", "runner", "STDIN.read"}, true},
		{append(slices.Clone(pod), "gitlab-rails", "runner", "/tmp/script.rb"), false},
		{append(slices.Clone(pod), "gitlab-rails", "runner", "puts 1", "STDIN"), false},
		{append(slices.Clone(pod), "gitlab-rails", "console"), false},
		{append(slices.Clone(pod), "gitlab-rails", "runner"), false},
		{append(slices.Clone(pod), "sh", "-c", "cat > /tmp/x"), false},
		{append(slices.Clone(pod), "ruby", "-e", "STDIN.read"), false},
		{strings.Fields("kubectl exec --context kind-lab toolbox-0 -- gitlab-rails runner STDIN.read"), false},
		{strings.Fields("git push https://gitlab.example/g/p.git"), false},
	} {
		if err := secret.Consumer(c.argv); (err == nil) != c.ok {
			t.Errorf("Consumer(%q) = %v, want allowed %v", c.argv, err, c.ok)
		}
	}
}

func TestGitConsumer(t *testing.T) {
	for argv, ok := range map[string]bool{
		"git push https://gitlab.example/g/p.git main":           true,
		"/usr/bin/git -C /repo push origin main":                 true,
		"git clone --depth 1 https://gitlab.example/g/p.git dst": true,
		"git fetch origin":                                       true,
		"git push -u origin main":                                true,
		"git ls-remote https://gitlab.example/g/p.git":           true,
		"git -c credential.helper=store push origin":             false,
		"git clone -c credential.helper=store https://h/p.git":   false,
		"git clone -qccredential.helper=store https://h/p.git":   false,
		"git clone --config=credential.helper=store https://h/p": false,
		"git clone -u /tmp/x https://h/p.git":                    false,
		"git fetch --upload-pack=/tmp/x origin":                  false,
		"git push --receive-pack /tmp/x origin":                  false,
		"git ls-remote -u /tmp/x https://h/p.git":                false,
		"git credential fill":                                    false,
		"git config credential.helper store":                     false,
		"git --git-dir /r push origin":                           false,
		"gh secret set X":                                        false,
	} {
		if err := secret.GitConsumer(strings.Fields(argv)); (err == nil) != ok {
			t.Errorf("GitConsumer(%q) = %v, want allowed %v", argv, err, ok)
		}
	}
}

// fakeGit writes a git that answers config as a repository without a
// credential helper and otherwise asks its askpass helper for the user and
// the password, keeps both, its argv and its environment aside in dir, and
// prints the password: the test reads what reached it.
func fakeGit(t *testing.T, dir string) string {
	t.Helper()
	git := filepath.Join(dir, "git")
	script := `#!/bin/sh
case "$1 $2 $3" in *config*) exit 1;; esac
u=$("$GIT_ASKPASS" "Username for 'https://gitlab.example': ") || exit 7
p=$("$GIT_ASKPASS" "Password for 'https://oauth2@gitlab.example': ") || exit 8
"$GIT_ASKPASS" "Are you sure you want to continue connecting (yes/no)? " && exit 9
printf '%s' "$u" > "` + dir + `/user"
printf '%s' "$p" > "` + dir + `/password"
echo "$@" > "` + dir + `/argv"
env > "` + dir + `/env"
cat "$GIT_ASKPASS" > "` + dir + `/helper"
echo "pushed with $p"
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test git
		t.Fatal(err)
	}
	return git
}

func TestCopyToGitAnswersTheAskpassPrompts(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: token})
	dir := t.TempDir()
	git := fakeGit(t, dir)
	t.Setenv("GIT_ASKPASS", "/caller/askpass")
	t.Setenv("GIT_CONFIG_GLOBAL", "/caller/gitconfig")
	code, out, err := ops(tools).CopyToGit(context.Background(), secret.Ref{Op: vaultRef}, []string{git, gitPush, "https://gitlab.example/g/p.git", "main"}, secret.GitUser)
	if err != nil || code != 0 {
		t.Fatalf("git = %d, %q, %v", code, out, err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the test's scratch file
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if u, p := read("user"), read("password"); u != "oauth2" || p != token {
		t.Fatalf("the helper answered user %q and a password of %d bytes", u, len(p))
	}
	for _, f := range []string{"argv", "env", "helper"} {
		if strings.Contains(read(f), token) {
			t.Errorf("the value is in git's %s", f)
		}
	}
	env := read("env")
	for _, kv := range []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_KEY_0=core.hooksPath"} {
		if !strings.Contains(env, kv+"\n") {
			t.Errorf("git's environment lacks %s", kv)
		}
	}
	if strings.Contains(env, "/caller/") {
		t.Error("git got the caller's askpass or global configuration")
	}
	if strings.Contains(out, token) || !strings.Contains(out, "[redacted: "+vaultRef+"]") {
		t.Errorf("output = %q", out)
	}
	helper := strings.TrimPrefix(lineWith(env, "GIT_ASKPASS="), "GIT_ASKPASS=")
	if _, err := os.Stat(filepath.Dir(helper)); !os.IsNotExist(err) {
		t.Errorf("the helper's directory %s outlives the call: %v", filepath.Dir(helper), err)
	}
	if _, _, err := ops(tools).CopyToGit(context.Background(), secret.Ref{Op: vaultRef}, []string{git, "-c", "credential.helper=store", gitPush}, secret.GitUser); err == nil {
		t.Error("git -c took a value")
	}
}

func TestCopyToGitRefusesARepositoryCredentialHelper(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: token})
	dir := t.TempDir()
	git := filepath.Join(dir, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\ncase \"$*\" in *config*) echo 'credential.https://gitlab.example.helper store'; exit 0;; esac\ntouch \""+dir+"/ran\"\n"), 0o700); err != nil { //nolint:gosec // an executable test git
		t.Fatal(err)
	}
	_, _, err := ops(tools).CopyToGit(context.Background(), secret.Ref{Op: vaultRef}, []string{git, gitPush, "origin"}, secret.GitUser)
	if err == nil || !strings.Contains(err.Error(), "credential.https://gitlab.example.helper") {
		t.Fatalf("a repository's credential helper = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("git ran")
	}
}

// A real git asks the helper once a server wants a credential, and sends
// the user and the value as its Basic credential.
func TestCopyToGitWithARealGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	got := make(chan bool, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		got <- u == "oauth2" && p == token
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	tools := secrettest.New(map[string]string{vaultRef: token})
	code, out, err := ops(tools).CopyToGit(context.Background(), secret.Ref{Op: vaultRef}, []string{git, "ls-remote", srv.URL + "/g/p.git"}, secret.GitUser)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-got:
		if !ok {
			t.Error("git sent another credential than the helper's")
		}
	default:
		t.Fatalf("git sent no credential: exit %d, %q", code, out)
	}
	if strings.Contains(out, token) {
		t.Error("the value is in git's output")
	}
}

// lineWith is the line of s starting with prefix, "" for none.
func lineWith(s, prefix string) string {
	for line := range strings.Lines(s) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

package secret_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	remoteHost  = "host.example"
	remoteOwner = "app:app"
	remoteSpec  = "ssh://" + remoteHost + "/etc/example/token"
)

// sshCall is one ssh command line a test's runner was asked to run and
// the stdin it got.
type sshCall struct {
	argv  []string
	stdin string
}

// remoteOps are the operations over the fake vault holding token, ssh
// recorded into calls and answering fail.
func remoteOps(t *testing.T, calls *[]sshCall, fail error) *secret.Ops {
	t.Helper()
	tools := secrettest.New(map[string]string{vaultRef: token})
	o := ops(tools)
	o.Run = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name != "ssh" {
			return tools.Run(ctx, dir, env, stdin, name, args...)
		}
		in, _ := io.ReadAll(stdin)
		*calls = append(*calls, sshCall{argv: append([]string{name}, args...), stdin: string(in)})
		return nil, fail
	}
	return o
}

func TestParseRemoteFile(t *testing.T) {
	f, err := secret.ParseRemoteFile("ssh://deploy@host.example:2222/etc/example/token")
	if err != nil {
		t.Fatal(err)
	}
	if f.User != "deploy" || f.Host != remoteHost || f.Port != "2222" || f.Path != "/etc/example/token" {
		t.Errorf("parsed %+v", f)
	}
	if s := f.String(); s != "ssh://deploy@host.example:2222/etc/example/token" {
		t.Errorf("String() = %q", s)
	}
	for _, bad := range []string{
		"ssh://host.example",                         // no path
		"ssh://host.example/",                        // the root
		"ssh://host.example/etc/../shadow",           // not clean
		"ssh://host.example/etc/example/",            // a directory
		"ssh://host.example/etc/a b",                 // a space
		"ssh://host.example/etc/$(id)",               // shell
		"ssh://host.example/etc/'x'",                 // a quote
		"ssh://-oProxyCommand=x/etc/example/token",   // an ssh option for a host
		"ssh://u:pw@host.example/etc/example/token",  // a password
		"ssh://-u@host.example/etc/example/token",    // an option for a user
		"ssh://host.example:99999/etc/example/token", // a port
		"ssh://host.example/etc/example/token?x=1",   // a query
		"scp://host.example/etc/example/token",       // no ssh://
	} {
		if _, err := secret.ParseRemoteFile(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestRemoteFileCheck(t *testing.T) {
	f, _ := secret.ParseRemoteFile(remoteSpec)
	for _, ok := range []secret.RemoteFile{
		{}, {Mode: "0640", Owner: remoteOwner}, {Mode: "600", Owner: "1000:1000"},
		{Template: "URL=https://api.example/push\nTOKEN={{value}}\n"},
	} {
		g := f
		g.Mode, g.Owner, g.Template = ok.Mode, ok.Owner, ok.Template
		if err := g.Check(); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
	for _, bad := range []secret.RemoteFile{
		{Mode: "0800"}, {Mode: "u+rw"}, {Mode: "0600; id"},
		{Owner: "app:app:x"}, {Owner: "app;id"}, {Owner: "-R"},
		{Template: "no placeholder"}, {Template: "{{value}}{{value}}"},
		{Template: "OTHER=" + "ghp_" + strings.Repeat("a1B2", 9) + "\nTOKEN={{value}}"},
	} {
		g := f
		g.Mode, g.Owner, g.Template = bad.Mode, bad.Owner, bad.Template
		if err := g.Check(); err == nil {
			t.Errorf("%+v passed", bad)
		}
	}
}

// TestCopyToRemoteValueOnStdinAlone: ssh runs once, without a TTY or a
// prompt, the host after "--", and the value on its stdin, never on its
// command line or in the answer.
func TestCopyToRemoteValueOnStdinAlone(t *testing.T) {
	var calls []sshCall
	o := remoteOps(t, &calls, nil)
	f, _ := secret.ParseRemoteFile(remoteSpec)
	f.Mode, f.Owner, f.Sudo = "0640", remoteOwner, true
	res, err := o.CopyToRemote(context.Background(), secret.Ref{Op: vaultRef}, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("ssh ran %d times", len(calls))
	}
	c := calls[0]
	if c.stdin != token {
		t.Errorf("stdin %q, want the value", c.stdin)
	}
	argv := strings.Join(c.argv, "\x00")
	if strings.Contains(argv, token) {
		t.Errorf("the value is on ssh's command line: %q", c.argv)
	}
	want := []string{"ssh", "-T", "-o", "BatchMode=yes", "--", remoteHost}
	if !slices.Equal(c.argv[:len(want)], want) || len(c.argv) != len(want)+1 {
		t.Errorf("argv %q, want %q and the script", c.argv, want)
	}
	script := c.argv[len(c.argv)-1]
	for _, s := range []string{"sudo -n sh -c ", "umask 077", "chmod 0640", "chown", remoteOwner, "/etc/example/.token.beekeeper.XXXXXX", "/etc/example/token", strconv.Itoa(len(token))} {
		if !strings.Contains(script, s) {
			t.Errorf("the script lacks %q: %s", s, script)
		}
	}
	if res.File != remoteSpec || res.Mode != "0640" || res.Owner != remoteOwner || res.Bytes != len(token) || res.Length != len(token) || res.Fingerprint != fmt.Sprintf("fp-%d", len(token)*7) {
		t.Errorf("answered %+v", res)
	}
	noValue(t, "copy to ssh", res)
}

func TestCopyToRemoteTemplate(t *testing.T) {
	var calls []sshCall
	o := remoteOps(t, &calls, nil)
	f, _ := secret.ParseRemoteFile(remoteSpec)
	f.Template = "URL=https://api.example/push\nTOKEN={{value}}\n"
	res, err := o.CopyToRemote(context.Background(), secret.Ref{Op: vaultRef}, f)
	if err != nil {
		t.Fatal(err)
	}
	body := "URL=https://api.example/push\nTOKEN=" + token + "\n"
	if len(calls) != 1 || calls[0].stdin != body {
		t.Fatalf("calls %+v", calls)
	}
	if res.Bytes != len(body) || res.Length != len(token) || !strings.Contains(calls[0].argv[len(calls[0].argv)-1], strconv.Itoa(len(body))) {
		t.Errorf("answered %+v, script %s", res, calls[0].argv[len(calls[0].argv)-1])
	}
}

// TestCopyToRemoteRefusesBeforeSSH: a reference that resolves to nothing,
// an empty value, a whole file or a bad mode fail before ssh runs.
func TestCopyToRemoteRefusesBeforeSSH(t *testing.T) {
	f, _ := secret.ParseRemoteFile(remoteSpec)
	for name, c := range map[string]struct {
		src  secret.Ref
		file secret.RemoteFile
	}{
		"unresolvable": {secret.Ref{Op: "op://Shared/absent/credential"}, f},
		"blank":        {secret.Ref{Op: "op://Shared/blank/credential"}, f},
		"whole file":   {secret.Ref{File: aFile}, f},
		"bad mode":     {secret.Ref{Op: vaultRef}, secret.RemoteFile{Host: f.Host, Path: f.Path, Mode: "999"}},
	} {
		var calls []sshCall
		o := remoteOps(t, &calls, nil)
		o.Run = func(next secret.Runner) secret.Runner {
			return func(ctx context.Context, dir string, env []string, stdin io.Reader, n string, args ...string) ([]byte, error) {
				if n == "op" && slices.Contains(args, "op://Shared/blank/credential") {
					return []byte("\n"), nil
				}
				return next(ctx, dir, env, stdin, n, args...)
			}
		}(o.Run)
		if _, err := o.CopyToRemote(context.Background(), c.src, c.file); err == nil {
			t.Errorf("%s: no error", name)
		}
		if len(calls) != 0 {
			t.Errorf("%s: ssh ran: %+v", name, calls)
		}
	}
}

// TestCopyToRemoteFailureRedacted: ssh failing answers an error without
// the value, naming the target as kept.
func TestCopyToRemoteFailureRedacted(t *testing.T) {
	var calls []sshCall
	o := remoteOps(t, &calls, errors.New("ssh: exit 1 (got "+token+")"))
	f, _ := secret.ParseRemoteFile(remoteSpec)
	_, err := o.CopyToRemote(context.Background(), secret.Ref{Op: vaultRef}, f)
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "target kept") {
		t.Errorf("error %q", err)
	}
}

// runScript runs a remote file's script in a local shell with stdin, as
// ssh would on the host.
func runScript(t *testing.T, f secret.RemoteFile, n int, stdin string) error {
	t.Helper()
	c := exec.Command("sh", "-c", f.Script(n)) //nolint:gosec // the script under test
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	if len(out) != 0 {
		t.Errorf("the script printed %q", out)
	}
	return nil
}

// leftovers are the files of dir but name.
func leftovers(t *testing.T, dir, name string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Name() != name {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestRemoteScriptWritesAndCleansUp runs the script itself: it places the
// file with its mode and owner, and on a short stream or a failing chown
// leaves the target as it was and no temp file.
func TestRemoteScriptWritesAndCleansUp(t *testing.T) {
	if _, err := exec.LookPath("mktemp"); err != nil {
		t.Skip("no mktemp")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	f := secret.RemoteFile{Host: remoteHost, Path: target, Mode: "0640", Owner: strconv.Itoa(os.Getuid())}
	if err := runScript(t, f, len(token), token); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target) //nolint:gosec // the test's temp file
	st, _ := os.Stat(target)
	if string(got) != token || st.Mode().Perm() != 0o640 {
		t.Errorf("wrote %q mode %v", got, st.Mode().Perm())
	}
	if l := leftovers(t, dir, "token"); l != nil {
		t.Errorf("left %v", l)
	}

	// a stream shorter than announced: the connection dropped
	if err := runScript(t, f, len(password)+5, password); err == nil || !strings.Contains(err.Error(), "short stream") {
		t.Errorf("short stream: %v", err)
	}
	// an owner the host does not know
	bad := f
	bad.Owner = "no-such-user-beekeeper"
	if err := runScript(t, bad, len(password), password); err == nil {
		t.Error("unknown owner: no error")
	}
	if got, _ := os.ReadFile(target); string(got) != token { //nolint:gosec // the test's temp file
		t.Errorf("a failure changed the target: %q", got)
	}
	if l := leftovers(t, dir, "token"); l != nil {
		t.Errorf("a failure left %v", l)
	}
}

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	storeOp  = "store"
	storeRef = "op://Shared/app/client-secret"
	appFlag  = "--github-app"
	keyLine  = "planted-Key-Lines-7c21e4"
	// pemValue is shaped like a key file; no key was ever in it, and no
	// key's header, which a leak scanner would flag.
	pemValue   = "-----BEGIN MADE-UP KEY-----\n" + keyLine + "\n-----END MADE-UP KEY-----\n"
	keyName    = "my-app.2026-10-10.private-key.pem"
	clientRef  = "op://Shared/my-app/client-secret"
	privateRef = "op://Shared/my-app/private-key"
)

// clipboardTools is the fake op with a fake Wayland clipboard in front.
type clipboardTools struct {
	*secrettest.Tools
	// pastes are what each wl-paste answers in turn; the last stays.
	pastes  []string
	cleared int
}

func (c *clipboardTools) run(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
	switch name {
	case "wl-paste":
		v := c.pastes[0]
		if len(c.pastes) > 1 {
			c.pastes = c.pastes[1:]
		}
		if v == "" {
			return nil, errors.New("wl-paste: exit 1 (Nothing is copied)")
		}
		return []byte(v), nil
	case "wl-copy":
		c.cleared++
		c.pastes = []string{""}
		return nil, nil
	}
	return c.Run(ctx, dir, env, stdin, name, args...)
}

// storeApp is secretApp in the person's terminal: a terminal, Enter read
// from terminalIn, the fake clipboard answering pastes in turn, and
// ~/Downloads in a scratch home.
func storeApp(t *testing.T, pastes ...string) (*app, *clipboardTools, string) {
	t.Helper()
	a, tools, _ := secretApp(t)
	c := &clipboardTools{Tools: tools, pastes: pastes}
	if len(pastes) == 0 {
		c.pastes = []string{""}
	}
	secretRun = c.run
	prevTerm, prevIn := isTerminal, terminalIn
	isTerminal = func(int) bool { return true }
	terminalIn = strings.NewReader("")
	t.Cleanup(func() { isTerminal, terminalIn = prevTerm, prevIn })
	for _, k := range agentMarkers {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	return a, c, downloads
}

// noKey fails when the private key's line is in s.
func noKey(t *testing.T, what, s string) {
	t.Helper()
	noSecret(t, what, s)
	if strings.Contains(s, keyLine) {
		t.Errorf("%s carries the key:\n%s", what, s)
	}
}

// storeEvents are the secret.store events logged, each without a value.
func storeEvents(t *testing.T, a *app) []state.Event {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret.store" })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		noKey(t, "the log", e.Detail)
	}
	return evs
}

// A store takes the clipboard's value into the field, prints its length and
// fingerprint, clears the clipboard, and leaves the value out of the
// answer, every command line and the log.
func TestSecretStoreTakesTheClipboardAndClearsIt(t *testing.T) {
	a, c, _ := storeApp(t, secretValue)
	out, err := runSecret(a, storeOp, storeRef)
	if err != nil {
		t.Fatalf("store: %v\n%s", err, out)
	}
	if c.Vault[storeRef] != secretValue {
		t.Errorf("the vault holds %q", c.Vault[storeRef])
	}
	want := fmt.Sprintf("wrote %s: %d bytes, hmac:", storeRef, len(secretValue))
	if !strings.HasPrefix(out, want) || !strings.Contains(out, "cleared the clipboard") || c.cleared != 1 {
		t.Errorf("the answer (cleared %d):\n%s", c.cleared, out)
	}
	noKey(t, "the answer", out)
	noKey(t, "the command lines", strings.Join(c.Calls, "\n"))
	evs := storeEvents(t, a)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, storeRef+" from the clipboard: "+fmt.Sprint(len(secretValue))+" bytes, hmac:") {
		t.Errorf("the log: %+v", evs)
	}
}

// While the clipboard is empty the store names the click and waits for
// Enter; with nothing to press Enter it is refused and stores nothing.
func TestSecretStoreWaitsForTheClipboard(t *testing.T) {
	a, c, _ := storeApp(t, "", secretValue)
	terminalIn = strings.NewReader("\n")
	out, err := runSecret(a, storeOp, storeRef)
	if err != nil || c.Vault[storeRef] != secretValue {
		t.Fatalf("store after Enter: %v\n%s", err, out)
	}
	if !strings.Contains(out, "the clipboard is empty: copy the value, then press Enter") {
		t.Errorf("no wait for the click:\n%s", out)
	}
	a, c, _ = storeApp(t, "")
	out, err = runSecret(a, storeOp, storeRef)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "nothing stored") || c.cleared != 0 {
		t.Errorf("without Enter: %v (cleared %d)\n%s", err, c.cleared, out)
	}
	if _, ok := c.Vault[storeRef]; ok {
		t.Error("an empty clipboard was stored")
	}
}

// --github-app stores the client secret from the clipboard and the private
// key from the newest download, shreds the key file, and names each click;
// --from-file names the key and --keep-file keeps it.
func TestSecretStoreGitHubApp(t *testing.T) {
	a, c, downloads := storeApp(t, " "+secretValue+"\n")
	old := filepath.Join(downloads, "old-app.2026-01-01.private-key.pem")
	key := filepath.Join(downloads, keyName)
	for p, body := range map[string]string{old: "older", key: pemValue} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	out, err := runSecret(a, storeOp, appFlag, "my-app")
	if err != nil {
		t.Fatalf("store --github-app: %v\n%s", err, out)
	}
	if c.Vault[clientRef] != secretValue || c.Vault[privateRef] != pemValue {
		t.Errorf("the vault holds client %q, key %q", c.Vault[clientRef], c.Vault[privateRef])
	}
	for _, want := range []string{
		"taking " + key,
		fmt.Sprintf("wrote %s: %d bytes, hmac:", clientRef, len(secretValue)),
		fmt.Sprintf("wrote %s: %d bytes, hmac:", privateRef, len(pemValue)),
		"cleared the clipboard", "shredded " + key,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the key file: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the older download: %v", err)
	}
	noKey(t, "the answer", out)
	noKey(t, "the command lines", strings.Join(c.Calls, "\n"))
	if evs := storeEvents(t, a); len(evs) != 2 {
		t.Errorf("%d store events, want 2", len(evs))
	}

	// the download not there yet: the click is named, Enter waited for
	a, c, downloads = storeApp(t, secretValue)
	terminalIn = &lateDownload{path: filepath.Join(downloads, keyName)}
	a.out = &bytes.Buffer{}
	if out, err := runSecret(a, storeOp, appFlag, "my-app"); err != nil || c.Vault[privateRef] != pemValue ||
		!strings.Contains(out, "no "+filepath.Join(downloads, githubKeyGlob)+`: click "Generate a private key"`) {
		t.Errorf("a late download: %v\n%s", err, out)
	}

	a, c, _ = storeApp(t, secretValue)
	named := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(named, []byte(pemValue), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runSecret(a, storeOp, appFlag, "other", "--from-file", named, "--keep-file")
	if err != nil || c.Vault["op://Shared/other/private-key"] != pemValue || !strings.Contains(out, "kept "+named) {
		t.Errorf("--from-file --keep-file: %v\n%s", err, out)
	}
	if _, err := os.Stat(named); err != nil {
		t.Errorf("the kept file: %v", err)
	}
}

// lateDownload is a terminal whose Enter arrives with the download.
type lateDownload struct {
	path string
	done bool
}

func (l *lateDownload) Read(p []byte) (int, error) {
	if l.done {
		return 0, io.EOF
	}
	l.done = true
	if err := os.WriteFile(l.path, []byte(pemValue), 0o600); err != nil {
		return 0, err
	}
	return copy(p, "\n"), nil
}

// The argument shapes: one op:// reference or --github-app <item>, a file
// to keep only with one named, the shared vault only; and the call is the
// person's, refused in an agent session, and stops before any click while
// no vault could take the value.
func TestSecretStoreShapes(t *testing.T) {
	a, c, _ := storeApp(t, secretValue)
	for _, bad := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{storeOp}, ExitUsage, "1 arg"},
		{[]string{storeOp, "a.sops.yaml#x"}, ExitUsage, "op://<vault>/<item>/<field>"},
		{[]string{storeOp, storeRef, appFlag, "x"}, ExitUsage, "unknown command"},
		{[]string{storeOp, storeRef, "--keep-file"}, ExitUsage, "--keep-file"},
		{[]string{storeOp, appFlag, "a/b"}, ExitUsage, "item's name"},
		{[]string{storeOp, "op://Other/app/x"}, ExitError, "only the shared vault"},
	} {
		a.out = &bytes.Buffer{}
		_, err := runSecret(a, bad.args...)
		if Code(err) != bad.code || err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("secret %s: %v (exit %d), want exit %d with %q", strings.Join(bad.args, " "), err, Code(err), bad.code, bad.want)
		}
	}
	if len(c.Vault) != 1 || c.cleared != 0 {
		t.Errorf("a refused store wrote: %q, cleared %d", c.Vault, c.cleared)
	}
	t.Setenv("CLAUDECODE", "1")
	if _, err := runSecret(a, storeOp, storeRef); Code(err) != ExitRefused || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Errorf("in an agent session: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	a.cfg.Secret.Vault = ""
	if _, err := runSecret(a, storeOp, storeRef); Code(err) != ExitVault {
		t.Errorf("without a shared vault: %v", err)
	}
	if _, err := runSecret(a, storeOp, appFlag, "app"); Code(err) != ExitVault {
		t.Errorf("--github-app without a shared vault: %v", err)
	}
	if len(c.pastes) != 1 || c.pastes[0] != secretValue {
		t.Error("a refused store read the clipboard")
	}
}

// The broker's storer writes with the session it is given, as the person,
// in its own process: no service account token, no value on a command line.
func TestVaultStoreIsTheBrokersStorer(t *testing.T) {
	a, tools, _ := secretApp(t)
	a.cfg.Secret.Session = true
	tools.Signed = true
	var envs [][]string
	secretRun = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		envs = append(envs, env)
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	const session = "OP_SESSION_T=tok"
	s, err := a.vaultStore(context.Background(), session, storeRef, secretValue)
	if err != nil || s.Ref != storeRef || s.Bytes != len(secretValue) || !strings.HasPrefix(s.Fingerprint, "hmac:") {
		t.Fatalf("vaultStore = %+v, %v", s, err)
	}
	if tools.Vault[storeRef] != secretValue {
		t.Error("the vault does not hold the value")
	}
	for _, env := range envs {
		if len(env) != 1 || env[0] != session {
			t.Errorf("op ran with %q, want the session", env)
		}
	}
	if len(envs) == 0 || len(tools.Tokens) != 0 {
		t.Errorf("%d op calls, tokens %q", len(envs), tools.Tokens)
	}
	noSecret(t, "the command lines", strings.Join(tools.Calls, "\n"))
	for _, bad := range []string{"op://Other/app/x", "nonsense", "op://Shared/x"} {
		if _, err := a.vaultStore(context.Background(), session, bad, secretValue); err == nil {
			t.Errorf("%s was stored", bad)
		}
	}
}

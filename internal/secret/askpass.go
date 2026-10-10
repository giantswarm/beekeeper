package secret

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// AskpassArgs are the arguments after the binary that make beekeeper the
// git askpass helper: secret askpass <socket> <prompt>.
var AskpassArgs = []string{"secret", "askpass"}

// GitUser is the user name the askpass helper answers by default: GitLab
// takes any for a token, GitHub too.
const GitUser = "oauth2"

// gitOps are the git commands that take a credential from GIT_ASKPASS.
var gitOps = []string{gitClone, "fetch", "push", gitLsRemote}

const (
	gitClone    = "clone"
	gitLsRemote = "ls-remote"
)

// GitConsumer allows a git operation that authenticates over HTTPS with the
// credential beekeeper's askpass helper answers: git [-C <dir>] clone,
// fetch, push or ls-remote, without an option that sets configuration or
// names a program to run (a credential helper or an upload-pack would get
// the credential's way in).
func GitConsumer(argv []string) error {
	cmd := strings.Join(argv, " ")
	if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
		return fmt.Errorf("%q: --git-askpass runs git [-C <dir>] %s …", cmd, strings.Join(gitOps, "|"))
	}
	i := gitOp(argv)
	if i == len(argv) || !slices.Contains(gitOps, argv[i]) {
		return fmt.Errorf("%q: --git-askpass runs git [-C <dir>] %s …, no other global option", cmd, strings.Join(gitOps, "|"))
	}
	// clone and ls-remote take -u for --upload-pack, clone -c for --config
	short := ""
	switch argv[i] {
	case gitClone:
		short = "cu"
	case gitLsRemote:
		short = "u"
	}
	for _, a := range argv[i+1:] {
		if a == "--" {
			break
		}
		long := strings.SplitN(a, "=", 2)[0]
		if slices.Contains([]string{"--config", "--upload-pack", "--receive-pack", "--exec"}, long) ||
			short != "" && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a[1:], short) {
			return fmt.Errorf("%q: %s sets configuration or runs a program, which would reach the credential", cmd, a)
		}
	}
	return nil
}

// gitOp is the index of git's command in argv, past -C <dir> pairs.
func gitOp(argv []string) int {
	i := 1
	for i+1 < len(argv) && argv[i] == "-C" {
		i += 2
	}
	return i
}

// CopyToGit runs an allowed git operation with GIT_ASKPASS answering user
// and the value, which never reaches git's argv, its environment or a file.
// It answers git's exit code and its output with the value redacted.
func (o *Ops) CopyToGit(ctx context.Context, src Ref, argv []string, user string) (int, string, error) {
	if err := GitConsumer(argv); err != nil {
		return 0, "", err
	}
	if user == "" {
		return 0, "", errors.New("--git-user: a user name for the askpass helper's Username prompt")
	}
	v, err := o.encoded(ctx, src)
	if err != nil {
		return 0, "", err
	}
	return askpassGit(ctx, v, user, src.String(), argv)
}

// askpassExe is the binary the askpass helper runs: this beekeeper.
var askpassExe = os.Executable

// askpassGit runs git with a temporary askpass helper, a script in a 0700
// directory that runs this beekeeper as secret askpass <socket> <prompt>.
// beekeeper answers on the socket, from its memory, a process of git's
// alone; the directory goes once git exits.
func askpassGit(ctx context.Context, v, user, ref string, argv []string) (int, string, error) {
	exe, err := askpassExe()
	if err != nil {
		return 0, "", err
	}
	dir, err := os.MkdirTemp("", "beekeeper-askpass-")
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = l.Close() }()
	helper := filepath.Join(dir, "askpass")
	script := "#!/bin/sh\nexec " + shellQuote(exe) + " " + strings.Join(AskpassArgs, " ") + " " + shellQuote(sock) + " \"$1\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil { //nolint:gosec // the helper git runs
		return 0, "", err
	}
	if err := gitCredentialFree(ctx, argv); err != nil {
		return 0, "", err
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // an allow-listed git operation
	c.Env = gitEnv(os.Environ(), helper)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		return 0, "", err
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		serveAskpass(l, c.Process.Pid, user, v)
	}()
	code := 0
	if err := c.Wait(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return 0, "", err
		}
		code = ee.ExitCode()
	}
	_ = l.Close()
	<-served
	return code, redact(out.String(), v, ref), nil
}

// gitCredentialFree refuses a repository whose own configuration names a
// credential helper: git would hand it the value to store. The global and
// system configuration are off for the call (gitEnv).
func gitCredentialFree(ctx context.Context, argv []string) error {
	op := gitOp(argv)
	if argv[op] == gitClone {
		return nil
	}
	q := append(slices.Clone(argv[:op]), "config", "--local", "--includes", "--get-regexp", `^credential\.`)
	c := exec.CommandContext(ctx, q[0], q[1:]...) //nolint:gosec // git config, read only
	c.Env = gitEnv(os.Environ(), "")
	out, err := c.Output()
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		keys := []string{}
		for line := range strings.Lines(string(out)) {
			keys = append(keys, strings.Fields(line)[0])
		}
		return fmt.Errorf("the repository's configuration names %s: git would hand the credential on; --git-askpass runs only without one", strings.Join(keys, ", "))
	}
	return nil
}

// gitEnv is env for a git call: the caller's askpass, trace and config
// variables dropped, beekeeper's askpass helper set (when helper is
// given), no terminal prompt, no global or system configuration (a
// credential helper there would answer, or store, first) and no hooks.
func gitEnv(env []string, helper string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "GIT_ASKPASS" || k == "SSH_ASKPASS" || k == "GIT_TERMINAL_PROMPT" || k == "GIT_CURL_VERBOSE" ||
			strings.HasPrefix(k, "GIT_CONFIG") || strings.HasPrefix(k, "GIT_TRACE")
	})
	out = append(out, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+os.DevNull)
	if helper != "" {
		out = append(out, "GIT_ASKPASS="+helper)
	}
	return out
}

// serveAskpass answers the helper's prompts on l until it closes: user for
// git's Username prompt, v for its Password prompt, and only to a process
// that descends from git (pid).
func serveAskpass(l net.Listener, pid int, user, v string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		answerAskpass(c, pid, user, v)
	}
}

// answerAskpass answers one prompt on c, or closes c without an answer.
func answerAskpass(c net.Conn, pid int, user, v string) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if askpassPeer(c, pid) != nil {
		return
	}
	prompt, err := bufio.NewReader(io.LimitReader(c, 4096)).ReadString('\n')
	if err != nil {
		return
	}
	switch {
	case strings.HasPrefix(prompt, "Username"):
		_, _ = io.WriteString(c, user)
	case strings.HasPrefix(prompt, "Password"):
		_, _ = io.WriteString(c, v)
	}
}

// Askpass is the helper's side: it asks the socket for prompt's answer and
// writes it to w, for git to read. A prompt beekeeper answers nothing to
// fails.
func Askpass(sock, prompt string, w io.Writer) error {
	c, err := net.DialTimeout("unix", sock, 10*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, strings.ReplaceAll(prompt, "\n", " ")+"\n"); err != nil {
		return err
	}
	answer, err := io.ReadAll(io.LimitReader(c, 64*1024))
	if err != nil {
		return err
	}
	if len(answer) == 0 {
		return fmt.Errorf("beekeeper answers no %q", strings.TrimSpace(prompt))
	}
	_, err = fmt.Fprintf(w, "%s\n", answer)
	return err
}

// AskpassMain runs the askpass helper for args after AskpassArgs, answering
// the exit code.
func AskpassMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, "beekeeper: secret askpass <socket> <prompt> is git's askpass helper, which copy --git-askpass starts")
		return 2
	}
	if err := Askpass(args[0], args[1], stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "beekeeper:", err)
		return 1
	}
	return 0
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

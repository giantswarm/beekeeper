package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The GitHub token of a sandboxed session is the devctl App's short-lived
// user token, which the broker keeps in two files of the user's runtime
// directory: gh's login (GitHubHosts) and git's Basic credential
// (GitHubGit). The policy masks both: commands read them with a
// placeholder in place of the token, and the sandbox proxy puts the real
// value into requests to GitHub's hosts only. The proxy re-reads the files
// for every command, so a renewed token reaches a running session.
//
// The runtime directory lies outside the home directory, which the policy
// denies: a mask under a denied path is never mounted, and one under a
// re-allowed path is mounted without injection.
const (
	// GitHubHosts is gh's hosts.yml, read through GH_CONFIG_DIR.
	GitHubHosts = "hosts.yml"
	// GitHubGit is git's Basic credential, read by beekeeper sandbox
	// git-credential: GitHub's git endpoint takes Basic only, and the
	// proxy substitutes the placeholder only verbatim, so the file holds
	// the encoded pair.
	GitHubGit = "git-credential"
)

// The masks' extract patterns: capture group 1 is the masked value.
const (
	githubHostsExtract = `oauth_token: (\S+)`
	githubGitExtract   = `basic: (\S+)`
)

// GitHubDir is where the broker keeps the token under the runtime
// directory, "" without one.
func GitHubDir(runtimeDir string) string {
	if runtimeDir == "" {
		return ""
	}
	return filepath.Join(runtimeDir, "beekeeper", "github")
}

// githubFiles are the files the broker writes for token: name and content.
func githubFiles(token string) map[string]string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return map[string]string{
		GitHubHosts: "github.com:\n    oauth_token: " + token + "\n    git_protocol: https\n",
		GitHubGit:   "basic: " + basic + "\n",
	}
}

// WriteGitHub puts token into dir's files. Each file is rewritten in
// place, never replaced by a rename: the sandbox masks a path, and a file
// swapped under it is a new file to a command that already runs. The
// token's length is fixed, so a reader never meets a short file.
func WriteGitHub(dir, token string) error {
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("not a GitHub token")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for name, content := range githubFiles(token) {
		if err := writeInPlace(filepath.Join(dir, name), content); err != nil {
			return err
		}
	}
	return nil
}

// writeInPlace writes content into path from its start and cuts it there,
// keeping the file's inode; a symlink at path is refused.
func writeInPlace(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the broker's own file under the runtime directory
	if err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(content), 0); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Truncate(int64(len(content))); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// basicLine is the git credential file's line.
var basicLine = regexp.MustCompile(`(?m)^` + githubGitExtract + `$`)

// GitBasic is the Basic credential in dir's git file: inside the sandbox
// the placeholder the proxy replaces.
func GitBasic(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, GitHubGit)) //nolint:gosec // the policy's masked file
	if err != nil {
		return "", err
	}
	m := basicLine.FindSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%s holds no credential", filepath.Join(dir, GitHubGit))
	}
	return string(m[1]), nil
}

// GitCredential answers one git credential helper call (git-credential(1))
// with in's attributes: a get for https://github.com is answered with the
// Basic credential as an authtype=Basic credential, which git sends as
// written, so the proxy finds the placeholder. Store and erase and other
// hosts get nothing; git older than 2.46 does not take authtype and gets
// nothing either.
func GitCredential(op string, in io.Reader, out io.Writer, dir string) error {
	raw, err := io.ReadAll(io.LimitReader(in, 64<<10))
	if err != nil {
		return err
	}
	if op != "get" {
		return nil
	}
	attrs := map[string]bool{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		attrs[strings.TrimSpace(line)] = true
	}
	if !attrs["protocol=https"] || !attrs["host=github.com"] {
		return nil
	}
	if !attrs["capability[]=authtype"] {
		return errors.New("git does not take an authtype credential: git 2.46 or newer pushes from the agent sandbox")
	}
	basic, err := GitBasic(dir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "capability[]=authtype\nauthtype=Basic\ncredential=%s\n", basic)
	return err
}

// KeepGitHub writes the token token returns into dir now and every every
// until ctx ends, telling say what happened, never the token. A failed
// read leaves the files as they are: a session's GitHub calls fail once
// the token in them expires, and say names why.
func KeepGitHub(ctx context.Context, dir string, every time.Duration, token func(context.Context) (string, error), say func(string)) {
	last := ""
	for {
		t, err := token(ctx)
		switch {
		case err != nil:
			say(fmt.Sprintf("github token: not renewed (%v); sandboxed sessions keep the token in %s until it expires", err, dir))
		case t != last:
			if err := WriteGitHub(dir, t); err != nil {
				say(fmt.Sprintf("github token: not written to %s (%v)", dir, err))
			} else {
				say("github token: written to " + dir)
				last = t
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

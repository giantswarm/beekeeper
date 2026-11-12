//go:build scenario && linux

package harness

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// The scenario's git is the real git with one transport replaced: a
// repository's GitHub URL (https://github.com/<owner>/<name>.git) leads to
// its bare origin in the sandbox. Beekeeper names a repository by that URL
// wherever it fetches or pushes itself (a publication, a host's source
// checkout) and strips every GIT_ variable first, so the scenario's
// environment cannot redirect it: the git on PATH is this wrapper, which
// puts the remote helper below ahead of git's own. Every transport of a
// scenario, the desk's, a worker's and an execution host's, so ends in
// the same origins, and a remote still reads as its GitHub URL.
const (
	// RealGit is the real git in the sandbox and RealGitCore its exec
	// path; GitCore holds the scenario's remote helpers.
	RealGit     = "/work/bin/real-git"
	RealGitCore = "/work/bin/real-git-core"
	GitCore     = "/work/bin/git-core"
	// OriginsDir holds the bare origins (<owner>/<name>.git).
	OriginsDir = "/work/origins"
	// GitHub is the prefix of a repository's URL on GitHub.
	GitHub = "https://github.com/"
)

// RemoteHelpers are the git remote helpers the scenario replaces.
var RemoteHelpers = []string{"git-remote-https", "git-remote-http"}

// git runs the real git with the scenario's remote helpers first on its
// exec path and its own exec path behind them.
func git() int {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_EXEC_PATH=") || strings.HasPrefix(kv, "PATH=")
	})
	path := os.Getenv("PATH")
	if !slices.Contains(filepath.SplitList(path), RealGitCore) {
		path = strings.TrimPrefix(path+string(os.PathListSeparator)+RealGitCore, string(os.PathListSeparator))
	}
	env = append(env, "GIT_EXEC_PATH="+GitCore, "PATH="+path)
	if err := syscall.Exec(RealGit, append([]string{"git"}, os.Args[1:]...), env); err != nil { //nolint:gosec // the sandbox's real git
		fmt.Fprintf(os.Stderr, "git: the scenario's real git %s: %v\n", RealGit, err)
	}
	return 127
}

// originOf is the bare origin a GitHub URL names, "" for any other URL.
func originOf(url string) string {
	rest, ok := strings.CutPrefix(url, GitHub)
	if !ok {
		return ""
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git")
	if parts := strings.Split(rest, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(rest, "..") {
		return ""
	}
	return filepath.Join(OriginsDir, rest+".git")
}

// gitRemote is git's remote helper for https in a scenario: it connects
// git to the upload-pack or receive-pack of the origin the URL names, as
// GitHub would serve it, and refuses every other URL as an unreachable
// network does.
func gitRemote() int {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "fatal: the scenario's remote helper takes a remote and a URL")
		return 128
	}
	url := os.Args[2]
	origin := originOf(url)
	if origin == "" {
		fmt.Fprintf(os.Stderr, "fatal: unable to access '%s': Could not resolve host: the scenario has no network\n", url)
		return 128
	}
	if fi, err := os.Stat(origin); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "remote: Repository not found.\nfatal: repository '%s' not found\n", url)
		return 128
	}
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return 0
		}
		switch line = strings.TrimSuffix(line, "\n"); {
		case line == "capabilities":
			fmt.Print("connect\n\n")
		case strings.HasPrefix(line, "option "):
			fmt.Println("unsupported")
		case line == "connect git-upload-pack" || line == "connect git-receive-pack":
			// The empty line says the connection stands; git then speaks the
			// pack protocol with the service on this process's pipes.
			fmt.Println()
			service := strings.TrimPrefix(line, "connect git-")
			if err := syscall.Exec(RealGit, []string{"git", service, origin}, os.Environ()); err != nil { //nolint:gosec // the sandbox's real git
				fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			}
			return 128
		case line == "":
			return 0
		default:
			fmt.Fprintf(os.Stderr, "fatal: the scenario's remote helper does not know %q\n", line)
			return 128
		}
	}
}

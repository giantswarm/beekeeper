package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
)

// Seams for the tests.
var (
	pullState     = github.PullState
	devctlVersion = toolVersion
)

// mergeFiles is where a merge's devctl writes its document (.json) and its
// stderr (.log): the state directory, not the caller's pipes, which close
// with the caller's session.
func (g *gateRun) mergeFiles() (string, error) {
	dir := filepath.Join(g.store.Dir(), "merges")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d", strings.ReplaceAll(g.repo, "/", "_"), g.pr)), nil
}

// followPoll is how often the gate copies devctl's new stderr lines.
const followPoll = 200 * time.Millisecond

// runDetached runs the merge's devctl in a session of its own, so the end of
// its caller's session does not end the merge: its stdout goes to
// base.json, its stderr to base.log, which the gate follows onto its own
// stderr, and it returns the document once devctl ends. Only SIGINT,
// a person's Ctrl-C, reaches devctl; SIGTERM and SIGHUP, a caller going
// away, do not (runMerge keeps them from ending the gate). started gets devctl's
// pid. rc is devctl's exit code, 128+n for a signal.
func runDetached(argv []string, base string, started func(pid int)) (doc []byte, rc int) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		gateLine("%v", err)
		return nil, guard.ExitNotFound
	}
	out, err := os.Create(base + ".json") //nolint:gosec // the gate's own file under the state directory
	if err != nil {
		gateLine("%v", err)
		return nil, guard.ExitNotFound
	}
	defer func() { _ = out.Close(); _ = os.Remove(out.Name()) }()
	log, err := os.Create(base + ".log") //nolint:gosec // the gate's own file under the state directory
	if err != nil {
		gateLine("%v", err)
		return nil, guard.ExitNotFound
	}
	defer func() { _ = log.Close(); _ = os.Remove(log.Name()) }()
	c := exec.Command(path, argv[1:]...) //nolint:gosec // running the caller's command is the purpose
	c.Stdout, c.Stderr = out, log
	detach(c)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := c.Start(); err != nil {
		gateLine("%v", err)
		return nil, guard.ExitNotFound
	}
	started(c.Process.Pid)
	done := make(chan struct{})
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		follow(base+".log", os.Stderr, done)
	}()
	go func() {
		said := false
		for s := range sig {
			switch {
			case s == syscall.SIGINT:
				_ = c.Process.Signal(s)
			case !said:
				gateLine("%v: the caller is going away; devctl (pid %d) merges on in its own session and the gate records its outcome", s, c.Process.Pid)
				said = true
			}
		}
	}()
	_ = c.Wait()
	close(done)
	<-followed
	doc, _ = os.ReadFile(out.Name())
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return doc, 128 + int(ws.Signal())
	}
	return doc, c.ProcessState.ExitCode()
}

// follow copies what is appended to the file at path onto w until done,
// then the rest.
func follow(path string, w io.Writer, done <-chan struct{}) {
	f, err := os.Open(path) //nolint:gosec // the gate's own log file
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	for {
		select {
		case <-done:
			_, _ = io.Copy(w, f)
			return
		case <-time.After(followPoll):
			if _, err := io.Copy(w, f); err != nil && !errors.Is(err, io.EOF) {
				return
			}
		}
	}
}

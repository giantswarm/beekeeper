//go:build unix

package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The first run of a command leads, a second one while the leader's
// merge-child runs follows it, and once the leader left its result the next
// run leads again.
func TestOneRunPollsPerCommand(t *testing.T) {
	dir := t.TempDir()
	argv := strings.Fields("devctl pr wait o/r 7")
	first := filepath.Join(dir, ownedRuns, "pr-wait-o_r-7-1")
	if err := os.MkdirAll(filepath.Dir(first), 0o700); err != nil {
		t.Fatal(err)
	}
	if lead, ok := claimLead(dir, argv, first); !ok || lead != first {
		t.Fatalf("the first run does not lead: %s %v", lead, ok)
	}
	if err := os.WriteFile(first+".pid", []byte(strconv.Itoa(sleeper(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, ownedRuns, "pr-wait-o_r-7-2")
	if lead, ok := claimLead(dir, argv, second); ok || lead != first {
		t.Errorf("the second run does not follow the first: %s %v", lead, ok)
	}
	if _, ok := claimLead(dir, strings.Fields("devctl pr wait o/r 8"), second); !ok {
		t.Errorf("another pull request's wait follows")
	}
	writeResult(first, runResult{RC: 0, Doc: greenDoc})
	if lead, ok := claimLead(dir, argv, second); !ok || lead != second {
		t.Errorf("a finished leader is followed: %s %v", lead, ok)
	}
	releaseLead(dir, argv, second)
	if _, err := os.Stat(leadFile(dir, argv)); err == nil {
		t.Errorf("the lead file stays")
	}
}

// A wait of a command another run polls for runs no devctl of its own: it
// prints the leader's stderr and document and exits with its code.
func TestAWaitFollowsTheRunThatPolls(t *testing.T) {
	noSystemd(t)
	marker := filepath.Join(t.TempDir(), "devctl-ran")
	fakeDevctl(t, "touch "+marker+"; exit 9")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s2")
	a := queueApp(t)
	argv := strings.Fields("devctl pr wait o/r 7")
	lead := filepath.Join(a.store.Dir(), ownedRuns, "pr-wait-o_r-7-1")
	if err := os.MkdirAll(filepath.Dir(lead), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lead+".log", []byte("checks pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lead+".pid", []byte(strconv.Itoa(sleeper(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leadFile(a.store.Dir(), argv), []byte(lead), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		writeResult(lead, runResult{RC: 4, Doc: greenDoc})
	}()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = a.ownedRun(argv)
	os.Stdout = stdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if Code(err) != 4 || strings.TrimSpace(string(out)) != greenDoc {
		t.Errorf("exit %d, stdout %q; want the leader's 4 and document", Code(err), out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the follower ran devctl itself")
	}
}

func TestFollowRunCopiesTheLeadersStderr(t *testing.T) {
	lead := filepath.Join(t.TempDir(), "run")
	if err := os.WriteFile(lead+".log", []byte("checks pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeResult(lead, runResult{RC: 1, Doc: greenDoc, Kept: "/kept.log"})
	var out, errs bytes.Buffer
	if rc := followRun(lead, &out, &errs); rc != 1 || out.String() != greenDoc || !strings.HasPrefix(errs.String(), "checks pending\n") ||
		!strings.Contains(errs.String(), "/kept.log") {
		t.Errorf("rc %d, stdout %q, stderr %q", rc, out.String(), errs.String())
	}
}

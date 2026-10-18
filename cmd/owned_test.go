//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// ownerName is the owner of the tests' runs.
const ownerName = "worker"

const greenDoc = `{"verdict":"green","reason":"every check passed"}`

// A wait runs outside its caller with its document and exit code unchanged,
// and leaves the heard marker for its merge-child when its caller listens.
func TestAWaitRunsOwnedWithItsOutputUnchanged(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo "checks pending" >&2; echo '`+greenDoc+`'; exit 4`)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", ownerName)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}, store: store, now: relayNow}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	argv := strings.Fields("devctl pr wait o/r 7")
	err = a.ownedRun(context.Background(), argv, 0)
	os.Stdout = stdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	var e *exitError
	if !errors.As(err, &e) || e.code != 4 {
		t.Errorf("exit: %v, want 4", err)
	}
	if strings.TrimSpace(string(out)) != greenDoc {
		t.Errorf("stdout %q, want the document", out)
	}
	base := ownedBase(store.Dir(), argv, os.Getpid())
	if !strings.HasSuffix(base, "/runs/pr-wait-o_r-7-"+strconv.Itoa(os.Getpid())) {
		t.Errorf("base %s", base)
	}
	if _, err := os.Stat(heardFile(base, os.Getpid())); err != nil {
		t.Errorf("no heard marker: %v", err)
	}
	if _, err := os.Stat(base + ".rc"); err == nil {
		t.Errorf("the run's files are left")
	}
}

// tellOwnerRun is a finished wait of worker's whose gate is gone.
func tellOwnerRun(t *testing.T, owner state.Party) (*app, childResult, *[]string) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Session: "s1", Name: ownerName}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	infra := infraReds
	infraReds = func(context.Context, []byte) []merge.Infra { return nil }
	t.Cleanup(func() { infraReds = infra })
	var woke []string
	was := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, by state.Party, q, msg, _ string) error {
		woke = append(woke, by.Name+" → "+q+": "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = was })
	r := childResult{
		spec: childSpec{Argv: strings.Fields("/usr/bin/devctl pr wait o/r 7"), Owner: owner, Gate: 0},
		base: filepath.Join(store.Dir(), ownedRuns, "pr-wait-o_r-7-1"),
		rc:   0, doc: []byte(greenDoc), last: "checks pending", kept: "/k/pr-wait.log",
	}
	return &app{cfg: &config.Config{}, store: store, now: relayNow}, r, &woke
}

// A run whose gate is gone without the heard marker wakes its owner with
// one line and logs devctl.unheard; a heard one wakes nobody.
func TestAnUnheardRunWakesItsOwner(t *testing.T) {
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	a.tellOwner(context.Background(), r)
	want := `beekeeper gate → s1: devctl pr wait o/r 7 exit 0: green: every check passed (output in /k/pr-wait.log)`
	if len(*woke) != 1 || (*woke)[0] != want {
		t.Errorf("woke %q, want %q", *woke, want)
	}
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbUnheard })
	if len(evs) != 1 || !strings.HasSuffix(evs[0].Detail, `; waking "worker"`) {
		t.Errorf("events %+v", evs)
	}

	a, r, woke = tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	if err := os.MkdirAll(filepath.Dir(r.base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(heardFile(r.base, r.spec.Gate), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.tellOwner(context.Background(), r)
	if len(*woke) != 0 {
		t.Errorf("a heard run woke %q", *woke)
	}
	if _, err := os.Stat(heardFile(r.base, r.spec.Gate)); err == nil {
		t.Errorf("the heard marker is left")
	}

	a, r, woke = tellOwnerRun(t, state.Party{Session: "s9", Name: "a person's session"})
	a.tellOwner(context.Background(), r)
	evs, _ = a.store.Events(0, func(e state.Event) bool { return e.Verb == verbUnheard })
	if len(*woke) != 0 || len(evs) != 1 || !strings.Contains(evs[0].Detail, "is no registered agent, nobody is woken") {
		t.Errorf("an owner off the roster: woke %q, events %+v", *woke, evs)
	}
}

func TestRunReason(t *testing.T) {
	for _, c := range []struct{ doc, last, handCut, want string }{
		{greenDoc, "x", "", "green: every check passed"},
		{mergedDoc, "x", "", "merged, release v1.2.4"},
		{`{"mergeCommitSha":"abc","verdict":"merged","reason":"released"}`, "", "", "merged, release unknown; merged: released"},
		{`{"mergeCommitSha":"abc","verdict":"merged","release":null}`, "", "release-1.3",
			"merged, release none awaited, release-1.3 has no auto-release and its tags are cut by hand; merged"},
		{redDoc, "x", "", redReason},
		{"", "devctl: not found", "", "devctl: not found"},
		{"", "", "", "no output"},
	} {
		if got := runReason([]byte(c.doc), c.last, c.handCut); got != c.want {
			t.Errorf("runReason(%q, %q) = %q, want %q", c.doc, c.last, got, c.want)
		}
	}
}

// A second merge of a pull request whose merge runs is refused with exit 3
// and the running one's start, owner and last line.
func TestASecondMergeOfARunningOneIsRefused(t *testing.T) {
	lane := config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}}
	g := runningMerge(t, scratchRepo, lane)
	base, err := g.mergeFiles()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".log", []byte("merging\nwaiting for the release v1.2.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := *g
	second.pid, second.me = g.pid+1, state.Party{Name: "second"}
	_, err = second.step()
	var e *exitError
	if !errors.As(err, &e) || e.code != ExitGateDuplicate {
		t.Fatalf("got %v, want exit %d", err, ExitGateDuplicate)
	}
	d := lastEvent(t, g, "merge.refused")
	for _, want := range []string{"o/r#7 is already merging: started", `by "worker"`, "last line: waiting for the release v1.2.4", "do not merge again"} {
		if !strings.Contains(d, want) {
			t.Errorf("refusal %q lacks %q", d, want)
		}
	}
	if st := gateState(t, g); len(st.Merges) != 1 || st.Merges[0].PID != g.pid {
		t.Errorf("merges %+v", st.Merges)
	}
}

// A waiter stopped by a signal before its command settled wakes its owner
// with the stop, not with a merge outcome; a stopped merge with the state
// GitHub reports for its pull request; one whose command reported a merge
// before the stop still hands that merge over.
func TestAStoppedWaiterNamesTheStop(t *testing.T) {
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	r.rc, r.doc, r.stop = 128+int(syscall.SIGTERM), nil, syscall.SIGTERM
	a.tellOwner(context.Background(), r)
	want := `beekeeper gate → s1: devctl pr wait o/r 7 did not settle: its waiter was stopped by SIGTERM (exit 143) and nothing waits for it any more: check the pull request or release it waited for and run it again if it still needs to`
	if len(*woke) != 1 || (*woke)[0] != want {
		t.Errorf("woke %q, want %q", *woke, want)
	}

	a, r, woke = tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	stubPull(t, github.Pull{State: github.Merged, MergedAt: mergedAt}, nil)
	r.spec.Argv, r.spec.Command = strings.Fields("/usr/bin/beekeeper gate --queued -- devctl pr merge o/r 7"), strings.Fields("devctl pr merge o/r 7")
	r.rc, r.doc, r.stop = 128+int(syscall.SIGTERM), nil, syscall.SIGTERM
	a.tellOwner(context.Background(), r)
	if len(*woke) != 1 || (*woke)[0] != "beekeeper gate → s1: "+stoppedMerged {
		t.Errorf("woke %q, want %q", *woke, stoppedMerged)
	}
	if st, _ := a.store.Read(); len(st.Owed) != 0 {
		t.Errorf("a handed-over outcome stays owed: %+v", st.Owed)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbMerged }); len(evs) != 1 ||
		!strings.HasPrefix(evs[0].Detail, "o/r#7 at 2026-10-14T19:47:02Z per GitHub") {
		t.Errorf("merged events %+v", evs)
	}

	a, r, woke = tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	r.spec.Argv, r.rc, r.doc, r.stop = strings.Fields("/usr/bin/devctl pr merge o/r 7"), 0, []byte(mergedDoc), syscall.SIGTERM
	a.tellOwner(context.Background(), r)
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "exit 0: merged, release v1.2.4") {
		t.Errorf("a merge before the stop: woke %q", *woke)
	}
}

// A waiter whose owner left the roster while it waited, stopped or not,
// exits without a wake.
func TestAWaiterWhoseOwnerLeftWakesNoOne(t *testing.T) {
	for _, stop := range []syscall.Signal{0, syscall.SIGTERM} {
		a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Agents = nil
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		r.stop = stop
		a.tellOwner(context.Background(), r)
		evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbUnheard })
		if len(*woke) != 0 || len(evs) != 1 || !strings.Contains(evs[0].Detail, "is no registered agent, nobody is woken") {
			t.Errorf("stop %v: woke %q, events %+v", stop, *woke, evs)
		}
	}
}

// merge-child records the SIGTERM that stops it as the run's stop, passes it
// on to its command and keeps it across the re-exec of a replaced binary.
func TestMergeChildRecordsItsStop(t *testing.T) {
	fakeDevctl(t, `echo "queued" >&2; exec sleep 30`)
	base := filepath.Join(t.TempDir(), ownedRuns, "pr-merge-o_r-7-1")
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(childSpec{Argv: mergeArgv("o/r"), Env: os.Environ()})
	if err := os.WriteFile(base+".spec", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		for deadline := time.Now().Add(childStart); readPID(base) == 0 && time.Now().Before(deadline); {
			time.Sleep(followPoll)
		}
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	r := mergeChild(base)
	if r.stop != syscall.SIGTERM || r.rc != 128+int(syscall.SIGTERM) {
		t.Fatalf("stop %v, exit %d: want SIGTERM, 143", r.stop, r.rc)
	}
	if got := r.done().result(); got.stop != syscall.SIGTERM {
		t.Errorf("the re-exec's outcome lost the stop: %v", got.stop)
	}
}

// The gate's stdout carries devctl's document and nothing else, on every
// route a wait takes: owned, unowned (no state), and owned without its run
// files (the gate's own line). The gate's lines and devctl's progress go to
// stderr, so `<gated wait> | jq .` parses.
func TestTheGateWritesNothingOfItsOwnToStdout(t *testing.T) {
	const doc = `{"verdict":"available","reason":""}`
	for _, c := range []struct {
		name  string
		store func(t *testing.T) state.Store
		said  string // a line of the gate's own on stderr, "" for none required
	}{
		{name: "owned", store: func(t *testing.T) state.Store {
			s, err := state.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
		{name: "unowned", store: func(*testing.T) state.Store { return nil }},
		{name: "owned without its run files", store: func(t *testing.T) state.Store {
			s, err := state.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			// A file where the runs directory goes: the wait runs unowned.
			if err := os.WriteFile(filepath.Join(s.Dir(), ownedRuns), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return s
		}, said: GatePrefix},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSystemd(t)
			fakeDevctl(t, `echo "waiting for image gsoci.azurecr.io/giantswarm/r:1.0.0" >&2; echo '`+doc+`'`)
			t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
			t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
			t.Setenv("CLAUDE_CODE_SESSION_NAME", ownerName)
			a := &app{cfg: &config.Config{}, store: c.store(t), now: relayNow}
			stdout, stderr := capture(t, &os.Stdout), capture(t, &os.Stderr)
			err := a.gate(context.Background(), strings.Fields("devctl release wait o/r v1.0.0 --timeout 9m"), DefaultGateWait, 0, false)
			out, said := stdout(), stderr()
			if err != nil {
				t.Errorf("exit: %v, want 0", err)
			}
			if strings.TrimSpace(out) != doc || !json.Valid([]byte(out)) {
				t.Errorf("stdout %q, want devctl's document only", out)
			}
			if !strings.Contains(said, "waiting for image") || !strings.Contains(said, c.said) {
				t.Errorf("stderr %q, want devctl's progress and the gate's lines", said)
			}
		})
	}
}

// A red merge whose check died on a transient infrastructure failure wakes
// its owner naming the matched line and the job's rerun, not as the pull
// request's break alone.
func TestARedRunOnAnInfraFlakeWakesItsOwnerWithTheRerun(t *testing.T) {
	a, r, woke := tellOwnerRun(t, state.Party{Session: "s1", Name: ownerName})
	was := infraReds
	t.Cleanup(func() { infraReds = was })
	infraReds = func(_ context.Context, doc []byte) []merge.Infra {
		if !merge.Red(doc) {
			t.Errorf("infra read for a document that is not red")
		}
		return []merge.Infra{{Check: "go-lint", Repo: scratchRepo, Run: 11, Job: 12, Signature: "Go module proxy", Line: "stream error"}}
	}
	r.spec.Argv = strings.Fields("/usr/bin/devctl pr merge o/r 7")
	r.rc, r.doc = 1, []byte(`{"exitCode":1,"verdict":"red","reason":"check go-lint concluded failure","checks":[{"name":"go-lint","conclusion":"failure"}]}`)
	a.tellOwner(context.Background(), r)
	want := "beekeeper gate → s1: devctl pr merge o/r 7 exit 1: red: check go-lint concluded failure; go-lint: a transient infrastructure failure, not this pull request's breakage (Go module proxy: \"stream error\"): rerun the job (`gh run rerun 11 --job 12 --repo o/r`), then the same merge again (output in /k/pr-wait.log)"
	if len(*woke) != 1 || (*woke)[0] != want {
		t.Errorf("woke %q, want %q", *woke, want)
	}
}

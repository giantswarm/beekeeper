//go:build unix

package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
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
	err = a.ownedRun(argv)
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
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "devctl.unheard" })
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
	evs, _ = a.store.Events(0, func(e state.Event) bool { return e.Verb == "devctl.unheard" })
	if len(*woke) != 0 || len(evs) != 1 || !strings.Contains(evs[0].Detail, "is no registered agent, nobody is woken") {
		t.Errorf("an owner off the roster: woke %q, events %+v", *woke, evs)
	}
}

func TestRunReason(t *testing.T) {
	for _, c := range []struct{ doc, last, want string }{
		{greenDoc, "x", "green: every check passed"},
		{mergedDoc, "x", "merged, release v1.2.4"},
		{`{"mergeCommitSha":"abc","verdict":"merged","reason":"released"}`, "", "merged, release unknown; merged: released"},
		{"", "devctl: not found", "devctl: not found"},
		{"", "", "no output"},
	} {
		if got := runReason([]byte(c.doc), c.last); got != c.want {
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

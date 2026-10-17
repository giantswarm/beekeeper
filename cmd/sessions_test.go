package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

var (
	parkedWorker = state.Party{Session: "p1", Name: "Parked worker"}
	activeWorker = state.Party{Session: "a1", Name: "Active worker"}
)

// parkedState is a roster with a parked worker and an active one, each
// serving ref by its record; the parked worker's CLI is gone.
func parkedState(ref string) (*state.State, []*claude.Session) {
	st := &state.State{
		Agents: []state.Agent{
			{Party: parkedWorker, Task: "fix " + ref, Park: &state.Park{By: parkedWorker, On: "#12", Waits: "the person's answer"}},
			{Party: activeWorker, Task: "fix example/tool#7"},
		},
		Records: []state.Record{
			{Session: parkedWorker, Issue: ref, Waits: "the person's answer", Ended: relayNow.Add(-time.Hour)},
			{Session: activeWorker, Issue: "example/tool#7", Waits: "CI"},
		},
	}
	return st, []*claude.Session{{ID: "a1", Name: activeWorker.Name}}
}

func printedRecords(st *state.State, sessions []*claude.Session) string {
	var out bytes.Buffer
	a := &app{out: &out, now: relayNow}
	a.printRecords(st, st.Records, sessions)
	return out.String()
}

func TestSessionsNameAParkedWorkerServingAnIssue(t *testing.T) {
	st, sessions := parkedState("example/tool#5")
	got := printedRecords(st, sessions)
	want := `- "Parked worker" parked, no CLI, parked on #12: the person's answer; it serves example/tool#5`
	if !strings.Contains(got, want) {
		t.Errorf("records lack %q:\n%s", want, got)
	}
	if strings.Contains(got, "ended at") || strings.Contains(got, "re-query") {
		t.Errorf("a parked record reads as ended:\n%s", got)
	}
}

func TestSessionsNameAParkedWorkerServingAPullRequest(t *testing.T) {
	st, sessions := parkedState("example/tool#6")
	st.Records[0].Waits = "the merge ahead in the lane"
	got := printedRecords(st, sessions)
	want := `- "Parked worker" parked, no CLI, parked on #12: the person's answer; it serves example/tool#6, waiting on the merge ahead in the lane`
	if !strings.Contains(got, want) {
		t.Errorf("records lack %q:\n%s", want, got)
	}
	rows := parkedRows(st, st.Records, sessions, relayNow)
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"session":`, `"issue":"example/tool#6"`, `"waits":"the merge ahead in the lane"`, `"parked":"parked on #12: the person's answer"`, `"by":`, `"at":`} {
		if !strings.Contains(string(b), field) {
			t.Errorf("--json parked lacks %s: %s", field, b)
		}
	}
}

func TestSessionsKeepActiveRecordsUnchanged(t *testing.T) {
	st, sessions := parkedState("example/tool#5")
	got := printedRecords(st, sessions)
	if want := "- \"Active worker\" serves example/tool#7, waiting on CI\n"; !strings.HasPrefix(got, want) {
		t.Errorf("records start %q, want %q", got, want)
	}
	// A parked agent whose CLI runs is an active row: the table names it.
	st.Records[0].Ended = time.Time{}
	sessions = append(sessions, &claude.Session{ID: "p1", Name: parkedWorker.Name})
	if rows := parkedRows(st, st.Records, sessions, relayNow); len(rows) != 0 {
		t.Errorf("parked rows of running sessions: %+v", rows)
	}
	if got := printedRecords(st, sessions); !strings.Contains(got, `- "Parked worker" serves example/tool#5, waiting on the person's answer`) {
		t.Errorf("running parked worker's record changed:\n%s", got)
	}
}

func TestSessionsLeaveOutAParkedWorkerWhoseRecordWasReleased(t *testing.T) {
	st, sessions := parkedState("example/tool#5")
	st.Records = st.Records[1:]
	if rows := parkedRows(st, st.Records, sessions, relayNow); len(rows) != 0 {
		t.Errorf("parked rows without a record: %+v", rows)
	}
	if got := printedRecords(st, sessions); strings.Contains(got, "Parked worker") {
		t.Errorf("records name the released worker:\n%s", got)
	}
	// Done with its task, its old record is an ended one again.
	st, sessions = parkedState("example/tool#5")
	st.Agents[0].Done = true
	if got := printedRecords(st, sessions); !strings.Contains(got, `- "Parked worker" ended at`) {
		t.Errorf("a done worker's record is not ended:\n%s", got)
	}
}

// A guard that greps the listing from a session finds no row of that
// session: its own commands (the listing and the grep) are masked, in the
// table and in the changes, while another session's still show.
func TestSessionsMaskTheCallersOwnCommands(t *testing.T) {
	const guardName = "Guard"
	self := envParty(func(k string) string {
		return map[string]string{"CLAUDE_CODE_SESSION_ID": "s1", "CLAUDE_CODE_SESSION_NAME": guardName}[k]
	})
	guard := &claude.Session{ID: "s1", Name: guardName, Commands: []claude.Command{
		{Args: "/usr/bin/beekeeper sessions --all"}, {Args: "grep reclaim-disk"},
	}}
	other := &claude.Session{ID: "s2", Name: "Builder", Commands: []claude.Command{{Args: "beekeeper run -- make test"}}}
	named := &claude.Session{Name: guardName}
	if !isSelf(self, guard) || isSelf(self, other) || isSelf(state.Party{Name: guardName}, named) {
		t.Fatal("isSelf matches by the ids only")
	}
	v := &view{Sessions: []*sessionView{{Session: guard, Self: true}, {Session: other}}, st: &state.State{}}
	var out bytes.Buffer
	a := &app{out: &out}
	a.printSessions(v)
	for _, f := range a.sessionFacts(v) {
		out.WriteString(f.Sig + "\n" + f.Line + "\n")
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "reclaim-disk") || strings.Contains(l, "sessions --all") {
			t.Errorf("the caller's own commands show: %q", l)
		}
	}
	if !strings.Contains(out.String(), selfRunning) || !strings.Contains(out.String(), "beekeeper run -- make") {
		t.Errorf("want the caller's row marked and the other's command shown:\n%s", out.String())
	}
}

// A roster name holding a slash resolves whole, for a running agent whose
// desktop title is shorter and for one whose CLI does not run; a part of it
// still resolves, and the slash is never read as a separator.
func TestSessionNamedRosterNameWithSlash(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	const running, absent = "GMC 2754 violet/elver close", "GMC 2755 violet/elver open"
	st := &state.State{Agents: []state.Agent{
		{Party: state.Party{Session: "s1", Name: running}},
		{Party: state.Party{Session: "s2", Name: absent}},
	}}
	sessions := []*claude.Session{
		{PID: 7, ID: "s1", Name: "GMC 2754"},
		{PID: 8, ID: "s3", Name: "violet"},
	}
	for q, want := range map[string]string{running: "s1", strings.ToUpper(running): "s1", "GMC 2754": "s1", absent: "s2", "elver open": "s2"} {
		s, _, err := sessionNamed(sessions, st, q, now)
		if err != nil || s.ID != want {
			t.Errorf("sessionNamed(%q) = %v, %v; want session %s", q, s, err, want)
		}
	}
	if _, _, err := sessionNamed(sessions, st, "violet/elver", now); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Errorf("a part two roster names share: %v", err)
	}
}

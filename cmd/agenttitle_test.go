package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A session whose desktop record lost its name has a steward set its title,
// "self" when the steward is its own desktop CLI, and retitle waits for the
// desktop to record it; a record that kept the name is left alone.
func TestRetitle(t *testing.T) {
	const name, host = "test: title after turn", "local_retitled"
	const ownTo, selfArg = "uds:/s/42.sock", `"` + selfSession + `"`
	own := func(context.Context, []string) (steward, error) { return steward{host: host, sock: "/s/42.sock"}, nil }
	other := func(context.Context, []string) (steward, error) {
		return steward{host: "local_2", sock: "/s/43.sock"}, nil
	}
	for _, c := range []struct {
		desc    string
		titles  []string // the record's title, read by read
		find    stewardFinder
		sendErr error
		wantTo  string
		wantArg string
		want    string // a substring of the line or the error
		wantErr bool
	}{
		{desc: "kept", titles: []string{name}, find: own, want: "keeps its title"},
		{desc: "lost, then retitled", titles: []string{"", "", name}, find: own, wantTo: ownTo, wantArg: selfArg, want: "the session retitled it"},
		{desc: "retitled by another steward", titles: []string{"", name}, find: other, wantTo: "uds:/s/43.sock", wantArg: `"local_retitled"`, want: "steward local_2 retitled it"},
		{desc: "replaced, then retitled", titles: []string{"klaus-lab", name}, find: own, wantTo: ownTo, wantArg: selfArg, want: `"klaus-lab" instead of`},
		{desc: "no steward", titles: []string{""}, find: func(context.Context, []string) (steward, error) { return steward{}, errors.New("no idle desktop CLI") }, want: "no idle desktop CLI", wantErr: true},
		{desc: "send fails", titles: []string{""}, find: own, sendErr: errors.New("not sent"), wantTo: ownTo, wantArg: selfArg, want: "not sent", wantErr: true},
		{desc: "never recorded", titles: []string{""}, find: own, wantTo: ownTo, wantArg: selfArg, want: "did not record it", wantErr: true},
	} {
		t.Run(c.desc, func(t *testing.T) {
			var reads atomic.Int32
			title := func() string {
				i := int(reads.Add(1)) - 1
				return c.titles[min(i, len(c.titles)-1)]
			}
			var to, msg string
			send := func(_ context.Context, t, m string) error {
				to, msg = t, m
				return c.sendErr
			}
			line, err := retitle(context.Background(), host, name, title, c.find, send, 3*time.Second)
			got := line
			if err != nil {
				got = err.Error()
			}
			if (err != nil) != c.wantErr || !strings.Contains(got, c.want) {
				t.Errorf("retitle = %q, %v; want %q, error %v", line, err, c.want, c.wantErr)
			}
			if to != c.wantTo {
				t.Fatalf("sent to %q, want %q", to, c.wantTo)
			}
			if to != "" && (!strings.Contains(msg, "set_session_title") || !strings.Contains(msg, c.wantArg) || !strings.Contains(msg, `"`+name+`"`)) {
				t.Errorf("sent %q", msg)
			}
		})
	}
}

// A request a steward declines goes to the next one, up to stewardTries.
func TestDelegate(t *testing.T) {
	var asked []string
	done := false
	find := func(_ context.Context, tried []string) (steward, error) {
		if len(tried) == stewardTries {
			t.Fatal("asked past stewardTries")
		}
		return steward{host: fmt.Sprintf("steward-%d", len(tried)), sock: "/s"}, nil
	}
	send := func(context.Context, string, string) error {
		asked = append(asked, "x")
		done = len(asked) == 2 // the first declines
		return nil
	}
	s, err := delegate(context.Background(), find, func(steward) string { return "m" }, func() bool { return done }, send, 2*time.Second)
	if err != nil || s.host != "steward-1" || len(asked) != 2 {
		t.Errorf("delegate = %+v, %v after %d asks; want local_1 after 2", s, err, len(asked))
	}
	asked, done = nil, false
	send = func(context.Context, string, string) error { asked = append(asked, "x"); return nil }
	if _, err := delegate(context.Background(), find, func(steward) string { return "m" }, func() bool { return false }, send, time.Second); err == nil || len(asked) != stewardTries {
		t.Errorf("all decline: %v after %d asks", err, len(asked))
	}
}

// A steward is an idle desktop CLI of a session beekeeper started: the
// target's own first, else the one idle longest; never the operator's own
// session, a role holder, a busy agent, a headless CLI or one in a turn.
func TestPickSteward(t *testing.T) {
	const stewardA = "local_a"
	now := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)
	quiet, recent := now.Add(-10*time.Minute), now.Add(-5*time.Second)
	desktop := []string{claudeComm, "--output-format", "stream-json", permissionPromptTool, "stdio"}
	session := func(pid int, id string, active time.Time) *claude.Session {
		return &claude.Session{PID: pid, ID: id, HostID: "local_" + id, LastActive: active}
	}
	table := func(ss ...*claude.Session) *proc.Table {
		t := &proc.Table{ByPID: map[int]*proc.Process{}}
		for _, s := range ss {
			t.ByPID[s.PID] = &proc.Process{PID: s.PID, Comm: claudeComm, Args: desktop}
		}
		return t
	}
	started := func(ids ...string) *state.State {
		st := &state.State{}
		for _, id := range ids {
			st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
		}
		return st
	}
	sock := func(pid int) string { return fmt.Sprintf("/s/%d.sock", pid) }
	for _, c := range []struct {
		desc     string
		st       *state.State
		sessions []*claude.Session
		table    func(*proc.Table)
		tried    []string
		want     string // the steward's host, "" for none
	}{
		{desc: "the target's own", st: started("a", "t"), sessions: []*claude.Session{session(1, "a", quiet.Add(-time.Hour)), session(2, "t", quiet)}, want: "local_t"},
		{desc: "the one idle longest", st: started("a", "b"), sessions: []*claude.Session{session(1, "a", quiet), session(2, "b", quiet.Add(-time.Hour))}, want: "local_b"},
		{desc: "a finished worker before a roster agent", st: func() *state.State {
			st := started("a", "b")
			st.Agents = []state.Agent{{Party: state.Party{Session: "b"}}}
			return st
		}(), sessions: []*claude.Session{session(1, "a", quiet), session(2, "b", quiet.Add(-time.Hour))}, want: stewardA},
		{desc: "not one tried", st: started("a", "b"), sessions: []*claude.Session{session(1, "a", quiet), session(2, "b", quiet.Add(-time.Hour))}, tried: []string{"local_b"}, want: stewardA},
		{desc: "the operator's own session", st: started(), sessions: []*claude.Session{session(1, "a", quiet)}},
		{desc: "in a turn", st: started("a"), sessions: []*claude.Session{session(1, "a", recent)}},
		{desc: "a busy agent", st: func() *state.State {
			st := started("a")
			st.Agents = []state.Agent{{Party: state.Party{Session: "a"}, Task: "work"}}
			return st
		}(), sessions: []*claude.Session{session(1, "a", quiet)}},
		{desc: "the guide", st: func() *state.State {
			st := started("a")
			st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "a"}}}
			return st
		}(), sessions: []*claude.Session{session(1, "a", quiet)}},
		{desc: "a relieved supervisor", st: func() *state.State {
			st := started("a")
			st.Relieved = []state.Relief{{Party: state.Party{Session: "a"}}}
			return st
		}(), sessions: []*claude.Session{session(1, "a", quiet)}},
		{desc: "a headless CLI", st: started("a"), sessions: []*claude.Session{session(1, "a", quiet)}, table: func(t *proc.Table) {
			t.ByPID[1].Args = []string{claudeComm, "-p", resumeFlag, "a"}
		}},
	} {
		t.Run(c.desc, func(t *testing.T) {
			tb := table(c.sessions...)
			if c.table != nil {
				c.table(tb)
			}
			s, err := pickSteward(c.st, c.sessions, tb, "local_t", c.tried, now, sock)
			if s.host != c.want || (err != nil) != (c.want == "") {
				t.Errorf("pickSteward = %+v, %v; want %q", s, err, c.want)
			}
		})
	}
}

// A removed agent's desktop session stays when beekeeper did not start it
// or it keeps a role, also a role's run whose relief was forgotten.
func TestArchiveDesktopKeeps(t *testing.T) {
	a := &app{}
	ag := state.Party{Session: "a", Name: "worker"}
	archive := func(st *state.State, ag state.Party) string {
		return a.archiveDesktops(context.Background(), st, []state.Party{ag}, "test")[0].line
	}
	if got := archive(&state.State{}, ag); !strings.Contains(got, "did not start it") {
		t.Errorf("not started: %q", got)
	}
	st := &state.State{Starts: []state.Start{{Party: ag}}, Supervisor: &state.Supervisor{Party: ag}}
	if got := archive(st, ag); !strings.Contains(got, "role") {
		t.Errorf("supervisor: %q", got)
	}
	run := state.Party{Session: "r", Name: "Supervisor run 7"}
	if got := archive(&state.State{Starts: []state.Start{{Party: run}}}, run); !strings.Contains(got, "role") {
		t.Errorf("a role's run, its relief forgotten: %q", got)
	}
}

func TestPeerSocket(t *testing.T) {
	if got := peerSocket("/run/user/1000", 878093); got != "/run/user/1000/cc-socks/878093.sock" {
		t.Errorf("peerSocket = %q", got)
	}
}

// A reopen the desktop did not take ends the unit successfully and leaves
// its reason in the event log.
func TestAMissedReopenFailsNoUnit(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{store: store, out: &out}
	if err := a.reopenMissed("#1 task", errors.New("the desktop recorded no title")); err != nil {
		t.Fatalf("a missed reopen fails its unit: %v", err)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == "agent.reopen" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "no title") || evs[0].By.Name != "#1 task" {
		t.Errorf("events %+v, %v", evs, err)
	}
	if !strings.Contains(out.String(), "reopen: missed") {
		t.Errorf("printed %q", out.String())
	}
}

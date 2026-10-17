package cmd

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	untitledPID    = 1<<23 + 34
	unrosteredPID  = 1<<23 + 35
	untitledName   = "klaus-lab-34"
	rosterName     = "BK worker"
	unrosteredName = "klaus-lab-35"
	servesRoster   = `"BK worker" serves`
	servesPark     = `"BK park" serves`
)

// serveApp runs two sessions the desktop has not titled, named by their
// processes; the first is on the roster as rosterName.
func serveApp(t *testing.T) (*app, state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Session: "s" + strconv.Itoa(untitledPID), Name: rosterName}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir}
	cfg.Claude.SessionsDir, cfg.Claude.DesktopDir, cfg.Claude.ProjectsDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.Omp.SessionsDir = t.TempDir()
	a := &app{cfg: cfg, store: store, now: keepNow, out: &bytes.Buffer{}, as: testSupervisor}
	was := plat.Machine
	t.Cleanup(func() { plat.Machine = was })
	runLive(t, a, map[int]string{untitledPID: untitledName, unrosteredPID: unrosteredName})
	return a, store
}

func serve(a *app, args ...string) (string, error) {
	c := a.serveCmd()
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

// An untitled session on the roster serves under its roster name, in the
// record, the command's line and the records `sessions` prints; a session
// not on the roster keeps its process name.
func TestServeRecordsTheRosterName(t *testing.T) {
	a, store := serveApp(t)
	if out, err := serve(a, untitledName, "giantswarm/beekeeper#1027"); err != nil || !strings.Contains(out, `"BK worker" serves giantswarm/beekeeper#1027`) {
		t.Fatalf("serve the roster's session: %q, %v", out, err)
	}
	if out, err := serve(a, unrosteredName, "giantswarm/beekeeper#1026"); err != nil || !strings.Contains(out, `"klaus-lab-35" serves giantswarm/beekeeper#1026`) {
		t.Fatalf("serve the session off the roster: %q, %v", out, err)
	}
	st, _ := store.Read()
	if len(st.Records) != 2 || st.Records[0].Session.Name != rosterName || st.Records[1].Session.Name != unrosteredName {
		t.Fatalf("records %+v", st.Records)
	}
	sessions, _, err := a.sessions()
	if err != nil {
		t.Fatal(err)
	}
	a.out = &bytes.Buffer{}
	a.printRecords(st, st.Records, sessions)
	if s := a.out.(*bytes.Buffer).String(); !strings.Contains(s, `- "BK worker" serves giantswarm/beekeeper#1027`) || strings.Contains(s, untitledName) {
		t.Errorf("records:\n%s", s)
	}
}

// serve resolves its session by one rule: a session id, PID, the session's
// own name or a registered agent's roster name, whole before a part; a
// roster agent whose CLI does not run is recorded and named so; anything
// else is refused with what to name.
func TestServeResolvesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name, q, want, refusal string
	}{
		{name: "roster name", q: rosterName, want: servesRoster},
		{name: "roster name, any case", q: "bk WORKER", want: servesRoster},
		{name: "session name", q: untitledName, want: servesRoster},
		{name: "session id", q: "s" + strconv.Itoa(untitledPID), want: servesRoster},
		{name: "PID", q: strconv.Itoa(untitledPID), want: servesRoster},
		{name: "session off the roster", q: unrosteredName, want: strconv.Quote(unrosteredName) + " serves"},
		{name: "whole name before a part", q: "BK park", want: servesPark},
		{name: "roster agent without a CLI", q: "BK parked worker", want: `"BK parked worker" serves giantswarm/beekeeper#789; no CLI of "BK parked worker" runs`},
		{name: "part of several names", q: "klaus-lab", refusal: `"klaus-lab" matches 2 sessions: "klaus-lab-34" (s` + strconv.Itoa(untitledPID) + `), "klaus-lab-35"`},
		{name: "task title", q: "BK park (opus, builds)", want: servesPark},
		{name: "task title, any case", q: "bk PARK (Opus, Builds)", want: servesPark},
		{name: "roster name and a suffix", q: "BK parked worker (sonnet, no build)", want: `"BK parked worker" serves`},
		{name: "no match", q: nobodyNamed, refusal: `no running session or registered agent matches "nobody": name one by its session name, roster name or session id; the closest roster names: `},
		{name: "near miss", q: "BK wroker (opus, builds)", refusal: `no running session or registered agent matches "BK wroker (opus, builds)": name one by its session name, roster name or session id; the closest roster names: "BK worker", "BK park", "BK parked worker"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, store := serveApp(t)
			if err := store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = append(st.Agents,
					state.Agent{Party: state.Party{Session: "s-parked", Name: "BK parked worker"}},
					state.Agent{Party: state.Party{Session: "s-park", Name: "BK park"}, Task: "BK park (opus, builds): giantswarm/beekeeper#1333"})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			out, err := serve(a, tc.q, "giantswarm/beekeeper#789")
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("want the refusal %q, got %q, %v", tc.refusal, out, err)
				}
				if st, _ := store.Read(); len(st.Records) != 0 {
					t.Errorf("a refusal recorded %+v", st.Records)
				}
				return
			}
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("want %q, got %q, %v", tc.want, out, err)
			}
			if st, _ := store.Read(); len(st.Records) != 1 || st.Records[0].Issue != "giantswarm/beekeeper#789" {
				t.Errorf("records %+v", st.Records)
			}
		})
	}
}

// A worker's serve naming another session is refused, so its records never
// land on another session's name; serve --clear removes its own record and
// only that (giantswarm/beekeeper#772).
func TestServeAWorkerRecordsAndClearsOnlyItsOwn(t *testing.T) {
	const worker, issue = "BK 772 worker", "giantswarm/beekeeper#772"
	a, store := serveApp(t)
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: worker}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := serve(a, unrosteredName, issue); err != nil {
		t.Fatalf("the supervisor serves another session: %q, %v", out, err)
	}
	a.as, a.out = worker, &bytes.Buffer{}
	for _, q := range []string{rosterName, unrosteredName} {
		if out, err := serve(a, q, "giantswarm/beekeeper#1"); err == nil || !strings.Contains(err.Error(), "a worker records only its own") {
			t.Fatalf("a worker served %q: %q, %v", q, out, err)
		}
	}
	if out, err := serve(a, worker, issue); err != nil || !strings.Contains(out, `"BK 772 worker" serves `+issue) {
		t.Fatalf("a worker serves itself: %q, %v", out, err)
	}
	if _, err := serve(a, "--clear", worker); err == nil {
		t.Error("serve --clear took a session")
	}
	a.out = &bytes.Buffer{}
	if out, err := serve(a, "--clear"); err != nil || !strings.Contains(out, `cleared the record of "BK 772 worker": it serves `+issue) {
		t.Fatalf("serve --clear: %q, %v", out, err)
	}
	st, _ := store.Read()
	if len(st.Records) != 1 || st.Records[0].Session.Name != unrosteredName {
		t.Fatalf("records after --clear: %+v", st.Records)
	}
	a.out = &bytes.Buffer{}
	if out, err := serve(a, "--clear"); err != nil || !strings.Contains(out, `"BK 772 worker" has no record to clear`) {
		t.Fatalf("serve --clear again: %q, %v", out, err)
	}
}

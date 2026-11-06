package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// The window flags and a day, as the tests of spend, report and log name them.
const (
	sinceFlag = "--since"
	untilFlag = "--until"
	oct8      = "2026-10-08"
	oct9      = "2026-10-09"
)

// busyStore is a store whose state lock is always busy for Log: an event
// that gives up on a busy lock is lost, one that waits is recorded.
type busyStore struct{ state.Store }

func (busyStore) Log(...state.Event) error { return errors.New("the state lock is busy") }

// The log keeps every event, however old, and --since 30d returns every
// agents.start of the last 30 days, past the 50 the default -n shows.
func TestLogKeepsAgentsStartsForTheWindow(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	start := func(at time.Time, detail string) state.Event {
		return state.Event{At: at, By: state.Party{Name: supervisorName}, Verb: verbStart, Detail: detail}
	}
	evs := []state.Event{start(now.AddDate(0, 0, -400), "a year back")}
	for i := range 60 {
		evs = append(evs,
			start(now.AddDate(0, 0, -29).Add(time.Duration(i)*time.Hour), fmt.Sprintf("worker %d", i)),
			state.Event{At: now.Add(-time.Minute), Verb: "run.start"})
	}
	if err := store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	logged := func(args ...string) []state.Event {
		t.Helper()
		var out bytes.Buffer
		a := &app{store: store, out: &out, json: true, now: now}
		c := a.logCmd()
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatalf("log %v: %v", args, err)
		}
		var got []state.Event
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("log %v: %v in %q", args, err, out.String())
		}
		return got
	}
	if got := logged("--verb", "agents.start", sinceFlag, "30d"); len(got) != 60 || got[0].Detail != "worker 0" || got[59].Detail != "worker 59" {
		t.Errorf("--since 30d = %d starts (%+v), want all 60 of the window", len(got), got)
	}
	if got := logged("--verb", "agents.start", sinceFlag, "30d", "-n", "5"); len(got) != 5 || got[4].Detail != "worker 59" {
		t.Errorf("--since 30d -n 5 = %+v, want the latest 5", got)
	}
	if got := logged("--verb", "agents.start", "-n", "0"); len(got) != 61 || got[0].Detail != "a year back" {
		t.Errorf("-n 0 = %d starts, want 61 from a year back: nothing is trimmed", len(got))
	}
	if got := logged("--verb", "agents.start"); len(got) != 50 {
		t.Errorf("default = %d starts, want the latest 50", len(got))
	}
}

// --since and --until bound the window, a bare date at its 00:00 local time.
func TestLogSinceUntil(t *testing.T) {
	const problem = "agents.problem"
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	ev := func(at time.Time, verb, detail string) state.Event {
		return state.Event{At: at, Verb: verb, Detail: detail}
	}
	if err := store.Record(
		ev(day.Add(-time.Minute), problem, "the day before"),
		ev(day, problem, "midnight"),
		ev(day.Add(20*time.Hour+15*time.Minute), problem, "20:15"),
		ev(day.Add(21*time.Hour), "agents.start", "another verb"),
		ev(day.Add(24*time.Hour), problem, "the next day"),
	); err != nil {
		t.Fatal(err)
	}
	now := day.Add(36 * time.Hour)
	logged := func(args ...string) ([]string, error) {
		var out bytes.Buffer
		a := &app{store: store, out: &out, json: true, now: now}
		c := a.logCmd()
		c.SetArgs(args)
		c.SilenceErrors, c.SilenceUsage = true, true
		if err := c.Execute(); err != nil {
			return nil, err
		}
		var evs []state.Event
		if err := json.Unmarshal(out.Bytes(), &evs); err != nil {
			t.Fatalf("log %v: %v in %q", args, err, out.String())
		}
		var got []string
		for _, e := range evs {
			got = append(got, e.Detail)
		}
		return got, nil
	}
	for _, c := range []struct {
		verb, since, until, want string
	}{
		{problem, oct8, oct9, "[midnight 20:15]"},
		{problem, oct8, "", "[midnight 20:15 the next day]"},
		{problem, "", oct8, "[the day before]"},
		{problem, "20:15", oct9, "[20:15]"},
		{"", oct8, oct9, "[midnight 20:15 another verb]"},
	} {
		args := []string{"--verb=" + c.verb}
		if c.since != "" {
			args = append(args, sinceFlag, c.since)
		}
		if c.until != "" {
			args = append(args, untilFlag, c.until)
		}
		got, err := logged(args...)
		if err != nil || fmt.Sprint(got) != c.want {
			t.Errorf("log %v = %v, %v; want %s", args, got, err, c.want)
		}
	}
	if _, err := logged(sinceFlag, oct9, untilFlag, oct8); Code(err) != ExitUsage {
		t.Errorf("--since after --until = %v, want a usage error", err)
	}
}

// --since takes days as well as Go durations.
func TestReportTimeTakesDays(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if got, err := reportTime(now, "30d", time.UTC); err != nil || !got.Equal(now.AddDate(0, 0, -30)) {
		t.Errorf("30d = %s, %v", got, err)
	}
	if _, err := reportTime(now, "-3d", time.UTC); Code(err) != ExitUsage {
		t.Errorf("-3d = %v, want a usage error", err)
	}
}

func TestLogAddRefusesUnlinkedRefs(t *testing.T) {
	a, out := noteApp(t)
	run := func(text string) error {
		c := a.logCmd()
		c.SetArgs([]string{strings.Fields("add x")[0], text})
		c.SetOut(out)
		c.SetErr(out)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	for name, c := range map[string]struct{ text, want string }{
		"unlinked #N":             {"merged #7", "#7 without its full URL (https://github.com/<owner>/<repo>/issues/7)"},
		"owner/repo#N":            {"merged x/y#7", "x/y#7 without its full URL (https://github.com/x/y/issues/7)"},
		"bare #N with single URL": {"merged " + prURL + " for #9", "#9 without its full URL (https://github.com/o/r/issues/9)"},
	} {
		if err := run(c.text); Code(err) != ExitRefused || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want refusal with %q", name, err, c.want)
		}
	}
	ok := "merged o/r#7 (" + prURL + "#issuecomment-1), note #3, timer #4, `#5`, `multi\n#6`, color: #123; background-color: #123456; and session beekeeper#12\n````md\n```\n#8\n```\n````"
	if err := run(ok); err != nil {
		t.Fatalf("log add(%q): %v", ok, err)
	}
	evs, err := a.store.Events(1, func(e state.Event) bool { return e.Verb == testStatusWord })
	if err != nil || len(evs) != 1 || evs[0].Detail != ok {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}

func TestLogDetailOutput(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	details := []string{
		strings.Repeat("report detail 界 ", 40) + "\nnext\tline\nfinal report detail",
		"second report\nwith another line",
	}
	for _, detail := range details {
		if err := store.Record(state.Event{At: now, Verb: "agents.report", By: state.Party{Name: workerRole}, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Record(state.Event{At: now, Verb: "agents.problem", Detail: "excluded problem"}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{litPlain, "short", "json", "json-short"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			a := &app{store: store, out: &out, now: now, json: strings.Contains(mode, "json")}
			c := a.logCmd()
			args := []string{"--verb", "agents.report"}
			if strings.Contains(mode, "short") {
				args = append(args, "--short")
			}
			c.SetArgs(args)
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			if a.json {
				var events []state.Event
				if err := json.Unmarshal(out.Bytes(), &events); err != nil {
					t.Fatal(err)
				}
				if len(events) != len(details) {
					t.Fatalf("got %d events, want %d", len(events), len(details))
				}
				for i, e := range events {
					if e.Detail != details[i] {
						t.Errorf("event %d detail = %q, want %q", i, e.Detail, details[i])
					}
				}
				return
			}
			if strings.Contains(out.String(), "excluded problem") {
				t.Fatal("verb filter included a problem")
			}
			if mode == "short" {
				lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
				if len(lines) != len(details) {
					t.Fatalf("got %d lines, want %d: %q", len(lines), len(details), out.String())
				}
				for i, line := range lines {
					if !strings.Contains(line, "agents.report") || !strings.HasSuffix(line, truncate(details[i], 100)) {
						t.Errorf("short entry %d = %q", i, line)
					}
				}
				return
			}
			for _, detail := range details {
				if !strings.Contains(out.String(), "  "+strings.ReplaceAll(detail, "\n", "\n  ")+"\n") {
					t.Errorf("plain output lost detail: %q", out.String())
				}
			}
		})
	}
}

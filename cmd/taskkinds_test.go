package cmd

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	kindFiler  = "filer"
	kindMeta   = "meta"
	filerOne   = "filer 1"
	filerTwo   = "filer 2"
	scanBoard  = "scan board"
	sliceAgent = "beekeeper 985"
	tzUTC      = "UTC"
)

const intervalKey = "interval"

const availableWord = "available"

// kindsApp is a stub app with a filer kind (one busy, one start an hour)
// and a meta kind over filers and scans (two busy).
func kindsApp(t *testing.T) *app {
	t.Helper()
	a, _ := stubApp(t)
	a.cfg.Agents.Kinds = []config.AgentKind{
		{Name: kindFiler, Match: "^filer", MaxBusy: 1, MinInterval: config.Duration{Duration: time.Hour}},
		{Name: kindMeta, Match: "^(filer|scan)", MaxBusy: 2},
	}
	return a
}

// recordKindStart records name started at, busy with a task, as every
// harness's start records one.
func recordKindStart(t *testing.T, a *app, name, session string, at time.Time) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, err := recordStart(st, state.Start{Party: state.Party{Session: session, Name: name}, At: at}, "a task", "", func(state.Party) bool { return false })
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKindGateRefusesInsideTheIntervalAndAtTheBusyCap(t *testing.T) {
	a := kindsApp(t)
	if err := a.kindGate(filerOne); err != nil {
		t.Fatalf("a first filer: %v", err)
	}
	recordKindStart(t, a, filerOne, "s-filer-1", a.now.Add(-10*time.Minute))
	err := a.kindGate(filerTwo)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitRefused {
		t.Fatalf("a second filer inside its interval and past its cap: %v", err)
	}
	for _, want := range []string{"no start filer: 1 busy (filer 1) at maxBusy 1", "last start filer 1 10m", "minInterval 1h"} {
		if !strings.Contains(ee.msg, want) {
			t.Errorf("refusal %q lacks %q", ee.msg, want)
		}
	}
	if strings.Contains(ee.msg, "no start meta") {
		t.Errorf("meta holds one of two: %q", ee.msg)
	}
	if err := a.kindGate(scanBoard); err != nil {
		t.Errorf("a scan under meta's cap: %v", err)
	}
	if err := a.kindGate(sliceAgent); err != nil {
		t.Errorf("an agent of no kind: %v", err)
	}

	// The filer is done: its busy cap frees, its interval still holds.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[len(st.Agents)-1].Done = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.kindGate(filerTwo); err == nil || strings.Contains(err.Error(), busyWord) || !strings.Contains(err.Error(), "minInterval") {
		t.Errorf("done filer inside the interval: %v", err)
	}
	a.now = a.now.Add(time.Hour)
	if err := a.kindGate(filerTwo); err != nil {
		t.Errorf("a filer after its interval: %v", err)
	}

	// Two busy meta workers hold every scan.
	recordKindStart(t, a, scanBoard, "s-scan", a.now.Add(-time.Minute))
	recordKindStart(t, a, "scan inbox", "s-scan-2", a.now.Add(-time.Minute))
	if err := a.kindGate("scan other"); err == nil || !strings.Contains(err.Error(), "no start meta: 2 busy (scan board, scan inbox) at maxBusy 2") {
		t.Errorf("meta at its cap: %v", err)
	}
}

// Concurrent starts of a kind pass its cap one at a time: the gate and the
// start's record happen under the kind lock.
func TestKindAdmissionIsAtomic(t *testing.T) {
	a := kindsApp(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			errs[i] = a.withKindAdmission(t.Context(), filerOne, func() error {
				time.Sleep(20 * time.Millisecond) // a launch before the record
				recordKindStart(t, a, filerOne, fmt.Sprintf("s-%d", i), a.now)
				return nil
			})
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case Code(err) != ExitRefused:
			t.Errorf("unexpected %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent filers admitted: %v", ok, len(errs), errs)
	}
}

func TestAgentStartRefusesAKindAtItsCap(t *testing.T) {
	a := kindsApp(t)
	recordKindStart(t, a, filerOne, "s-filer-1", a.now.Add(-5*time.Minute))
	brief := filepath.Join(t.TempDir(), "brief.md")
	writeFile(t, brief, "# file the findings\n")
	for _, flag := range []string{"--force", testDryRunFlag} {
		c := a.agentStartCmd()
		c.SilenceErrors, c.SilenceUsage = true, true
		c.SetArgs(testCheckoutArgs(t, filepath.Dir(brief), []string{filerTwo, brief, usageDirFlag, filepath.Dir(brief), flag}))
		c.SetContext(t.Context())
		err := c.Execute()
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start filer:") {
			t.Fatalf("agents start %s of a second filer: %v", flag, err)
		}
	}
	st, rerr := a.store.Read()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(st.Starts) != 1 {
		t.Errorf("the refused start was recorded: %+v", st.Starts)
	}
}

func TestSSHAgentStartKindAdmission(t *testing.T) {
	for _, limit := range []string{intervalKey, busyWord, availableWord} {
		for _, dry := range []bool{false, true} {
			if limit == availableWord && !dry {
				continue
			}
			for _, force := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/dry=%t/force=%t", limit, dry, force), func(t *testing.T) {
					a, brief := sshStartTestApp(t)
					a.cfg.Capacity.Providers = map[string]config.Provider{claudeComm: {Ceiling: 10}}
					stubSubscriptionUsage(a)
					k := config.AgentKind{Name: kindFiler, Match: "^filer"}
					switch limit {
					case intervalKey:
						k.MinInterval = config.Duration{Duration: time.Hour}
					case busyWord:
						k.MaxBusy = 1
					}
					a.cfg.Agents.Kinds = []config.AgentKind{k}
					if limit != availableWord {
						recordKindStart(t, a, filerOne, "s-filer-1", a.now.Add(-5*time.Minute))
					}
					a.executionTransport = func(config.ExecutionHost) execution.Transport {
						t.Fatal("kind refusal or dry run reached SSH transport")
						return nil
					}
					args := []string{agentStartName, filerTwo, brief, loop.LoopHostFlag, sshTestSelector, loop.LoopRepoFlag, sshTestRepository, loop.LoopRefFlag, mainBranch, modelFlag, bgModel}
					if dry {
						args = append(args, testDryRunFlag)
					}
					if force {
						args = append(args, usageForceFlag)
					}
					out, err := runAgents(a, args...)
					wantStarts := 1
					if limit == availableWord {
						wantStarts = 0
						if err != nil || !strings.Contains(out, "dry run:") {
							t.Fatalf("admitted dry run: %q, %v", out, err)
						}
					} else {
						want := "minInterval"
						if limit == busyWord {
							want = "maxBusy"
						}
						if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start filer:") || !strings.Contains(err.Error(), want) || strings.Contains(out, "dry run:") {
							t.Fatalf("kind admission: %q, %v", out, err)
						}
					}
					st, err := a.store.Read()
					if err != nil {
						t.Fatal(err)
					}
					if len(st.Starts) != wantStarts || len(st.Agents) != wantStarts {
						t.Fatal("kind refusal or dry run changed reservations")
					}
				})
			}
		}
	}
}

func TestCapacityNamesTheKindsAtACap(t *testing.T) {
	a := kindsApp(t)
	recordKindStart(t, a, filerOne, "s-filer-1", a.now.Add(-5*time.Minute))
	out := a.out.(interface{ String() string })
	c := a.capacityCmd()
	c.SetArgs(nil)
	c.SetContext(t.Context())
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\nno start filer: 1 busy (filer 1) at maxBusy 1; last start filer 1 5m") || strings.Contains(out.String(), "no start meta") {
		t.Errorf("capacity:\n%s", out.String())
	}
}

func TestKindSpendSumsPerKind(t *testing.T) {
	a := kindsApp(t)
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	recordKindStart(t, a, filerOne, "s-filer", from.Add(9*time.Hour))
	recordKindStart(t, a, scanBoard, "s-scan", from.Add(10*time.Hour))
	recordKindStart(t, a, filerTwo, "s-yesterday", from.Add(-time.Hour))
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Metrics = config.Metrics{Models: map[string]config.Model{"m": {Input: 1}}}
	req := func(id string, h float64, mtok int64) claude.SpendRequest {
		return claude.SpendRequest{ID: id, Model: "m", At: from.Add(time.Duration(h * float64(time.Hour))), Tokens: claude.Tokens{Input: mtok * 1_000_000}}
	}
	ts := []claude.SpendTranscript{
		{Session: "s-filer", Requests: []claude.SpendRequest{req("f1", 9, 2), req("f2", 10.5, 1)}},
		{Session: "s-filer", Subagent: true, Requests: []claude.SpendRequest{req("f3", 10, 1)}},
		{Session: "s-scan", Requests: []claude.SpendRequest{req("c1", 10, 3), req("c2", 10.5, 1)}},
		{Session: "s-person", Requests: []claude.SpendRequest{req("p1", 11, 5)}},
		{Requests: []claude.SpendRequest{req("o1", 12, 7)}},
	}
	r := kindSpend(a.cfg, st, ts, from, to)
	want := map[string]kindDay{
		kindFiler: {Starts: 1, Sessions: 1, Hours: 1.5, USD: 4},
		kindMeta:  {Starts: 2, Sessions: 2, Hours: 2, USD: 8},
	}
	if len(r.Kinds) != 2 {
		t.Fatalf("kinds %+v", r.Kinds)
	}
	for _, d := range r.Kinds {
		w := want[d.Kind]
		if d.Starts != w.Starts || d.Sessions != w.Sessions || math.Abs(d.Hours-w.Hours) > 1e-9 || math.Abs(d.USD-w.USD) > 1e-9 {
			t.Errorf("%s = %+v, want %+v", d.Kind, d, w)
		}
	}
	if math.Abs(r.Unattributed-12) > 1e-9 {
		t.Errorf("unattributed $%.2f, want $12", r.Unattributed)
	}
	text := kindReportText(r, time.UTC)
	for _, line := range []string{"  filer: 1 starts, 1 sessions, 1.5h, $4.00\n", "  meta: 2 starts, 2 sessions, 2.0h, $8.00\n", "  of no kind or no recorded session: $12.00\n"} {
		if !strings.Contains(text, line) {
			t.Errorf("report lacks %q:\n%s", line, text)
		}
	}
}

// sessions --by kind reads the day's transcripts in full and prints the
// kinds; any other --by is a usage error.
func TestSessionsByKind(t *testing.T) {
	a := kindsApp(t)
	a.cfg.Metrics = config.Metrics{Models: map[string]config.Model{"m": {Input: 1}}}
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	a.zone = func() (*time.Location, error) { return time.UTC, nil }
	recordKindStart(t, a, filerOne, "11111111-aaaa", day.Add(8*time.Hour))
	p := filepath.Join(a.cfg.Claude.ProjectsDir, "proj", "11111111-aaaa.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, `{"type":"assistant","timestamp":"2026-11-02T08:00:00Z","message":{"id":"r1","model":"m","usage":{"input_tokens":2000000}}}`+"\n"+
		`{"type":"assistant","timestamp":"2026-11-02T09:00:00Z","message":{"id":"r2","model":"m","usage":{"input_tokens":1000000}}}`+"\n")
	c := a.sessionsCmd()
	c.SetArgs([]string{"--by", paramKind, "--day", "2026-11-02", tzFlag, tzUTC})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	out := a.out.(interface{ String() string }).String()
	if !strings.Contains(out, "2026-11-02T00:00:00Z to 2026-11-03T00:00:00Z") || !strings.Contains(out, "  filer: 1 starts, 1 sessions, 1.0h, $3.00\n") {
		t.Errorf("sessions --by kind:\n%s", out)
	}
	c = a.sessionsCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs([]string{"--by", "bogus"})
	if err := c.Execute(); Code(err) != ExitUsage {
		t.Errorf("--by bogus: %v", err)
	}
}

// The watch says SPEND <kind> once a day past its dailySpend, and a
// restarted watch keeps its mark.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWatchSaysKindSpendOnceADay(t *testing.T) {
	a := kindsApp(t)
	a.cfg.Agents.Kinds[0].DailySpend = 2
	a.cfg.Metrics = config.Metrics{Models: map[string]config.Model{"m": {Input: 1}}}
	a.zone = func() (*time.Location, error) { return time.UTC, nil }
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	recordKindStart(t, a, filerOne, "22222222-bbbb", now.Add(-2*time.Hour))
	p := filepath.Join(a.cfg.Claude.ProjectsDir, "proj", "22222222-bbbb.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, `{"type":"assistant","timestamp":"2026-11-03T10:00:00Z","message":{"id":"r1","model":"m","usage":{"input_tokens":3000000}}}`+"\n")
	if err := os.Chtimes(p, now, now); err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	a.out = out
	w := a.newWatcher(false, false)
	w.KindSpending(now)
	w.KindSpending(now.Add(time.Minute))
	if n := strings.Count(out.String(), "SPEND filer: $3.00 today"); n != 1 {
		t.Fatalf("SPEND said %d times:\n%s", n, out)
	}
	if strings.Contains(out.String(), "SPEND meta") {
		t.Errorf("meta has no dailySpend:\n%s", out)
	}
	if !w.Reported[kindSpendKey(kindFiler, now.Truncate(24*time.Hour))] {
		t.Errorf("no mark for the day: %v", w.Reported)
	}
	// The next day forgets the mark of the last.
	w.KindSpending(now.Add(24 * time.Hour))
	if w.Reported[kindSpendKey(kindFiler, now.Truncate(24*time.Hour))] {
		t.Errorf("yesterday's mark kept: %v", w.Reported)
	}
}

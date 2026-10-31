package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

type tightMachine struct{ platform.Machine }

func (tightMachine) Mem() (machine.Mem, error) {
	return machine.Mem{AvailableMiB: 10 << 10, TotalMiB: 96 << 10}, nil
}

const (
	usageDirFlag   = "--dir"
	usageForceFlag = "--force"
	usageVerbStart = "agents.start"
	usageCaller    = "usage-sup"
)

func isolateUsageTestEnv(t *testing.T) (dir, cfgPath string) {
	t.Helper()
	orig := plat
	stub := platform.Stub()
	stub.Machine = roomyMachine{Machine: stub.Machine}
	plat = stub
	t.Cleanup(func() { plat = orig })

	dir = t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	t.Setenv("HOME", dir)
	t.Setenv("BEEKEEPER_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("MEMCAP_STATE", filepath.Join(dir, "memcap"))
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return dir, cfgPath
}

// TestRunUsageCommandAndCaching exercises a real executable usage-reader test
// double: stdout JSON parsing, non-zero exit + stderr reporting, missing
// configured window rejection, usage.json reuse within capacity.usage.every,
// and immediate re-read once capacity.usage.every elapses or resetsAt passes.
func TestRunUsageCommandAndCaching(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)

	outFile := filepath.Join(dir, "payload.json")
	errFile := filepath.Join(dir, "stderr.txt")
	codeFile := filepath.Join(dir, "exitcode")
	callsFile := filepath.Join(dir, "calls")
	reader := filepath.Join(dir, "read-usage")
	script := `#!/bin/sh
echo x >> "` + callsFile + `"
if [ -f "` + errFile + `" ]; then
  cat "` + errFile + `" >&2
fi
if [ -f "` + codeFile + `" ]; then
  exit $(cat "` + codeFile + `")
fi
cat "` + outFile + `"
`
	if err := os.WriteFile(reader, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	countCalls := func() int {
		b, _ := os.ReadFile(callsFile) //nolint:gosec // the test's own file
		return bytes.Count(b, []byte("x\n"))
	}

	rawCfg := fmt.Sprintf(`capacity:
  usage:
    command: [%q]
    every: 5m
    timeout: 5s
    fiveHour:
      - {upTo: 50, floor: 5, ceiling: 10}
      - {upTo: 85, floor: 2, ceiling: 4}
    sevenDay:
      - {upTo: 75, floor: 5, ceiling: 10}
      - {upTo: 90, floor: 1, ceiling: 3}
`, reader)
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := keepNow
	reset5h := t0.Add(2 * time.Minute).UTC()
	reset7d := t0.Add(72 * time.Hour).UTC()
	writePayload := func(s string) {
		t.Helper()
		if err := os.WriteFile(outFile, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Valid reading writes usage.json and derives floor 2, ceiling 4.
	writePayload(fmt.Sprintf(`{"fiveHour":{"usedPercent":62.5,"resetsAt":%q},"sevenDay":{"usedPercent":40,"resetsAt":%q}}`,
		reset5h.Format(time.RFC3339), reset7d.Format(time.RFC3339)))
	a := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &bytes.Buffer{}}
	k, u := a.resolveUsage(context.Background(), a.liveCapacity())
	if u == nil || u.Error != "" || k.Floor != 2 || k.Ceiling != 4 || u.High || countCalls() != 1 {
		t.Fatalf("first resolve = (%d,%d), u=%+v, calls=%d", k.Floor, k.Ceiling, u, countCalls())
	}

	// 2. Within every (1m < 5m) and before reset5h (2m), usage.json is reused without running reader.
	a.now = t0.Add(time.Minute)
	k, u = a.resolveUsage(context.Background(), a.liveCapacity())
	if u == nil || u.Error != "" || k.Floor != 2 || k.Ceiling != 4 || countCalls() != 1 {
		t.Fatalf("cached resolve = (%d,%d), u=%+v, calls=%d", k.Floor, k.Ceiling, u, countCalls())
	}

	// 3. Crossing reset5h (2m10s < 5m) invalidates usage.json and re-runs reader immediately.
	nextReset5h := reset5h.Add(5 * time.Hour)
	writePayload(fmt.Sprintf(`{"fiveHour":{"usedPercent":4,"resetsAt":%q},"sevenDay":{"usedPercent":80,"resetsAt":%q}}`,
		nextReset5h.Format(time.RFC3339), reset7d.Format(time.RFC3339)))
	a.now = t0.Add(2*time.Minute + 10*time.Second)
	k, u = a.resolveUsage(context.Background(), a.liveCapacity())
	if u == nil || u.Error != "" || k.Floor != 1 || k.Ceiling != 3 || countCalls() != 2 {
		t.Fatalf("after 5h reset = (%d,%d), u=%+v, calls=%d; want (1,3) and 2 calls", k.Floor, k.Ceiling, u, countCalls())
	}

	// 4. Advancing past every (5m) re-runs reader; non-zero exit + stderr surfaces in u.Error and blocks capacity.
	if err := os.WriteFile(errFile, []byte("upstream 503\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codeFile, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(6 * time.Minute)
	var capOut bytes.Buffer
	a.out = &capOut
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if countCalls() != 3 || !strings.Contains(capOut.String(), "usage unknown (") ||
		!strings.Contains(capOut.String(), "upstream 503") ||
		!strings.Contains(capOut.String(), "no start: subscription usage unknown (") {
		t.Fatalf("error capacity output (calls=%d):\n%s", countCalls(), capOut.String())
	}

	// 5. Omitting a window whose bands are configured is rejected cleanly.
	_ = os.Remove(errFile)
	_ = os.Remove(codeFile)
	writePayload(`{"fiveHour":{"usedPercent":12}}`)
	_, u = a.resolveUsage(context.Background(), a.liveCapacity())
	if u == nil || !strings.Contains(u.Error, "omits sevenDay") {
		t.Fatalf("missing sevenDay error = %+v", u)
	}

	// 6. Inside the agent sandbox, a cached host reading within 2 × every is
	//    kept when the reader cannot run inside the sandbox.
	writePayload(fmt.Sprintf(`{"fiveHour":{"usedPercent":30,"resetsAt":%q},"sevenDay":{"usedPercent":40,"resetsAt":%q}}`,
		nextReset5h.Format(time.RFC3339), reset7d.Format(time.RFC3339)))
	if k, u = a.resolveUsage(context.Background(), a.liveCapacity()); u.Error != "" || k.Floor != 5 || k.Ceiling != 10 {
		t.Fatalf("seed host reading = (%d,%d), u=%+v", k.Floor, k.Ceiling, u)
	}
	if err := os.WriteFile(codeFile, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.Env, "1")
	t.Setenv(sandbox.Brokered, "")
	a.now = a.now.Add(6 * time.Minute) // > every (5m), <= 2 × every (10m)
	if k, u = a.resolveUsage(context.Background(), a.liveCapacity()); u == nil || u.Error != "" || k.Floor != 5 || k.Ceiling != 10 {
		t.Fatalf("sandbox grace fallback = (%d,%d), u=%+v", k.Floor, k.Ceiling, u)
	}
}

// TestCapacityUsageBandsJSONAndStartGate covers capacity text + --json, agents
// start refusal & --force override under USAGE HIGH, queued --builds re-check,
// status, and handover prompt capacity line.
func TestCapacityUsageBandsJSONAndStartGate(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `capacity:
  usage:
    command: ["/bin/true"]
    every: 5m
    highPercent: 95
    fiveHour:
      - {upTo: 50, floor: 4, ceiling: 8}
      - {upTo: 85, floor: 2, ceiling: 3}
      - {upTo: 95, floor: 0, ceiling: 1}
    sevenDay:
      - {upTo: 70, floor: 4, ceiling: 8}
      - {upTo: 90, floor: 1, ceiling: 2}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{tasked(busyOne)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	t0 := keepNow
	reset5h := t0.Add(90 * time.Minute).UTC()
	reset7d := t0.Add(96 * time.Hour).UTC()
	cur := usageReading{
		FiveHour: &usageWindow{UsedPercent: 60, ResetsAt: reset5h},
		SevenDay: &usageWindow{UsedPercent: 75, ResetsAt: reset7d},
	}
	reader := func(_ context.Context, _ []string, _ time.Duration) (usageReading, error) {
		return cur, nil
	}

	openApp := func(now time.Time, jsonOut bool) (*app, *bytes.Buffer) {
		var out bytes.Buffer
		return &app{
			cfgPath:   cfgPath,
			cfg:       cfg,
			store:     store,
			now:       now,
			out:       &out,
			as:        agentOne,
			json:      jsonOut,
			readUsage: reader,
		}, &out
	}

	// 5h 60% -> (2, 3); 7d 75% -> (1, 2) => derived (1, 2); 1 busy => room for 1 start.
	aText, outText := openApp(t0, false)
	if err := aText.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if got := outText.String(); !strings.Contains(got, "capacity: 1 busy of floor 1, ceiling 2: W 1;") ||
		!strings.Contains(got, "; usage 5h 60% (resets ") ||
		!strings.Contains(got, ", 7d 75% (resets ") ||
		!strings.Contains(got, "\nroom for 1 start\n") {
		t.Fatalf("capacity text:\n%s", got)
	}

	aJSON, outJSON := openApp(t0, true)
	if err := aJSON.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Floor   int        `json:"floor"`
		Ceiling int        `json:"ceiling"`
		Room    int        `json:"room"`
		Verdict string     `json:"verdict"`
		Usage   *usageView `json:"usage"`
	}
	if err := json.Unmarshal(outJSON.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal capacity --json: %v\n%s", err, outJSON.String())
	}
	if parsed.Floor != 1 || parsed.Ceiling != 2 || parsed.Room != 1 || parsed.Usage == nil ||
		parsed.Usage.FiveHour == nil || parsed.Usage.FiveHour.UsedPercent != 60 ||
		parsed.Usage.SevenDay == nil || parsed.Usage.SevenDay.UsedPercent != 75 || parsed.Usage.High {
		t.Fatalf("capacity --json = %+v (usage %+v)", parsed, parsed.Usage)
	}

	// status and promptCapacity pick up the cached usage.json without running a subprocess.
	stView, err := aText.status()
	if err != nil || stView.Floor != 1 || stView.Ceiling != 2 {
		t.Fatalf("status = %+v, %v; want floor 1, ceiling 2", stView, err)
	}
	stNow, _ := store.Read()
	var promptBuf bytes.Buffer
	aText.promptCapacity(func(f string, args ...any) { _, _ = fmt.Fprintf(&promptBuf, f+"\n", args...) }, stNow, nil)
	if !strings.Contains(promptBuf.String(), "1 busy of floor 1, ceiling 2") || !strings.Contains(promptBuf.String(), "; usage 5h 60%") {
		t.Fatalf("promptCapacity:\n%s", promptBuf.String())
	}

	// Queued agents start --builds re-checks usage when its build slot turn arrives.
	tHigh := t0.Add(10 * time.Minute)
	aQueue, _ := openApp(t0, false)
	vBusySlot := newCapacity(agentCount{}, config.Capacity{Floor: 1, Ceiling: 2}, &headroom{AvailableMiB: 45 << 10, AvailMinMiB: 20 << 10, Slots: 1, SlotsFree: 0, MaxLabs: 2}, true)
	_, qErr := queueStart(vBusySlot, true, false, "8m",
		func() (bool, int, error) {
			cur.FiveHour = &usageWindow{UsedPercent: 96, ResetsAt: reset5h}
			aQueue.now = tHigh
			return true, 0, nil
		},
		func() (*capacityView, error) { return aQueue.startCapacity(context.Background(), false) })
	if Code(qErr) != ExitRefused || !strings.Contains(qErr.Error(), "no start: usage high: 5h 96% at or past 95%") {
		t.Fatalf("queued --builds after USAGE HIGH = exit %d (%v)", Code(qErr), qErr)
	}

	// Direct agents start refuses under USAGE HIGH, and --force overrides & logs it.
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aStart, outStart := openApp(tHigh, false)
	startCmd := aStart.agentStartCmd()
	startCmd.SilenceErrors = true
	startCmd.SilenceUsage = true
	startCmd.SetArgs([]string{busyTwo, brief, usageDirFlag, dir})
	if err := startCmd.Execute(); Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: usage high: 5h 96% at or past 95%") {
		t.Fatalf("agents start under USAGE HIGH = exit %d (%v)", Code(err), err)
	}

	stubBackground(t, "backgrounded · "+bgShortID+" · "+busyTwo+"\n", bgSession{ID: bgShortID, SessionID: bgID, Name: busyTwo})
	outStart.Reset()
	forceCmd := aStart.agentStartCmd()
	forceCmd.SilenceErrors = true
	forceCmd.SilenceUsage = true
	forceCmd.SetArgs([]string{busyTwo, brief, usageDirFlag, dir, modeFlag, config.ModeBackground, usageForceFlag})
	if err := forceCmd.Execute(); err != nil {
		t.Fatalf("agents start --force under USAGE HIGH = %v", err)
	}
	if !strings.Contains(outStart.String(), "started over no start: usage high: 5h 96% at or past 95%") {
		t.Fatalf("agents start --force output = %q", outStart.String())
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == usageVerbStart })
	if err != nil || !slices.ContainsFunc(evs, func(e state.Event) bool {
		return strings.Contains(e.Detail, busyTwo+": started over no start: usage high: 5h 96% at or past 95%")
	}) {
		t.Fatalf("agents.start events = %+v, %v", evs, err)
	}
}

// TestWatchUsageReportHighWorkersAndAutoResume verifies watch --once, periodic
// USAGE across watch restarts, USAGE HIGH / ENDED USAGE HIGH, once-per-episode
// messaging of live busy workers, and gating of agents resume + autoResume
// until fiveHour.resetsAt.
func TestWatchUsageReportHighWorkersAndAutoResume(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `agents:
  autoResume: true
capacity:
  usage:
    command: ["/bin/true"]
    every: 5m
    reportEvery: 30m
    highPercent: 95
    fiveHour:
      - {upTo: 60, floor: 4, ceiling: 8}
      - {upTo: 95, floor: 1, ceiling: 3}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := keepNow
	sup := tasked("Supervisor run 1")
	parkedAuto := tasked("ParkedAuto")
	parkedAuto.Park = &state.Park{By: parkedAuto.Party, At: t0.UTC(), On: "#1", Waits: "answer to #1", Told: t0.UTC()}
	parkedManual := tasked("ParkedManual")
	parkedManual.Park = &state.Park{By: parkedManual.Party, At: t0.UTC(), Waits: "manual go", Told: t0.UTC()}
	waitingWorker := tasked("Waiter")
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: sup.Party, Since: t0.UTC()}
		st.Agents = []state.Agent{sup, tasked(busyOne), tasked(busyTwo), parkedAuto, parkedManual, waitingWorker}
		st.Notes = []state.Note{{ID: 1, At: t0.UTC(), Text: "ship it?", Due: t0.Add(time.Hour).UTC()}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	var woke []string
	origWake := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, _ state.Party, name, msg, _ string) error {
		woke = append(woke, name+" :: "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = origWake })

	reset5h := t0.Add(2 * time.Hour).UTC()
	var readErr error
	cur := usageReading{
		FiveHour: &usageWindow{UsedPercent: 40, ResetsAt: reset5h},
		SevenDay: &usageWindow{UsedPercent: 52, ResetsAt: t0.Add(96 * time.Hour).UTC()},
	}
	reader := func(_ context.Context, _ []string, _ time.Duration) (usageReading, error) {
		return cur, readErr
	}

	sessionsAt := func(now time.Time, waiterBusy bool) []*claude.Session {
		waiterSess := &claude.Session{
			ID:         "s-Waiter",
			LastActive: now.Add(-2 * waitQuiet),
			Commands:   []claude.Command{{Args: "gh run watch 9", Wait: true}},
		}
		if waiterBusy {
			waiterSess = &claude.Session{ID: "s-Waiter", LastActive: now}
		}
		// busyOne is live; busyTwo has no CLI in sessions (stopped CLI -> never woken headlessly by USAGE HIGH).
		return []*claude.Session{
			{ID: sup.Session, LastActive: now},
			{ID: "s-" + busyOne, LastActive: now},
			waiterSess,
		}
	}

	// 1. watch --once prints USAGE once and fails onceErr when the reader fails.
	readErr = fmt.Errorf("reader offline")
	var onceErrOut bytes.Buffer
	aOnceErr := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &onceErrOut, as: usageCaller, readUsage: reader}
	wOnceErr := aOnceErr.newWatcher(false, false)
	wOnceErr.now = t0
	wOnceErr.pending(context.Background(), sessionsAt(t0, false))
	if wOnceErr.onceErr() == nil || !strings.Contains(onceErrOut.String(), "USAGE unreadable: reader offline") {
		t.Fatalf("watch --once on error: err=%v, out=%q", wOnceErr.onceErr(), onceErrOut.String())
	}

	readErr = nil
	var onceOut bytes.Buffer
	aOnce := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &onceOut, as: usageCaller, readUsage: reader}
	wOnce := aOnce.newWatcher(false, false)
	wOnce.now = t0
	wOnce.pending(context.Background(), sessionsAt(t0, false))
	if err := wOnce.onceErr(); err != nil || !strings.Contains(onceOut.String(), "USAGE: 5h 40%") || !strings.Contains(onceOut.String(), "target floor 4, ceiling 8") {
		t.Fatalf("watch --once ok: err=%v, out=\n%s", err, onceOut.String())
	}

	// 2. Continuous watch prints USAGE on first poll, persists UsageReported in
	// seen.watch.<caller>.json, and a restarted watch stays quiet before 30m.
	newWatch := func(now time.Time) (*watcher, *bytes.Buffer) {
		var out bytes.Buffer
		a := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: now, out: &out, as: usageCaller, readUsage: reader}
		w := a.newWatcher(false, true)
		w.now = now
		w.doctoring.Store(true) // the doctor is not under test
		w.readHeadroom = func(context.Context, *swapReading) *headroom { return roomy() }
		t.Cleanup(func() { w.bg.Wait() })
		return w, &out
	}

	w1, out1 := newWatch(t0)
	w1.pending(context.Background(), sessionsAt(t0, false))
	w1.saveMark()
	if !strings.Contains(out1.String(), "USAGE: 5h 40%") || !strings.Contains(out1.String(), "CAPACITY LOW 2 of 4") {
		t.Fatalf("poll 1 output:\n%s", out1.String())
	}

	t5 := t0.Add(6 * time.Minute)
	w2, out2 := newWatch(t5)
	w2.pending(context.Background(), sessionsAt(t5, false))
	w2.saveMark()
	if strings.Contains(out2.String(), "USAGE: ") {
		t.Fatalf("restarted watch repeated USAGE before 30m:\n%s", out2.String())
	}

	// 3. At 31m, 5h crosses highPercent (96% >= 95%) AND note #1 closes:
	//    - USAGE (with "(USAGE HIGH)") prints after 30m;
	//    - USAGE HIGH prints once; CAPACITY LOW clears ("ENDED CAPACITY LOW");
	//    - live busyOne is told once to finish its step and park (busyTwo with no CLI is NOT woken);
	//    - ParkedAuto becomes AGENT RESUMABLE, and autoResume holds off while USAGE HIGH stands.
	t31 := t0.Add(31 * time.Minute)
	cur.FiveHour = &usageWindow{UsedPercent: 96, ResetsAt: reset5h}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = nil
		return []state.Event{event(state.Party{Name: holderPerson}, answerRecorded, "#1 answered: go ahead")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	woke = nil
	w2.now = t31
	out2.Reset()
	w2.pending(context.Background(), sessionsAt(t31, false))
	w2.saveMark()
	got31 := out2.String()
	if !strings.Contains(got31, "USAGE: 5h 96%") ||
		!strings.Contains(got31, "USAGE HIGH: 5h 96% at or past 95%") ||
		!strings.Contains(got31, "ENDED CAPACITY LOW") ||
		!strings.Contains(got31, `AGENT RESUMABLE "ParkedAuto"`) ||
		strings.Contains(got31, "AGENT RESUME FAILED") {
		t.Fatalf("at USAGE HIGH (31m):\n%s", got31)
	}
	if len(woke) != 1 || !strings.HasPrefix(woke[0], busyOne+" :: USAGE HIGH (5h 96% at or past 95%") {
		t.Fatalf("woke at 31m = %q, want only %s", woke, busyOne)
	}

	// 4. Next poll (37m, with +3s jitter on resetsAt and a watch restart) does
	//    NOT re-emit USAGE HIGH, does NOT re-tell busyOne, DOES tell Waiter once
	//    it turns busy, and still holds autoResume.
	t37 := t0.Add(37 * time.Minute)
	cur.FiveHour = &usageWindow{UsedPercent: 97, ResetsAt: reset5h.Add(3 * time.Second)}
	woke = nil
	w3, out3 := newWatch(t37)
	w3.pending(context.Background(), sessionsAt(t37, true))
	w3.saveMark()
	if strings.Contains(out3.String(), "USAGE HIGH:") || strings.Contains(out3.String(), "AGENT RESUME FAILED") {
		t.Fatalf("at 37m repeated USAGE HIGH or failed resume:\n%s", out3.String())
	}
	if len(woke) != 1 || !strings.HasPrefix(woke[0], "Waiter :: USAGE HIGH (5h 97% at or past 95%") {
		t.Fatalf("woke at 37m = %q, want only newly-busy Waiter", woke)
	}

	// 5. Manual agents resume refuses without --force under USAGE HIGH, and
	//    succeeds with --force on ParkedManual.
	aResume := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t37, out: &bytes.Buffer{}, as: usageCaller, readUsage: reader}
	resCmd := aResume.agentResumeCmd()
	resCmd.SilenceErrors = true
	resCmd.SilenceUsage = true
	resCmd.SetArgs([]string{"ParkedAuto"})
	if err := resCmd.Execute(); Code(err) != ExitRefused || !strings.Contains(err.Error(), "no resume: usage high:") {
		t.Fatalf("agents resume without --force = exit %d (%v)", Code(err), err)
	}

	woke = nil
	forceResCmd := aResume.agentResumeCmd()
	forceResCmd.SilenceErrors = true
	forceResCmd.SilenceUsage = true
	forceResCmd.SetArgs([]string{usageForceFlag, "ParkedManual"})
	if err := forceResCmd.Execute(); err != nil || len(woke) != 1 || !strings.HasPrefix(woke[0], "ParkedManual :: ") {
		t.Fatalf("agents resume --force = %v, woke=%q", err, woke)
	}
	resEvs, err := store.Events(0, func(e state.Event) bool { return e.Verb == resumedEvent })
	if err != nil || !slices.ContainsFunc(resEvs, func(e state.Event) bool {
		return strings.Contains(e.Detail, "ParkedManual") && strings.Contains(e.Detail, "over no resume: usage high:")
	}) {
		t.Fatalf("agents.resumed events = %+v, %v", resEvs, err)
	}

	// 6. Once fiveHour.resetsAt passes, watch re-reads immediately, emits ENDED
	//    USAGE HIGH, and autoResume wakes ParkedAuto in the same poll!
	tAfterReset := reset5h.Add(10 * time.Second)
	cur.FiveHour = &usageWindow{UsedPercent: 3, ResetsAt: reset5h.Add(5 * time.Hour)}
	woke = nil
	w3.now = tAfterReset
	out3.Reset()
	w3.pending(context.Background(), sessionsAt(tAfterReset, true))
	w3.saveMark()
	if !strings.Contains(out3.String(), "ENDED USAGE HIGH") {
		t.Fatalf("after 5h reset, missing ENDED USAGE HIGH:\n%s", out3.String())
	}
	if len(woke) != 1 || !strings.HasPrefix(woke[0], "ParkedAuto :: Your park on #1") {
		t.Fatalf("autoResume after 5h reset: woke = %q", woke)
	}
	stAfter, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(stAfter.Agents, func(ag state.Agent) bool { return ag.Name == "ParkedAuto" }); i < 0 || stAfter.Agents[i].Park != nil {
		t.Fatalf("ParkedAuto still parked after autoResume: %+v", stAfter.Agents)
	}

	// 7. Live-reloading config.yaml without capacity.usage clears an active USAGE HIGH.
	tSecondHigh := tAfterReset.Add(10 * time.Minute)
	cur.FiveHour = &usageWindow{UsedPercent: 98, ResetsAt: reset5h.Add(5 * time.Hour)}
	w3.now = tSecondHigh
	out3.Reset()
	w3.pending(context.Background(), sessionsAt(tSecondHigh, false))
	if !strings.Contains(out3.String(), "USAGE HIGH: 5h 98%") {
		t.Fatalf("second episode did not fire:\n%s", out3.String())
	}
	if err := os.WriteFile(cfgPath, []byte("agents:\n  autoResume: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w3.now = tSecondHigh.Add(time.Minute)
	out3.Reset()
	w3.pending(context.Background(), sessionsAt(w3.now, false))
	if !strings.Contains(out3.String(), "ENDED USAGE HIGH") {
		t.Fatalf("removing capacity.usage did not clear USAGE HIGH:\n%s", out3.String())
	}
}

// TestSnapshotUsageAndDiff checks that snapshot prints usage and diffs band or
// USAGE HIGH transitions while ignoring small movements within the same band.
func TestSnapshotUsageAndDiff(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `capacity:
  usage:
    command: ["/bin/true"]
    every: 5m
    fiveHour:
      - {upTo: 50, floor: 5, ceiling: 10}
      - {upTo: 90, floor: 2, ceiling: 4}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := keepNow
	reset5h := t0.Add(3 * time.Hour).UTC()
	pct5h := 30.0
	a := &app{
		cfgPath: cfgPath,
		cfg:     cfg,
		store:   store,
		now:     t0,
		out:     &bytes.Buffer{},
		readUsage: func(_ context.Context, _ []string, _ time.Duration) (usageReading, error) {
			return usageReading{FiveHour: &usageWindow{UsedPercent: pct5h, ResetsAt: reset5h}}, nil
		},
	}

	s1, err := a.takeSnapshot(context.Background(), t0.Add(-time.Hour), false, false)
	if err != nil {
		t.Fatal(err)
	}
	var printBuf bytes.Buffer
	a.out = &printBuf
	a.printSnapshot(s1)
	if !strings.Contains(printBuf.String(), "usage: 5h 30%") || !strings.Contains(printBuf.String(), "target floor 5, ceiling 10") {
		t.Fatalf("printSnapshot:\n%s", printBuf.String())
	}

	// Movement within the same band (30% -> 35%): no diff line.
	pct5h = 35
	a.now = t0.Add(6 * time.Minute)
	s2, err := a.takeSnapshot(context.Background(), t0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffSnapshots(s1, s2); slices.ContainsFunc(d, func(s string) bool { return strings.HasPrefix(s, "usage:") }) {
		t.Errorf("same-band diff = %q, want no usage line", d)
	}

	// Crossing into second band (35% -> 60%): diff reports the new target.
	pct5h = 60
	a.now = t0.Add(12 * time.Minute)
	s3, err := a.takeSnapshot(context.Background(), a.now.Add(-6*time.Minute), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffSnapshots(s2, s3); !slices.ContainsFunc(d, func(s string) bool {
		return strings.Contains(s, "usage: 5h 60%") && strings.Contains(s, "target floor 2, ceiling 4")
	}) {
		t.Errorf("band-change diff = %q, want usage target floor 2, ceiling 4", d)
	}
}

// TestParseAgyUsageAndStaleRead verifies parsing `agy -p /usage` TSV output
// and checks that a stale or failed usage read yields ceiling 0, room 0 for
// that provider rather than full room.
func TestParseAgyUsageAndStaleRead(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)

	tsv := strings.Join([]string{
		"Gemini Models\tWeekly Limit Remaining\t78%\t2026-11-03T09:23:51Z",
		"Gemini Models\tFive Hour Limit Remaining\t94%\t2026-10-28T05:23:51Z",
		"Claude and GPT models\tWeekly Limit Remaining\t91%\t2026-11-03T11:39:11Z",
		"Claude and GPT models\tFive Hour Limit Remaining\t89%\t2026-10-28T06:23:51Z",
	}, "\n")
	parsed, err := parseUsageOutput([]byte(tsv))
	if err != nil {
		t.Fatalf("parseUsageOutput TSV: %v", err)
	}
	gem, ok := parsed.Buckets[config.DefaultAgyGeminiBucket]
	if !ok || gem.FiveHour == nil || gem.FiveHour.UsedPercent != 6 || gem.SevenDay == nil || gem.SevenDay.UsedPercent != 22 {
		t.Fatalf("Gemini bucket = %+v", gem)
	}
	cl, ok := parsed.Buckets[config.DefaultAgyClaudeBucket]
	if !ok || cl.FiveHour == nil || cl.FiveHour.UsedPercent != 11 || cl.SevenDay == nil || cl.SevenDay.UsedPercent != 9 {
		t.Fatalf("Claude bucket = %+v", cl)
	}

	claudeFile := filepath.Join(dir, "claude-usage.json")
	claudeExit := filepath.Join(dir, "claude-exit")
	claudeReader := filepath.Join(dir, "read-claude")
	if err := os.WriteFile(claudeReader, []byte("#!/bin/sh\nif [ -f \""+claudeExit+"\" ]; then exit 1; fi\ncat \""+claudeFile+"\"\n"), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	agyFile := filepath.Join(dir, "agy-usage.tsv")
	agyReader := filepath.Join(dir, "read-agy")
	if err := os.WriteFile(agyReader, []byte("#!/bin/sh\ncat \""+agyFile+"\"\n"), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	// The reader's Gemini week is 8% used, under its even pace of 10.5%: the
	// fallback below takes it, as it would no choice ahead of the pace.
	if err := os.WriteFile(agyFile, []byte(strings.Replace(tsv, "78%", "92%", 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	rawCfg := fmt.Sprintf(`capacity:
  floor: 2
  ceiling: 4
  providers:
    claude:
      floor: 1
      ceiling: 2
      staleAfter: 15m
      command: [%q]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
      sevenDay:
        - {upTo: 90, floor: 1, ceiling: 2}
    agy:
      floor: 1
      ceiling: 2
      command: [%q]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
      sevenDay:
        - {upTo: 90, floor: 1, ceiling: 2}
  choices:
    - {harness: claude, model: claude-opus-4-7}
    - {harness: agy, model: gemini-3-1-pro-medium}
`, claudeReader, agyReader)
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)

	// 1. Stale reading via ageMinutes > staleAfter (15m) yields ceiling 0, room 0 for claude, falling through to agy.
	if err := os.WriteFile(claudeFile, []byte(`{"ageMinutes":32,"fiveHour":{"usedPercent":10},"sevenDay":{"usedPercent":20}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &bytes.Buffer{}}
	v, err := a.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if cp := v.Providers[claudeComm]; cp == nil || cp.Ceiling != 0 || cp.Room != 0 || !cp.Stale {
		t.Fatalf("stale ageMinutes claude provider = %+v, want ceiling 0, room 0, stale=true", cp)
	}
	if ap := v.Providers[harnessAgy]; ap == nil || ap.Ceiling != 2 || ap.Room != 2 {
		t.Fatalf("agy provider = %+v, want ceiling 2, room 2", ap)
	}
	if v.Choice == nil || v.Choice.Harness != harnessAgy || v.Choice.Model != testModelGemini {
		t.Fatalf("choice on stale claude = %+v, want agy/gemini-3-1-pro-medium", v.Choice)
	}

	// 2. Stale reading via "at" timestamp older than staleAfter also yields ceiling 0, room 0.
	staleAt := t0.Add(-20 * time.Minute).Format(time.RFC3339)
	if err := os.WriteFile(claudeFile, []byte(fmt.Sprintf(`{"at":%q,"fiveHour":{"usedPercent":10},"sevenDay":{"usedPercent":20}}`, staleAt)), 0o600); err != nil {
		t.Fatal(err)
	}
	v2, err := a.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if cp := v2.Providers[claudeComm]; cp == nil || cp.Ceiling != 0 || cp.Room != 0 || !cp.Stale {
		t.Fatalf("stale at claude provider = %+v, want ceiling 0, room 0, stale=true", cp)
	}

	// 3. Failed reading (exit 1) also yields ceiling 0, room 0 for claude (never full room).
	if err := os.WriteFile(claudeExit, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v3, err := a.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if cp := v3.Providers[claudeComm]; cp == nil || cp.Ceiling != 0 || cp.Room != 0 || cp.Error == "" {
		t.Fatalf("failed claude provider = %+v, want ceiling 0, room 0, error set", cp)
	}
}

const (
	testModelOpus47 = "claude-opus-4-7"
	testModelOpus46 = "claude-opus-4-6-thinking"
	testModelGemini = "gemini-3-1-pro-medium"
	testReadClaude  = "read-claude"
	testExcludeFlag = "--exclude"
	testHarnessFlag = "--harness"
	testDryRunFlag  = "--dry-run"
)

// TestProvidersChoicesPaceMarginExcludeAndMachineGuards exercises per-provider
// capacity verdicts, choice order, the even pace no paceMargin, skipPace or
// review loosens, --exclude, busy agent attribution per provider/bucket,
// machine guards capping the total, watch once-per-tick USAGE output, and
// agents start --harness auto.
func TestProvidersChoicesPaceMarginExcludeAndMachineGuards(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `capacity:
  floor: 2
  ceiling: 3
  reviewSkipPace: true
  providers:
    claude:
      floor: 1
      ceiling: 2
      command: ["read-claude"]
      fiveHourMax: 90
      sevenDayMax: 95
      paceMargin: 5
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
      sevenDay:
        - {upTo: 95, floor: 1, ceiling: 2}
    agy:
      floor: 1
      ceiling: 2
      command: ["read-agy"]
      fiveHourMax: 90
      sevenDayMax: 95
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
      sevenDay:
        - {upTo: 95, floor: 1, ceiling: 2}
  choices:
    - harness: claude
      model: claude-opus-4-7
      paceMargin: 5
    - harness: agy
      model: claude-opus-4-6-thinking
      paceMargin: 0
    - harness: agy
      model: gemini-3-1-pro-medium
      skipPace: true
      sevenDayMax: 80
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	reset5h := t0.Add(3 * time.Hour).UTC()
	// 84h remaining out of 168h => pace is exactly 50.0%.
	reset7d := t0.Add(84 * time.Hour).UTC()

	claude5h, claude7d := 20.0, 48.0 // 48% under the even pace of 50% => OK
	agyClaude5h, agyClaude7d := 15.0, 48.0
	agyGemini5h, agyGemini7d := 10.0, 72.0

	reader := func(_ context.Context, cmd []string, _ time.Duration) (usageReading, error) {
		if len(cmd) > 0 && cmd[0] == testReadClaude {
			return usageReading{
				FiveHour: &usageWindow{UsedPercent: claude5h, ResetsAt: reset5h},
				SevenDay: &usageWindow{UsedPercent: claude7d, ResetsAt: reset7d},
			}, nil
		}
		return usageReading{
			Buckets: map[string]usageBucket{
				config.DefaultAgyClaudeBucket: {
					FiveHour: &usageWindow{UsedPercent: agyClaude5h, ResetsAt: reset5h},
					SevenDay: &usageWindow{UsedPercent: agyClaude7d, ResetsAt: reset7d},
				},
				config.DefaultAgyGeminiBucket: {
					FiveHour: &usageWindow{UsedPercent: agyGemini5h, ResetsAt: reset5h},
					SevenDay: &usageWindow{UsedPercent: agyGemini7d, ResetsAt: reset7d},
				},
			},
			BucketOrder: []string{config.DefaultAgyGeminiBucket, config.DefaultAgyClaudeBucket},
		}, nil
	}

	newApp := func(now time.Time, jsonOut bool) (*app, *bytes.Buffer) {
		var out bytes.Buffer
		// Clear cached provider usage files so each step sees the updated test numbers.
		_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(claudeComm)))
		_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(harnessAgy)))
		return &app{
			cfgPath:   cfgPath,
			cfg:       cfg,
			store:     store,
			now:       now,
			out:       &out,
			as:        agentOne,
			json:      jsonOut,
			readUsage: reader,
		}, &out
	}

	// 1. claude 7d 48% is under the pace of 50%: first choice claude/claude-opus-4-7 wins.
	//    Total room is capped by capacity.ceiling (3 < 2+2=4).
	a1, out1 := newApp(t0, false)
	if err := a1.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	got1 := out1.String()
	if !strings.Contains(got1, "provider claude:") ||
		!strings.Contains(got1, "provider agy:") ||
		!strings.Contains(got1, "choice: claude / claude-opus-4-7 (provider claude)") ||
		!strings.Contains(got1, "room for 3 starts") {
		t.Fatalf("step 1 capacity output:\n%s", got1)
	}

	// 2. --exclude claude-opus-4-7 skips the first choice and picks agy/claude-opus-4-6-thinking.
	aEx, outEx := newApp(t0, true)
	capEx := aEx.capacityCmd()
	capEx.SetArgs([]string{testExcludeFlag, testModelOpus47})
	if err := capEx.Execute(); err != nil {
		t.Fatal(err)
	}
	var parsedEx capacityView
	if err := json.Unmarshal(outEx.Bytes(), &parsedEx); err != nil {
		t.Fatal(err)
	}
	if parsedEx.Choice == nil || parsedEx.Choice.Harness != harnessAgy || parsedEx.Choice.Model != testModelOpus46 ||
		parsedEx.Choice.Bucket != config.DefaultAgyClaudeBucket {
		t.Fatalf("excluded choice = %+v", parsedEx.Choice)
	}

	// 3. claude 7d moves to 54%: ahead of the pace of 50%, which its paceMargin
	//    of 5 no longer excuses, so agy/claude-opus-4-6-thinking wins.
	claude7d = 54.0
	a2, _ := newApp(t0, false)
	v2, err := a2.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Choice == nil || v2.Choice.Harness != harnessAgy || v2.Choice.Model != testModelOpus46 {
		t.Fatalf("step 3 choice = %+v, want agy/claude-opus-4-6-thinking", v2.Choice)
	}

	// 4. agy Claude bucket 7d moves to 52% (> 50%) -> ahead of pace too; the
	//    Gemini bucket's 72% is ahead of the pace as well, which its skipPace
	//    no longer waives: no choice, room 0.
	agyClaude7d = 52.0
	a3, _ := newApp(t0, false)
	v3, err := a3.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Room != 0 || v3.Choice == nil || v3.Choice.Harness != "" || !strings.Contains(v3.Choice.Candidates[2].Reason, "ahead of pace") {
		t.Fatalf("step 4 = room %d, choice %+v; want room 0, no choice, gemini ahead of pace", v3.Room, v3.Choice)
	}

	// 5. A review is decided like a worker's start: with capacity.reviewSkipPace
	//    configured, the same windows give it no choice either.
	a4Rev, _ := newApp(t0, false)
	vReview, err := a4Rev.startCapacityWithOptions(context.Background(), false, nil, forReview)
	if err != nil {
		t.Fatal(err)
	}
	if vReview.Room != 0 || vReview.Choice == nil || vReview.Choice.Harness != "" || !vReview.Choice.Review {
		t.Fatalf("review ahead of pace = room %d, choice %+v; want room 0 and no choice", vReview.Room, vReview.Choice)
	}

	// 6. Busy agents count against their provider/bucket, and machine guards cap the total across providers.
	claude7d = 40.0
	agyClaude7d = 40.0
	agyGemini7d = 40.0
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		w1 := tasked("ClaudeWorker1")
		w1.Harness, w1.Model = claudeComm, testModelOpus47
		w2 := tasked("ClaudeWorker2")
		w2.Harness, w2.Model = claudeComm, testModelOpus47
		w3 := tasked("AgyWorker1")
		w3.Harness, w3.Model = harnessAgy, testModelGemini
		st.Agents = []state.Agent{w1, w2, w3}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// 2 busy on claude (at its ceiling of 2 -> room 0) + 1 busy on agy (ceiling 2 -> room 1),
	// BUT total busy is 3, which is at top-level capacity.ceiling of 3 -> machineRoomCap caps agy to 0!
	a5, _ := newApp(t0, false)
	vCap, err := a5.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if vCap.Room != 0 || vCap.Providers[claudeComm].Room != 0 || vCap.Providers[harnessAgy].Room != 0 {
		t.Fatalf("at top-level ceiling 3: total room=%d, claude=%d, agy=%d; want all 0",
			vCap.Room, vCap.Providers[claudeComm].Room, vCap.Providers[harnessAgy].Room)
	}

	// Free one claude worker so 1 busy on claude (room 1) + 1 busy on agy (room 1), total busy 2 < 3 => room 1.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = st.Agents[1:]
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a6, _ := newApp(t0, false)
	vOpen, err := a6.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	if err != nil {
		t.Fatal(err)
	}
	if vOpen.Room != 1 || vOpen.Choice == nil || vOpen.Choice.Harness != claudeComm {
		t.Fatalf("with 2 busy of 3: room=%d, choice=%+v; want room 1, claude", vOpen.Room, vOpen.Choice)
	}

	// Machine guard (low MemAvailable) caps all providers and total room to 0.
	origMachine := plat.Machine
	plat.Machine = tightMachine{Machine: origMachine}
	aTight, _ := newApp(t0, false)
	vTight, err := aTight.startCapacityWithOptions(context.Background(), false, nil, forWorker)
	plat.Machine = origMachine
	if err != nil {
		t.Fatal(err)
	}
	if vTight.Room != 0 || vTight.Providers[claudeComm].Room != 0 || vTight.Providers[harnessAgy].Room != 0 {
		t.Fatalf("under low MemAvailable: total=%d, claude=%d, agy=%d; want all 0",
			vTight.Room, vTight.Providers[claudeComm].Room, vTight.Providers[harnessAgy].Room)
	}

	// 7. watch emits the per-provider USAGE line with choice once per tick, and
	//    agents start --harness auto picks the first choice with room.
	var watchOut bytes.Buffer
	aWatch, _ := newApp(t0, false)
	aWatch.out = &watchOut
	w := aWatch.newWatcher(false, true)
	w.now = t0
	w.doctoring.Store(true)
	w.readHeadroom = func(context.Context, *swapReading) *headroom { return roomy() }
	t.Cleanup(func() { w.bg.Wait() })
	w.pending(context.Background(), nil)
	if got := watchOut.String(); !strings.Contains(got, "USAGE: claude: ") ||
		!strings.Contains(got, "; agy: ") ||
		!strings.Contains(got, "; choice: claude/claude-opus-4-7") {
		t.Fatalf("watch providers USAGE line:\n%s", got)
	}

	brief := filepath.Join(dir, "auto-brief.md")
	if err := os.WriteFile(brief, []byte("Work the auto task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubBackground(t, "backgrounded · "+bgShortID+" · AutoWorker\n", bgSession{ID: bgShortID, SessionID: bgID, Name: "AutoWorker"})
	aAuto, outAuto := newApp(t0, false)
	startCmd := aAuto.agentStartCmd()
	startCmd.SilenceErrors = true
	startCmd.SilenceUsage = true
	startCmd.SetArgs([]string{"AutoWorker", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, harnessAuto})
	if err := startCmd.Execute(); err != nil {
		t.Fatalf("agents start --harness auto: %v", err)
	}
	if !strings.Contains(outAuto.String(), "choice: claude / claude-opus-4-7 (provider claude)") ||
		!strings.Contains(outAuto.String(), "its turns run on claude-opus-4-7") {
		t.Fatalf("agents start --harness auto output:\n%s", outAuto.String())
	}

	// 8. When claude and agy/Claude are ahead of pace and agy/Gemini is at 82% (>= 80% worker cap):
	//    - a worker `agents start --harness auto` is refused with ExitRefused naming the choice reasons;
	//    - `agents start --harness auto --review --exclude claude --exclude claude-opus-4-6-thinking`
	//      is refused alike: a review skips no gate;
	//    - with agy/Gemini back under its pace and cap, that review starts the session through the agy harness on gemini-3-1-pro-medium.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	claude7d = 56.0
	agyClaude7d = 52.0
	agyGemini7d = 82.0
	aRefuse, outRefuse := newApp(t0, false)
	refuseCmd := aRefuse.agentStartCmd()
	refuseCmd.SilenceErrors = true
	refuseCmd.SilenceUsage = true
	refuseCmd.SetArgs([]string{"BlockedWorker", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto})
	err = refuseCmd.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: no choice has room:") ||
		!strings.Contains(err.Error(), "7d 82% at or past worker cap 80%") || outRefuse.String() != "" {
		t.Fatalf("worker --harness auto at 82%% gemini: code=%d, err=%v, out=%q", Code(err), err, outRefuse.String())
	}

	binDir := t.TempDir()
	fakeAgy := filepath.Join(binDir, harnessAgy)
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\necho 'Available models:'\necho '  gemini-3-1-pro-medium'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	origLauncher := plat.Launcher
	origProcMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origProcMachine
	})
	plat.Machine = tableMachine{plat.Machine}
	plat.Launcher = &agyTestLauncher{
		onStart: func(u platform.Unit) {
			go func() {
				var startID string
				for _, e := range u.Env {
					if v, ok := strings.CutPrefix(e, agy.EnvAgent+"="); ok {
						startID = v
					}
				}
				inbox := agy.InboxPath(cfg.StateDir, startID)
				logPath := agy.StreamPath(cfg.StateDir, startID)
				f, oerr := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
				if oerr != nil {
					return
				}
				defer func() { _ = f.Close() }()
				_, _ = bufio.NewReader(f).ReadString('\n')
				_, _ = agy.Drain(inbox)
				stream := `{"event":"init","conversation_id":"conv-review-agy"}` + "\n" +
					`{"event":"step_update","step_update":{"conversation_id":"conv-review-agy","step_type":"user_input"}}` + "\n"
				_ = os.WriteFile(logPath, []byte(stream), 0o600)
			}()
		},
	}
	reviewArgs := []string{"ReviewAgy", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, "--" + reviewFlag, testExcludeFlag, claudeComm, testExcludeFlag, testModelOpus46}
	aRevRefuse, outRevRefuse := newApp(t0, false)
	revRefuseCmd := aRevRefuse.agentStartCmd()
	revRefuseCmd.SilenceErrors = true
	revRefuseCmd.SilenceUsage = true
	revRefuseCmd.SetArgs(reviewArgs)
	err = revRefuseCmd.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "7d 82% at or past worker cap 80%") || outRevRefuse.String() != "" {
		t.Fatalf("review --harness auto at 82%% gemini: code=%d, err=%v, out=%q", Code(err), err, outRevRefuse.String())
	}
	agyGemini7d = 45.0
	aRevStart, outRevStart := newApp(t0, false)
	revCmd := aRevStart.agentStartCmd()
	revCmd.SilenceErrors = true
	revCmd.SilenceUsage = true
	revCmd.SetArgs(reviewArgs)
	if err := revCmd.Execute(); err != nil {
		t.Fatalf("agents start --harness auto --review: %v", err)
	}
	if !strings.Contains(outRevStart.String(), "choice: agy / gemini-3-1-pro-medium (provider agy, bucket Gemini Models, review)") ||
		!strings.Contains(outRevStart.String(), "started ReviewAgy: agy agent agy_") ||
		!strings.Contains(outRevStart.String(), "on gemini-3-1-pro-medium") {
		t.Fatalf("agents start --harness auto --review output:\n%s", outRevStart.String())
	}
}

// TestExplicitHarnessGateAndAutoHarnessValidation verifies that:
//   - an explicit --harness start (--harness claude included) is gated
//     against its resolved provider and bucket room/blocks (refusing with exit 3
//     unless --force is given) even when another provider has room;
//   - --harness auto refuses clearly before writing any state or printing choice/gaps
//     when it selects a choice whose harness is agy (or omp with --mode background),
//     and dispatches --dry-run on the resolved harness.
func TestExplicitHarnessGateAndAutoHarnessValidation(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `capacity:
  floor: 2
  ceiling: 4
  providers:
    claude:
      floor: 1
      ceiling: 1
      command: ["read-claude"]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 1}
      sevenDay:
        - {upTo: 95, floor: 1, ceiling: 1}
    agy:
      floor: 1
      ceiling: 2
      command: ["read-agy"]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 1}
      sevenDay:
        - {upTo: 95, floor: 1, ceiling: 1}
    omp:
      floor: 1
      ceiling: 1
  choices:
    - {harness: claude, model: claude-opus-4-7}
    - {harness: agy, model: gemini-3-1-pro-medium, skipPace: true}
    - {harness: omp, model: provider/omp-model, provider: omp, skipPace: true}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	reset5h := t0.Add(3 * time.Hour).UTC()
	reset7d := t0.Add(84 * time.Hour).UTC()

	var claudeErr error
	claude5h := 20.0
	agyClaude5h := 15.0

	reader := func(_ context.Context, cmd []string, _ time.Duration) (usageReading, error) {
		if len(cmd) > 0 && cmd[0] == testReadClaude {
			if claudeErr != nil {
				return usageReading{}, claudeErr
			}
			return usageReading{
				FiveHour: &usageWindow{UsedPercent: claude5h, ResetsAt: reset5h},
				SevenDay: &usageWindow{UsedPercent: 30, ResetsAt: reset7d},
			}, nil
		}
		return usageReading{
			Buckets: map[string]usageBucket{
				config.DefaultAgyClaudeBucket: {
					FiveHour: &usageWindow{UsedPercent: agyClaude5h, ResetsAt: reset5h},
					SevenDay: &usageWindow{UsedPercent: 30, ResetsAt: reset7d},
				},
				config.DefaultAgyGeminiBucket: {
					FiveHour: &usageWindow{UsedPercent: 10, ResetsAt: reset5h},
					SevenDay: &usageWindow{UsedPercent: 20, ResetsAt: reset7d},
				},
			},
			BucketOrder: []string{config.DefaultAgyGeminiBucket, config.DefaultAgyClaudeBucket},
		}, nil
	}

	newApp := func() (*app, *bytes.Buffer) {
		var out bytes.Buffer
		_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(claudeComm)))
		_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(harnessAgy)))
		return &app{
			cfgPath:   cfgPath,
			cfg:       cfg,
			store:     store,
			now:       t0,
			out:       &out,
			as:        agentOne,
			readUsage: reader,
		}, &out
	}

	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1a. claude reader fails while agy has Room > 0: `agents start --harness claude` refuses with exit 3.
	claudeErr = fmt.Errorf("claude endpoint down")
	aFail, _ := newApp()
	cmdFail := aFail.agentStartCmd()
	cmdFail.SilenceErrors = true
	cmdFail.SilenceUsage = true
	cmdFail.SetArgs([]string{"ClaudeOnFail", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, claudeComm})
	err = cmdFail.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: subscription usage unknown (claude endpoint down)") {
		t.Fatalf("explicit claude start on failed read: code=%d, err=%v", Code(err), err)
	}

	// 1b. claude in USAGE HIGH (96%) while agy has Room > 0: `agents start --harness claude` refuses with exit 3.
	claudeErr = nil
	claude5h = 96.0
	aHigh, _ := newApp()
	cmdHigh := aHigh.agentStartCmd()
	cmdHigh.SilenceErrors = true
	cmdHigh.SilenceUsage = true
	cmdHigh.SetArgs([]string{"ClaudeOnHigh", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, claudeComm})
	err = cmdHigh.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: usage high: 5h 96% at or past 95%") {
		t.Fatalf("explicit claude start on USAGE HIGH: code=%d, err=%v", Code(err), err)
	}

	// 1c. claude at its derived ceiling (1 busy of 1) while agy has Room > 0:
	//     refuses without --force, and succeeds with --force recording the provider block.
	claude5h = 20.0
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		w := tasked("BusyClaude")
		w.Harness, w.Model = claudeComm, testModelOpus47
		st.Agents = []state.Agent{w}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aCeil, _ := newApp()
	cmdCeil := aCeil.agentStartCmd()
	cmdCeil.SilenceErrors = true
	cmdCeil.SilenceUsage = true
	cmdCeil.SetArgs([]string{"ClaudeAtCeil", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, claudeComm})
	err = cmdCeil.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: 1 busy at the ceiling of 1") {
		t.Fatalf("explicit claude start at ceiling: code=%d, err=%v", Code(err), err)
	}

	stubBackground(t, "backgrounded · "+bgShortID+" · ClaudeForced\n", bgSession{ID: bgShortID, SessionID: bgID, Name: "ClaudeForced"})
	aForce, outForce := newApp()
	cmdForce := aForce.agentStartCmd()
	cmdForce.SilenceErrors = true
	cmdForce.SilenceUsage = true
	cmdForce.SetArgs([]string{"ClaudeForced", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, claudeComm, usageForceFlag})
	if err := cmdForce.Execute(); err != nil {
		t.Fatalf("explicit claude start --force: %v", err)
	}
	if !strings.Contains(outForce.String(), "started over no start: provider claude: 1 busy at the ceiling of 1 (--force)") {
		t.Fatalf("explicit claude start --force output:\n%s", outForce.String())
	}

	// 1d. Bucket-level gate: when agy's Claude bucket is at its ceiling (1 busy of 1) while Gemini bucket is open,
	//     startGateWithOptions for agy + claude-opus-4-6-thinking refuses with exit 3 naming the bucket.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		w := tasked("BusyAgyClaude")
		w.Harness, w.Model = harnessAgy, testModelOpus46
		st.Agents = []state.Agent{w}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aBucket, _ := newApp()
	_, _, err = aBucket.startGateWithOptions(context.Background(), "AgyClaudeTry", harnessAgy, testModelOpus46, false, false, nil, forWorker)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider agy (Claude and GPT models): 1 busy at the ceiling of 1") {
		t.Fatalf("bucket gate: code=%d, err=%v", Code(err), err)
	}

	// 2a. --dry-run --harness auto picking agy prints the agy choice and agy's dry-run command line.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	claude5h = 96.0 // claude blocked, so auto picks agy/gemini-3-1-pro-medium
	aDryAgy, outDryAgy := newApp()
	cmdDryAgy := aDryAgy.agentStartCmd()
	cmdDryAgy.SilenceErrors = true
	cmdDryAgy.SilenceUsage = true
	cmdDryAgy.SetArgs([]string{"DryAgy", brief, usageDirFlag, dir, testDryRunFlag, testHarnessFlag, harnessAuto})
	if err := cmdDryAgy.Execute(); err != nil {
		t.Fatalf("--dry-run --harness auto picking agy: %v", err)
	}
	if !strings.Contains(outDryAgy.String(), "choice: agy / gemini-3-1-pro-medium (provider agy, bucket Gemini Models)") ||
		!strings.Contains(outDryAgy.String(), "as an agy agent on gemini-3-1-pro-medium") ||
		!strings.Contains(outDryAgy.String(), "start: agy --input-format stream-json --output-format stream-json --model gemini-3-1-pro-medium") {
		t.Fatalf("--dry-run --harness auto agy output:\n%s", outDryAgy.String())
	}

	// 2b. --mode background --harness auto picking agy refuses with usage error, and --harness auto picking agy validates the model against agy models.
	aBgAgy, _ := newApp()
	cmdBgAgy := aBgAgy.agentStartCmd()
	cmdBgAgy.SilenceErrors = true
	cmdBgAgy.SilenceUsage = true
	cmdBgAgy.SetArgs([]string{"BgAgy", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, harnessAuto})
	err = cmdBgAgy.Execute()
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--mode background and --harness agy") {
		t.Fatalf("--mode background --harness auto picking agy: code=%d, err=%v", Code(err), err)
	}

	binDir := t.TempDir()
	fakeAgy := filepath.Join(binDir, harnessAgy)
	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\necho 'Available models:'\necho '  claude-opus-4-6-thinking'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	evsBeforeBad, err := store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	aAutoAgyBadModel, outAutoAgyBadModel := newApp()
	cmdAutoAgyBadModel := aAutoAgyBadModel.agentStartCmd()
	cmdAutoAgyBadModel.SilenceErrors = true
	cmdAutoAgyBadModel.SilenceUsage = true
	cmdAutoAgyBadModel.SetArgs([]string{"AutoAgyBadModel", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, usageForceFlag})
	err = cmdAutoAgyBadModel.Execute()
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), `agy lists no model "gemini-3-1-pro-medium"`) {
		t.Fatalf("--harness auto picking agy unlisted model: code=%d, err=%v", Code(err), err)
	}
	if outAutoAgyBadModel.String() != "" {
		t.Fatalf("--harness auto unlisted model printed output before refusing:\n%s", outAutoAgyBadModel.String())
	}
	stAfterBad, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	evsAfterBad, err := store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evsAfterBad) != len(evsBeforeBad) || len(stAfterBad.Agents) != 0 {
		t.Fatalf("--harness auto unlisted model mutated state: events=%d (was %d), agents=%+v", len(evsAfterBad), len(evsBeforeBad), stAfterBad.Agents)
	}

	if err := os.WriteFile(fakeAgy, []byte("#!/bin/sh\necho 'Available models:'\necho '  claude-opus-4-6-thinking'\necho '  gemini-3-1-pro-medium'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	origLauncher := plat.Launcher
	origProcMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origProcMachine
	})
	plat.Machine = tableMachine{plat.Machine}
	plat.Launcher = &agyTestLauncher{
		onStart: func(u platform.Unit) {
			go func() {
				var startID, ompID string
				for _, e := range u.Env {
					if v, ok := strings.CutPrefix(e, agy.EnvAgent+"="); ok {
						startID = v
					}
					if v, ok := strings.CutPrefix(e, omp.EnvAgent+"="); ok {
						ompID = v
					}
				}
				if ompID != "" {
					inbox := omp.InboxPath(cfg.StateDir, ompID)
					f, oerr := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
					if oerr != nil {
						return
					}
					defer func() { _ = f.Close() }()
					_, _ = bufio.NewReader(f).ReadString('\n')
					return
				}
				inbox := agy.InboxPath(cfg.StateDir, startID)
				logPath := agy.StreamPath(cfg.StateDir, startID)
				f, oerr := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
				if oerr != nil {
					return
				}
				defer func() { _ = f.Close() }()
				_, _ = bufio.NewReader(f).ReadString('\n')
				_, _ = agy.Drain(inbox)
				stream := `{"event":"init","conversation_id":"conv-auto-agy"}` + "\n" +
					`{"event":"step_update","step_update":{"conversation_id":"conv-auto-agy","step_type":"user_input"}}` + "\n"
				_ = os.WriteFile(logPath, []byte(stream), 0o600)
			}()
		},
	}
	aAutoAgy, outAutoAgy := newApp()
	cmdAutoAgy := aAutoAgy.agentStartCmd()
	cmdAutoAgy.SilenceErrors = true
	cmdAutoAgy.SilenceUsage = true
	cmdAutoAgy.SetArgs([]string{"AutoAgy", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto})
	if err := cmdAutoAgy.Execute(); err != nil {
		t.Fatalf("--harness auto picking agy: %v", err)
	}
	if !strings.Contains(outAutoAgy.String(), "choice: agy / gemini-3-1-pro-medium (provider agy, bucket Gemini Models)") ||
		!strings.Contains(outAutoAgy.String(), "started AutoAgy: agy agent agy_") ||
		!strings.Contains(outAutoAgy.String(), "on gemini-3-1-pro-medium") {
		t.Fatalf("--harness auto agy start output:\n%s", outAutoAgy.String())
	}
	stAfterAgy, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(stAfterAgy.Agents) != 1 || stAfterAgy.Agents[0].Name != "AutoAgy" ||
		stAfterAgy.Agents[0].Harness != harnessAgy || stAfterAgy.Agents[0].Model != testModelGemini ||
		!strings.HasPrefix(stAfterAgy.Agents[0].HostSession, agy.HostPrefix) {
		t.Fatalf("roster after --harness auto agy start = %+v", stAfterAgy.Agents)
	}
	autoAgyID := strings.TrimPrefix(stAfterAgy.Agents[0].HostSession, agy.HostPrefix)
	if _, serr := os.Stat(agy.InboxPath(cfg.StateDir, autoAgyID)); serr != nil {
		t.Fatalf("expected FIFO inbox for AutoAgy: %v", serr)
	}

	// 2c. --dry-run and real start with --harness auto picking omp (when claude is high and agy is excluded).
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aDryOmp, outDryOmp := newApp()
	cmdDryOmp := aDryOmp.agentStartCmd()
	cmdDryOmp.SilenceErrors = true
	cmdDryOmp.SilenceUsage = true
	cmdDryOmp.SetArgs([]string{"DryOmp", brief, usageDirFlag, dir, testDryRunFlag, testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy})
	if err := cmdDryOmp.Execute(); err != nil {
		t.Fatalf("--dry-run --harness auto picking omp: %v", err)
	}
	if !strings.Contains(outDryOmp.String(), "choice: omp / provider/omp-model (provider omp)") ||
		!strings.Contains(outDryOmp.String(), "as an omp agent on provider/omp-model") ||
		!strings.Contains(outDryOmp.String(), "start: omp --mode rpc --no-ui --approval-mode yolo --model provider/omp-model") {
		t.Fatalf("--dry-run --harness auto omp output:\n%s", outDryOmp.String())
	}

	aBgOmp, _ := newApp()
	cmdBgOmp := aBgOmp.agentStartCmd()
	cmdBgOmp.SilenceErrors = true
	cmdBgOmp.SilenceUsage = true
	cmdBgOmp.SetArgs([]string{"BgOmp", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy})
	err = cmdBgOmp.Execute()
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--mode background and --harness omp") {
		t.Fatalf("--mode background --harness auto picking omp: code=%d, err=%v", Code(err), err)
	}

	fakeOmp := filepath.Join(binDir, omp.Comm)
	if err := os.WriteFile(fakeOmp, []byte("#!/bin/sh\necho '{\"models\":[{\"selector\":\"provider/omp-model\"}]}'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	aAutoOmp, outAutoOmp := newApp()
	cmdAutoOmp := aAutoOmp.agentStartCmd()
	cmdAutoOmp.SilenceErrors = true
	cmdAutoOmp.SilenceUsage = true
	cmdAutoOmp.SetArgs([]string{"AutoOmp", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy})
	if err := cmdAutoOmp.Execute(); err != nil {
		t.Fatalf("--harness auto picking omp: %v", err)
	}
	if !strings.Contains(outAutoOmp.String(), "choice: omp / provider/omp-model (provider omp)") ||
		!strings.Contains(outAutoOmp.String(), "started AutoOmp: omp agent omp_") ||
		!strings.Contains(outAutoOmp.String(), "on provider/omp-model") {
		t.Fatalf("--harness auto omp start output:\n%s", outAutoOmp.String())
	}

	// 2d. Refusal when no choice has room (all choices excluded or blocked), and explicit --harness agy unchanged.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aNone, outNone := newApp()
	cmdNone := aNone.agentStartCmd()
	cmdNone.SilenceErrors = true
	cmdNone.SilenceUsage = true
	cmdNone.SetArgs([]string{"NoChoiceWorker", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy, testExcludeFlag, omp.Harness})
	err = cmdNone.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: no choice has room: claude/claude-opus-4-7: usage high: 5h 96% at or past 95%") ||
		!strings.Contains(err.Error(), "agy/gemini-3-1-pro-medium: excluded") ||
		!strings.Contains(err.Error(), "omp/provider/omp-model: excluded") {
		t.Fatalf("--harness auto with no choice having room: code=%d, err=%v", Code(err), err)
	}
	if outNone.String() != "" {
		t.Fatalf("--harness auto refusal printed output:\n%s", outNone.String())
	}

	// Explicit --harness agy --model claude-opus-4-6-thinking still starts on agy as before.
	aExpAgy, outExpAgy := newApp()
	cmdExpAgy := aExpAgy.agentStartCmd()
	cmdExpAgy.SilenceErrors = true
	cmdExpAgy.SilenceUsage = true
	cmdExpAgy.SetArgs([]string{"ExplicitAgy", brief, usageDirFlag, dir, testHarnessFlag, harnessAgy, modelFlag, testModelOpus46})
	if err := cmdExpAgy.Execute(); err != nil {
		t.Fatalf("explicit --harness agy: %v", err)
	}
	if strings.Contains(outExpAgy.String(), "choice:") ||
		!strings.Contains(outExpAgy.String(), "started ExplicitAgy: agy agent agy_") ||
		!strings.Contains(outExpAgy.String(), "on claude-opus-4-6-thinking") {
		t.Fatalf("explicit --harness agy output:\n%s", outExpAgy.String())
	}

	// 2e. Regression coverage for round-2 review findings:
	//     - --harness auto with --model evaluates the requested model against choices, exclusions, and its own bucket;
	//     - Claude preflight validates the claude executable before printing choice:/started over or recording a force event;
	//     - brokered OMP validates preflight before delegating and does not print choice: when preflight or the broker fails.
	cfg.Capacity.Choices = []config.Choice{
		{Harness: claudeComm, Model: testModelOpus47},
		{Harness: harnessAgy, Model: testModelOpus46, SkipPace: true},
		{Harness: harnessAgy, Model: testModelGemini, SkipPace: true},
		{Harness: omp.Harness, Model: "provider/omp-model", Provider: omp.Harness, SkipPace: true},
	}
	rawCfgWithOpus46 := strings.Replace(rawCfg,
		"    - {harness: agy, model: gemini-3-1-pro-medium, skipPace: true}\n",
		"    - {harness: agy, model: claude-opus-4-6-thinking, skipPace: true}\n    - {harness: agy, model: gemini-3-1-pro-medium, skipPace: true}\n", 1)
	if err := os.WriteFile(cfgPath, []byte(rawCfgWithOpus46), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	agyClaude5h = 96.0 // agy Claude bucket is in USAGE HIGH (96%), while Gemini bucket is 10% (eligible)

	// Unlisted --model with --harness auto is rejected with ExitUsage and empty output.
	aUnknownModel, outUnknownModel := newApp()
	cmdUnknownModel := aUnknownModel.agentStartCmd()
	cmdUnknownModel.SilenceErrors = true
	cmdUnknownModel.SilenceUsage = true
	cmdUnknownModel.SetArgs([]string{"AutoUnknownModel", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, modelFlag, "not-in-choices"})
	err = cmdUnknownModel.Execute()
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "not in capacity.choices") || outUnknownModel.String() != "" {
		t.Fatalf("--harness auto with unknown --model: code=%d, err=%v, out=%q", Code(err), err, outUnknownModel.String())
	}

	// --exclude claude-opus-4-6-thinking --model claude-opus-4-6-thinking refuses in both dry-run and real start (even with --force), never announcing Gemini. The refusal comes before any verdict: its words are in its error, never in its output.
	for _, isDry := range []bool{true, false} {
		aExModel, outExModel := newApp()
		cmdExModel := aExModel.agentStartCmd()
		cmdExModel.SilenceErrors = true
		cmdExModel.SilenceUsage = true
		args := []string{"AutoExModel", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, testExcludeFlag, testModelOpus46, modelFlag, testModelOpus46}
		if isDry {
			args = append(args, testDryRunFlag)
		}
		cmdExModel.SetArgs(args)
		err = cmdExModel.Execute()
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), "excluded") || outExModel.String() != "" {
			t.Fatalf("--harness auto excluded --model (dry=%v): code=%d, err=%v, out=%q", isDry, Code(err), err, outExModel.String())
		}
	}

	// --model claude-opus-4-6-thinking without --force refuses because its Claude bucket is at 96% (even though Gemini is at 10%), in both dry-run and real start.
	for _, isDry := range []bool{true, false} {
		aHighModel, outHighModel := newApp()
		cmdHighModel := aHighModel.agentStartCmd()
		cmdHighModel.SilenceErrors = true
		cmdHighModel.SilenceUsage = true
		args := []string{"AutoHighModel", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, modelFlag, testModelOpus46}
		if isDry {
			args = append(args, testDryRunFlag)
		}
		cmdHighModel.SetArgs(args)
		err = cmdHighModel.Execute()
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), "agy/claude-opus-4-6-thinking: usage high: 5h 96% at or past 95%") || outHighModel.String() != "" {
			t.Fatalf("--harness auto blocked bucket --model (dry=%v): code=%d, err=%v, out=%q", isDry, Code(err), err, outHighModel.String())
		}
	}

	// With --force, --model claude-opus-4-6-thinking prints choice: agy / claude-opus-4-6-thinking (the pair actually dispatched).
	aForceModelDry, outForceModelDry := newApp()
	cmdForceModelDry := aForceModelDry.agentStartCmd()
	cmdForceModelDry.SilenceErrors = true
	cmdForceModelDry.SilenceUsage = true
	cmdForceModelDry.SetArgs([]string{"AutoForceModelDry", brief, usageDirFlag, dir, testDryRunFlag, testHarnessFlag, harnessAuto, modelFlag, testModelOpus46, usageForceFlag})
	if err := cmdForceModelDry.Execute(); err != nil {
		t.Fatalf("--dry-run --harness auto --model --force: %v", err)
	}
	if !strings.Contains(outForceModelDry.String(), "choice: agy / claude-opus-4-6-thinking (provider agy, bucket Claude and GPT models)") ||
		strings.Contains(outForceModelDry.String(), testModelGemini) {
		t.Fatalf("--dry-run --harness auto --model --force output:\n%s", outForceModelDry.String())
	}

	// Claude preflight: with claude absent from PATH, an auto start with --force --mode background fails before printing choice:/started over or recording an event.
	t.Setenv("PATH", binDir) // binDir has fake agy and omp, but no claude
	evsBeforeNoClaude, err := store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	aNoClaude, outNoClaude := newApp()
	cmdNoClaude := aNoClaude.agentStartCmd()
	cmdNoClaude.SilenceErrors = true
	cmdNoClaude.SilenceUsage = true
	cmdNoClaude.SetArgs([]string{"AutoNoClaude", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, testHarnessFlag, harnessAuto, modelFlag, testModelOpus47, usageForceFlag})
	if err := cmdNoClaude.Execute(); err == nil {
		t.Fatal("expected error when claude is absent from PATH")
	}
	if outNoClaude.String() != "" {
		t.Fatalf("missing claude preflight printed output:\n%s", outNoClaude.String())
	}
	evsAfterNoClaude, err := store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evsAfterNoClaude) != len(evsBeforeNoClaude) {
		t.Fatalf("missing claude preflight recorded state event: %+v", evsAfterNoClaude[len(evsBeforeNoClaude):])
	}

	// Brokered OMP preflight and unavailable broker: no premature choice: output on invalid dir or unavailable broker.
	cfg.Secret.Session = true
	cfg.Omp.ModelsFile = filepath.Join(dir, "omp-models.yml")
	if err := os.WriteFile(cfg.Omp.ModelsFile, []byte("providers: {provider: {apiKey: PROVIDER_API_KEY}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Omp.Providers = map[string]config.OmpProvider{"provider": {APIKey: sparkRef}}
	aBrokerBadDir, outBrokerBadDir := newApp()
	cmdBrokerBadDir := aBrokerBadDir.agentStartCmd()
	cmdBrokerBadDir.SilenceErrors = true
	cmdBrokerBadDir.SilenceUsage = true
	cmdBrokerBadDir.SetArgs([]string{"AutoBrokerBadDir", brief, usageDirFlag, filepath.Join(dir, "nonexistent"), testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy})
	err = cmdBrokerBadDir.Execute()
	if Code(err) != ExitUsage || outBrokerBadDir.String() != "" {
		t.Fatalf("brokered omp invalid dir: code=%d, err=%v, out=%q", Code(err), err, outBrokerBadDir.String())
	}

	aBrokerUnavail, outBrokerUnavail := newApp()
	cmdBrokerUnavail := aBrokerUnavail.agentStartCmd()
	cmdBrokerUnavail.SilenceErrors = true
	cmdBrokerUnavail.SilenceUsage = true
	cmdBrokerUnavail.SetArgs([]string{"AutoBrokerUnavail", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy})
	err = cmdBrokerUnavail.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no sandbox broker answers") || outBrokerUnavail.String() != "" {
		t.Fatalf("brokered omp unavailable broker: code=%d, err=%v, out=%q", Code(err), err, outBrokerUnavail.String())
	}
}

// TestCachedProvidersCapacityAndWatcherCurrentUsage verifies that:
//   - cachedUsageCapacity treats any enabled provider with a missing, stale, or
//     invalid reading as 0 floor and 0 ceiling (never falling back to raw k);
//   - watcher.currentUsage propagates provider errors so watcher.capacity
//     suppresses CAPACITY LOW when provider usage is unreadable;
//   - watchProvidersUsage with !w.chores reuses the resolved reading in memory
//     even when w.store == nil.
func TestCachedProvidersCapacityAndWatcherCurrentUsage(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `capacity:
  floor: 3
  ceiling: 6
  providers:
    claude:
      floor: 2
      ceiling: 4
      staleAfter: 15m
      command: ["read-claude"]
      fiveHour:
        - {upTo: 90, floor: 2, ceiling: 4}
    agy:
      floor: 1
      ceiling: 2
      staleAfter: 15m
      command: ["read-agy"]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	a := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &bytes.Buffer{}}

	// 1. No cached files for either enabled provider: floor 0, ceiling 0 (never raw 3, 6), u.Error set.
	derived0, u0 := a.cachedUsageCapacity(cfg.Capacity)
	if derived0.Floor != 0 || derived0.Ceiling != 0 || u0 == nil || u0.Error == "" {
		t.Fatalf("missing cache: derived=(%d,%d), u=%+v; want (0,0) and error", derived0.Floor, derived0.Ceiling, u0)
	}

	// 2. Write fresh reading for agy (floor 1, ceiling 2) and stale reading for claude:
	//    claude contributes (0,0), agy contributes (1,2), and u.Error names claude.
	staleReading := usageReading{
		At:       t0.Add(-20 * time.Minute),
		FiveHour: &usageWindow{UsedPercent: 10, ResetsAt: t0.Add(2 * time.Hour)},
	}
	freshAgy := usageReading{
		At:       t0.Add(-1 * time.Minute),
		FiveHour: &usageWindow{UsedPercent: 10, ResetsAt: t0.Add(2 * time.Hour)},
	}
	if err := store.WriteFile(usageProviderFile(claudeComm), staleReading); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFile(usageProviderFile(harnessAgy), freshAgy); err != nil {
		t.Fatal(err)
	}
	derived1, u1 := a.cachedUsageCapacity(cfg.Capacity)
	if derived1.Floor != 1 || derived1.Ceiling != 2 || u1 == nil || !strings.Contains(u1.Error, "claude: usage reading is stale") {
		t.Fatalf("stale claude + fresh agy: derived=(%d,%d), u=%+v; want (1,2) and claude stale error", derived1.Floor, derived1.Ceiling, u1)
	}

	// 3. Make claude fresh as well, then set w.providerUsageErr["claude"] on watcher:
	//    w.currentUsage returns error and w.capacity suppresses CAPACITY LOW.
	freshClaude := usageReading{
		At:       t0.Add(-1 * time.Minute),
		FiveHour: &usageWindow{UsedPercent: 10, ResetsAt: t0.Add(2 * time.Hour)},
	}
	if err := store.WriteFile(usageProviderFile(claudeComm), freshClaude); err != nil {
		t.Fatal(err)
	}
	var watchOut bytes.Buffer
	aWatch := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &watchOut}
	w := aWatch.newWatcher(false, true)
	w.now = t0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { return roomy() }
	w.providerUsageErr = map[string]string{claudeComm: "endpoint timeout"}

	kw, uw := w.currentUsage(cfg.Capacity, t0)
	if kw.Floor != 1 || kw.Ceiling != 2 || uw == nil || !strings.Contains(uw.Error, "claude: endpoint timeout") {
		t.Fatalf("watcher.currentUsage with providerUsageErr: kw=(%d,%d), uw=%+v", kw.Floor, kw.Ceiling, uw)
	}
	w.capacity(context.Background(), &state.State{}, nil)
	if strings.Contains(watchOut.String(), "CAPACITY LOW") {
		t.Fatalf("watcher.capacity emitted CAPACITY LOW on unreadable provider usage:\n%s", watchOut.String())
	}

	// 4. Nit check: watchProvidersUsage with !w.chores and w.store == nil reuses the in-memory reading.
	var storelessOut bytes.Buffer
	aStoreless := &app{
		cfgPath: cfgPath,
		cfg:     cfg,
		store:   nil,
		now:     t0,
		out:     &storelessOut,
		readUsage: func(_ context.Context, _ []string, _ time.Duration) (usageReading, error) {
			return freshClaude, nil
		},
	}
	wOnce := aStoreless.newWatcher(false, false)
	wOnce.now = t0
	wOnce.readHeadroom = func(context.Context, *swapReading) *headroom { return roomy() }
	wOnce.watchProvidersUsage(context.Background(), cfg.Capacity, &state.State{}, nil)
	if !strings.Contains(storelessOut.String(), "USAGE: claude: ") ||
		!strings.Contains(storelessOut.String(), "usage 5h 10%") ||
		strings.Contains(storelessOut.String(), "unreadable") {
		t.Fatalf("storeless watch --once output:\n%s", storelessOut.String())
	}
}

// TestProvidersAndBucketsUsageHighResumeAndWatch verifies that:
//   - multi-bucket USAGE HIGH tracks and emits USAGE HIGH per high bucket,
//     telling only busy workers of that high bucket to finish their step and park;
//   - agents resume and watcher.autoResume scope USAGE HIGH to the specific
//     agent's provider and bucket (blocking resume on a high provider/bucket
//     even when another provider or bucket has room, while allowing resume on
//     non-high providers/buckets).
func TestProvidersAndBucketsUsageHighResumeAndWatch(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)

	rawCfg := `agents:
  autoResume: true
capacity:
  floor: 2
  ceiling: 6
  providers:
    claude:
      floor: 1
      ceiling: 2
      command: ["read-claude"]
      highPercent: 95
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
    agy:
      floor: 1
      ceiling: 2
      command: ["read-agy"]
      highPercent: 95
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
    local:
      floor: 1
      ceiling: 2
  choices:
    - {harness: claude, model: claude-opus-4-7}
    - {harness: agy, model: claude-opus-4-6-thinking}
    - {harness: agy, model: gemini-3-1-pro-medium}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	reset5h := t0.Add(2 * time.Hour).UTC()
	reset7d := t0.Add(84 * time.Hour).UTC()

	sup := tasked("Supervisor run 1")
	busyClaude := tasked("BusyClaude")
	busyClaude.Harness, busyClaude.Model = claudeComm, testModelOpus47
	busyAgyClaude := tasked("BusyAgyClaude")
	busyAgyClaude.Harness, busyAgyClaude.Model = harnessAgy, testModelOpus46
	busyAgyGemini := tasked("BusyAgyGemini")
	busyAgyGemini.Harness, busyAgyGemini.Model = harnessAgy, testModelGemini

	parkedClaude := tasked("ParkedClaude")
	parkedClaude.Harness, parkedClaude.Model = claudeComm, testModelOpus47
	parkedClaude.Park = &state.Park{By: parkedClaude.Party, At: t0.UTC(), On: "#1", Waits: "note 1", Told: t0.UTC(), Resumable: t0.UTC(), Answer: "go"}

	parkedAgyClaude := tasked("ParkedAgyClaude")
	parkedAgyClaude.Harness, parkedAgyClaude.Model = harnessAgy, testModelOpus46
	parkedAgyClaude.Park = &state.Park{By: parkedAgyClaude.Party, At: t0.UTC(), On: "#2", Waits: "note 2", Told: t0.UTC(), Resumable: t0.UTC(), Answer: "go"}

	parkedAgyGemini := tasked("ParkedAgyGemini")
	parkedAgyGemini.Harness, parkedAgyGemini.Model = harnessAgy, testModelGemini
	parkedAgyGemini.Park = &state.Park{By: parkedAgyGemini.Party, At: t0.UTC(), On: "#3", Waits: "note 3", Told: t0.UTC(), Resumable: t0.UTC(), Answer: "go"}

	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: sup.Party, Since: t0.UTC()}
		st.Agents = []state.Agent{sup, busyClaude, busyAgyClaude, busyAgyGemini, parkedClaude, parkedAgyClaude, parkedAgyGemini}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	var woke []string
	origWake := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, _ state.Party, name, msg, _ string) error {
		woke = append(woke, name+" :: "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = origWake })

	claude5h := 30.0
	agyClaude5h := 96.0 // HIGH on agy's Claude bucket!
	agyGemini5h := 10.0 // Not high on agy's Gemini bucket

	reader := func(_ context.Context, cmd []string, _ time.Duration) (usageReading, error) {
		if len(cmd) > 0 && cmd[0] == testReadClaude {
			return usageReading{
				FiveHour: &usageWindow{UsedPercent: claude5h, ResetsAt: reset5h},
				SevenDay: &usageWindow{UsedPercent: 30, ResetsAt: reset7d},
			}, nil
		}
		return usageReading{
			Buckets: map[string]usageBucket{
				config.DefaultAgyClaudeBucket: {
					FiveHour: &usageWindow{UsedPercent: agyClaude5h, ResetsAt: reset5h},
					SevenDay: &usageWindow{UsedPercent: 30, ResetsAt: reset7d},
				},
				config.DefaultAgyGeminiBucket: {
					FiveHour: &usageWindow{UsedPercent: agyGemini5h, ResetsAt: reset5h},
					SevenDay: &usageWindow{UsedPercent: 20, ResetsAt: reset7d},
				},
			},
			BucketOrder: []string{config.DefaultAgyGeminiBucket, config.DefaultAgyClaudeBucket},
		}, nil
	}

	sessions := []*claude.Session{
		{ID: sup.Session, LastActive: t0},
		{ID: "s-BusyClaude", LastActive: t0},
		{ID: "s-BusyAgyClaude", LastActive: t0},
		{ID: "s-BusyAgyGemini", LastActive: t0},
	}

	// 1. Manual `agents resume` refuses ParkedAgyClaude (whose bucket is in USAGE HIGH),
	//    even though claude, local, and agy/Gemini Models are not high.
	aResume := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &bytes.Buffer{}, as: usageCaller, readUsage: reader}
	resAgyClaude := aResume.agentResumeCmd()
	resAgyClaude.SilenceErrors = true
	resAgyClaude.SilenceUsage = true
	resAgyClaude.SetArgs([]string{"ParkedAgyClaude"})
	err = resAgyClaude.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no resume: provider agy (Claude and GPT models): usage high: 5h 96% at or past 95%") {
		t.Fatalf("resume ParkedAgyClaude under bucket USAGE HIGH: code=%d, err=%v", Code(err), err)
	}

	// 2. Watch tick:
	//    - emits USAGE HIGH (agy/Claude and GPT models): 5h 96% at or past 95%;
	//    - tells ONLY BusyAgyClaude to finish its step and park;
	//    - autoResume wakes ParkedClaude and ParkedAgyGemini, while keeping ParkedAgyClaude parked without AGENT RESUME FAILED.
	var watchOut bytes.Buffer
	aWatch := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &watchOut, as: usageCaller, readUsage: reader}
	w := aWatch.newWatcher(false, true)
	w.now = t0
	w.doctoring.Store(true)
	w.readHeadroom = func(context.Context, *swapReading) *headroom { return roomy() }
	t.Cleanup(func() { w.bg.Wait() })
	w.pending(context.Background(), sessions)

	gotWatch := watchOut.String()
	if !strings.Contains(gotWatch, "USAGE HIGH (agy/Claude and GPT models): 5h 96% at or past 95%") ||
		strings.Contains(gotWatch, "USAGE HIGH (agy/Gemini Models)") ||
		strings.Contains(gotWatch, "USAGE HIGH (claude)") ||
		strings.Contains(gotWatch, "AGENT RESUME FAILED") {
		t.Fatalf("watch bucket USAGE HIGH output:\n%s", gotWatch)
	}
	if !slices.ContainsFunc(woke, func(s string) bool {
		return strings.HasPrefix(s, "BusyAgyClaude :: USAGE HIGH (5h 96% at or past 95%")
	}) {
		t.Fatalf("BusyAgyClaude was not told to park on bucket USAGE HIGH: woke=%q", woke)
	}
	if slices.ContainsFunc(woke, func(s string) bool {
		return strings.HasPrefix(s, "BusyClaude ::") || strings.HasPrefix(s, "BusyAgyGemini ::")
	}) {
		t.Fatalf("non-high workers were told to park: woke=%q", woke)
	}
	if !slices.ContainsFunc(woke, func(s string) bool { return strings.HasPrefix(s, "ParkedClaude ::") }) ||
		!slices.ContainsFunc(woke, func(s string) bool { return strings.HasPrefix(s, "ParkedAgyGemini ::") }) ||
		slices.ContainsFunc(woke, func(s string) bool { return strings.HasPrefix(s, "ParkedAgyClaude ::") }) {
		t.Fatalf("autoResume per-provider/bucket wake mismatch: woke=%q", woke)
	}

	// 3. Now claude enters USAGE HIGH (97%) while agy's buckets are both low (10%) and local is disabled (!Enabled()):
	//    manual `agents resume` on a parked claude worker refuses, and autoResume holds it while resuming ParkedAgyClaude.
	claude5h = 97.0
	agyClaude5h = 10.0
	_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(claudeComm)))
	_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(harnessAgy)))
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Agents {
			if st.Agents[i].Name == "ParkedClaude" {
				st.Agents[i].Park = &state.Park{By: parkedClaude.Party, At: t0.UTC(), On: "#1", Waits: "note 1", Told: t0.UTC(), Resumable: t0.UTC(), Answer: "go"}
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aResume2 := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0.Add(6 * time.Minute), out: &bytes.Buffer{}, as: usageCaller, readUsage: reader}
	resClaude := aResume2.agentResumeCmd()
	resClaude.SilenceErrors = true
	resClaude.SilenceUsage = true
	resClaude.SetArgs([]string{"ParkedClaude"})
	err = resClaude.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no resume: provider claude: usage high: 5h 97% at or past 95%") {
		t.Fatalf("resume ParkedClaude under claude USAGE HIGH: code=%d, err=%v", Code(err), err)
	}
}

func TestSandboxedAutoExcludeSerializationAndDispatch(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	origMachine := plat.Machine
	plat.Machine = tableMachine{plat.Machine}
	t.Cleanup(func() { plat.Machine = origMachine })

	rawCfg := `capacity:
  floor: 1
  ceiling: 4
  providers:
    claude:
      floor: 1
      ceiling: 2
    agy:
      floor: 1
      ceiling: 2
  choices:
    - {harness: claude, model: claude-opus-4-7}
    - {harness: agy, model: gemini-3-1-pro-medium}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Fix https://github.com/giantswarm/beekeeper/issues/1684\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	a := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &bytes.Buffer{}, as: usageCaller}

	// 1. scratchArgv serializes --exclude without square brackets so a brokered child
	//    agentStartCmd excludes claude and dispatches agy.
	parentCmd := a.agentStartCmd()
	if err := parentCmd.ParseFlags([]string{testHarnessFlag, harnessAuto, "--exclude", claudeComm, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	argv, err := a.scratchArgv(parentCmd, []string{"SandboxedAuto1", brief})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "--exclude=claude") || slices.Contains(argv, "--exclude=[claude]") {
		t.Fatalf("scratchArgv serialized --exclude with brackets: %q", argv)
	}
	childArgs, _, err := scratchConfig(argv[1:])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a.out = &out
	childCmd := a.agentStartCmd()
	childCmd.SilenceErrors = true
	childCmd.SilenceUsage = true
	childCmd.SetArgs(childArgs)
	if err := childCmd.Execute(); err != nil {
		t.Fatalf("brokered child agentStartCmd failed: %v", err)
	}
	if !strings.Contains(out.String(), "choice: agy / gemini-3-1-pro-medium") {
		t.Fatalf("brokered child did not exclude claude:\n%s", out.String())
	}

	// 2. Even if bracketed --exclude=[claude] is passed directly, normalizeExcludes strips brackets.
	out.Reset()
	bracketCmd := a.agentStartCmd()
	bracketCmd.SilenceErrors = true
	bracketCmd.SilenceUsage = true
	bracketCmd.SetArgs([]string{testHarnessFlag, harnessAuto, "--exclude=[claude]", "--dry-run", "SandboxedAuto2", brief})
	if err := bracketCmd.Execute(); err != nil {
		t.Fatalf("bracketed --exclude failed: %v", err)
	}
	if !strings.Contains(out.String(), "choice: agy / gemini-3-1-pro-medium") {
		t.Fatalf("bracketed --exclude=[claude] did not exclude claude:\n%s", out.String())
	}
}

func TestAutoModelFilterCountsBusyAgentsAcrossAllChoices(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	origMachine := plat.Machine
	plat.Machine = tableMachine{plat.Machine}
	t.Cleanup(func() { plat.Machine = origMachine })

	rawCfg := `capacity:
  floor: 1
  ceiling: 4
  providers:
    shared-agy:
      floor: 1
      ceiling: 1
  choices:
    - {harness: agy, model: gemini-3-1-pro-medium, provider: shared-agy}
    - {harness: agy, model: claude-opus-4-6-thinking, provider: shared-agy}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sup := tasked("Supervisor run 1")
	busyGemini := tasked("BusyGemini")
	busyGemini.Harness, busyGemini.Model = harnessAgy, testModelGemini
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: sup.Party, Since: t0.UTC()}
		st.Agents = []state.Agent{sup, busyGemini}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Fix https://github.com/giantswarm/beekeeper/issues/1684\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// BusyGemini runs on gemini-3-1-pro-medium, mapped via choices[0] to provider shared-agy (ceiling 1).
	// Requesting --harness auto --model claude-opus-4-6-thinking must still attribute BusyGemini to
	// shared-agy before filtering candidate choices to claude-opus-4-6-thinking, and refuse the start.
	a := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &bytes.Buffer{}, as: usageCaller}
	cmd := a.agentStartCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{testHarnessFlag, harnessAuto, modelFlag, testModelOpus46, "BlockedWorker", brief})
	err = cmd.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "1 busy at the ceiling of 1") {
		t.Fatalf("expected refusal when shared provider ceiling 1 is held by another choice's model: code=%d, err=%v", Code(err), err)
	}
}

func TestCodexProviderUsageAndAutoHarness(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	origLauncher := plat.Launcher
	origMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origMachine
	})
	plat.Machine = tableMachine{plat.Machine}

	rawCfg := `codex:
  model: gpt-5.4
capacity:
  floor: 1
  ceiling: 4
  providers:
    claude:
      floor: 1
      ceiling: 2
      command: ["read-claude"]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
      sevenDay:
        - {upTo: 95, floor: 1, ceiling: 2}
    codex:
      floor: 1
      ceiling: 2
      staleAfter: 15m
      fiveHourMax: 90
      sevenDayMax: 95
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 2}
      sevenDay:
        - {upTo: 95, floor: 1, ceiling: 2}
  choices:
    - {harness: claude, model: claude-opus-4-7}
    - {harness: codex, model: gpt-5.4, sevenDayMax: 80, paceMargin: 0}
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Capacity.Providers[codex.Harness].Command, config.DefaultCodexUsageCommand) {
		t.Fatalf("providers.codex.command = %v, want %v", cfg.Capacity.Providers[codex.Harness].Command, config.DefaultCodexUsageCommand)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	reset5h := t0.Add(3 * time.Hour).UTC()
	// 84h remaining of 168h => 50% elapsed weekly pace.
	reset7d := t0.Add(84 * time.Hour).UTC()

	if err := store.WriteFile(usageProviderFile(claudeComm), usageReading{
		At:       t0,
		FiveHour: &usageWindow{UsedPercent: 96, ResetsAt: reset5h},
		SevenDay: &usageWindow{UsedPercent: 40, ResetsAt: reset7d},
	}); err != nil {
		t.Fatal(err)
	}

	writeCodexRollout := func(baseDir, rel string, mod time.Time, lines ...string) {
		t.Helper()
		p := filepath.Join(baseDir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(p), 0o750); mkErr != nil {
			t.Fatal(mkErr)
		}
		if wErr := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); wErr != nil {
			t.Fatal(wErr)
		}
		if !mod.IsZero() {
			_ = os.Chtimes(p, mod, mod)
		}
	}

	// 1. No rollout files yet -> codex fails closed (ceiling 0, room 0).
	var capOut bytes.Buffer
	aCap := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &capOut, as: usageCaller}
	capCmd := aCap.capacityCmd()
	if err := capCmd.Execute(); err != nil {
		t.Fatalf("capacity with no codex rollouts: %v", err)
	}
	if !strings.Contains(capOut.String(), "provider codex: 0 busy of floor 0, ceiling 0; usage unknown (no codex rollout files found); no start: subscription usage unknown (no codex rollout files found)") {
		t.Fatalf("expected codex fail-closed when no rollouts exist, got:\n%s", capOut.String())
	}

	// 2. Session A has a valid dual-window reading (5h 18%, 7d 40% vs pace 50%), while newer Session B
	//    was just started and has written only session_meta + turn_context -> codex uses Session A's reading!
	writeCodexRollout(cfg.Codex.SessionsDir, "2026/10/28/rollout-a.jsonl", t0.Add(-2*time.Minute),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":18,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":40,"window_minutes":10080,"resets_at":%d},"plan_type":"plus","rate_limit_reached_type":null,"spend_control_reached":false}}}`,
			t0.Add(-2*time.Minute).Format(time.RFC3339), reset5h.Unix(), reset7d.Unix()),
	)
	writeCodexRollout(codex.HomeSessionsDir(cfg.StateDir), "2026/10/28/rollout-b-meta.jsonl", t0,
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"t-meta","cwd":%q}}`, t0.Format(time.RFC3339), dir),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"cwd":%q,"model":"gpt-5.4"}}`, t0.Format(time.RFC3339), dir),
	)

	capOut.Reset()
	if err := aCap.capacityCmd().Execute(); err != nil {
		t.Fatalf("capacity with valid codex rollout + metadata-only newer rollout: %v", err)
	}
	if !strings.Contains(capOut.String(), "provider codex: 0 busy of floor 1, ceiling 2; usage 5h 18%") ||
		!strings.Contains(capOut.String(), "room for 2 starts") ||
		!strings.Contains(capOut.String(), "choice: codex / gpt-5.4 (provider codex)") {
		t.Fatalf("capacity output with dual-window codex rollout:\n%s", capOut.String())
	}

	// 3. Pro plan single-window reading (`primary` window_minutes 10080, `secondary` null) in a fresher event:
	//    succeeds even though fiveHour bands are configured, and enforces weekly pace / sevenDayMax.
	//    At 7d 85% (above worker sevenDayMax 80 and pace 50%, below provider sevenDayMax 95):
	//    the choice is blocked on 7d/pace, for a --review as for any start.
	writeCodexRollout(codex.HomeSessionsDir(cfg.StateDir), "2026/10/28/rollout-c-pro.jsonl", t0,
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":85,"window_minutes":10080,"resets_at":%d},"secondary":null,"plan_type":"pro","rate_limit_reached_type":null,"spend_control_reached":false}}}`,
			t0.Add(-time.Minute).Format(time.RFC3339), reset7d.Unix()),
	)
	capOut.Reset()
	if err := aCap.capacityCmd().Execute(); err != nil {
		t.Fatalf("capacity on Pro single-window codex reading: %v", err)
	}
	if !strings.Contains(capOut.String(), "provider codex: 0 busy of floor 1, ceiling 2; usage 7d 85%") ||
		!strings.Contains(capOut.String(), "choice: none (claude/claude-opus-4-7: usage high: 5h 96% at or past 95%") ||
		!strings.Contains(capOut.String(), "codex/gpt-5.4: 7d 85% at or past worker cap 80%") {
		t.Fatalf("capacity normal mode at 85%% 7d:\n%s", capOut.String())
	}

	capOut.Reset()
	revCmd := aCap.capacityCmd()
	revCmd.SetArgs([]string{"--" + reviewFlag})
	if err := revCmd.Execute(); err != nil {
		t.Fatalf("capacity --review on Pro single-window codex reading: %v", err)
	}
	if !strings.Contains(capOut.String(), "codex/gpt-5.4: 7d 85% at or past worker cap 80%") {
		t.Fatalf("capacity --review should be blocked like any start:\n%s", capOut.String())
	}

	// 4. Lower 7d to 40% (within pace 50% and worker cap 80%), then test `agents start --harness auto`
	//    in both --dry-run and real unit launch modes.
	writeCodexRollout(codex.HomeSessionsDir(cfg.StateDir), "2026/10/28/rollout-c-pro.jsonl", t0,
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":40,"window_minutes":10080,"resets_at":%d},"secondary":null,"plan_type":"pro","rate_limit_reached_type":null,"spend_control_reached":false}}}`,
			t0.Format(time.RFC3339), reset7d.Unix()),
	)

	binDir := t.TempDir()
	fakeCodex := filepath.Join(binDir, codex.Comm)
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the codex task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var dryOut bytes.Buffer
	aDry := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &dryOut, as: usageCaller}
	dryCmd := aDry.agentStartCmd()
	dryCmd.SilenceErrors = true
	dryCmd.SilenceUsage = true
	dryCmd.SetArgs([]string{"DryCodexAuto", brief, usageDirFlag, dir, testDryRunFlag, testHarnessFlag, harnessAuto})
	if err := dryCmd.Execute(); err != nil {
		t.Fatalf("--dry-run --harness auto picking codex: %v", err)
	}
	if !strings.Contains(dryOut.String(), "choice: codex / gpt-5.4 (provider codex)") ||
		!strings.Contains(dryOut.String(), "as a codex agent on gpt-5.4") {
		t.Fatalf("--dry-run --harness auto codex output:\n%s", dryOut.String())
	}

	plat.Launcher = &agyTestLauncher{
		onStart: func(u platform.Unit) {
			go func() {
				var startID string
				for _, e := range u.Env {
					if v, ok := strings.CutPrefix(e, codex.EnvAgent+"="); ok {
						startID = v
					}
				}
				if startID == "" {
					return
				}
				inbox := codex.InboxPath(cfg.StateDir, startID)
				logPath := codex.LogPath(cfg.StateDir, startID)
				f, oerr := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
				if oerr != nil {
					return
				}
				defer func() { _ = f.Close() }()
				_, _ = bufio.NewReader(f).ReadString('\n')
				stream := `{"type":"thread.started","thread_id":"thread-auto-codex"}` + "\n" +
					`{"type":"turn.started"}` + "\n"
				_ = os.WriteFile(logPath, []byte(stream), 0o600)
			}()
		},
	}

	var startOut bytes.Buffer
	aStart := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &startOut, as: usageCaller}
	startCmd := aStart.agentStartCmd()
	startCmd.SilenceErrors = true
	startCmd.SilenceUsage = true
	startCmd.SetArgs([]string{"AutoCodexWorker", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto})
	if err := startCmd.Execute(); err != nil {
		t.Fatalf("--harness auto picking codex: %v", err)
	}
	if !strings.Contains(startOut.String(), "choice: codex / gpt-5.4 (provider codex)") ||
		!strings.Contains(startOut.String(), "started AutoCodexWorker: codex agent codex_") ||
		!strings.Contains(startOut.String(), "on gpt-5.4") {
		t.Fatalf("--harness auto codex start output:\n%s", startOut.String())
	}

	// 5. Explicit stopped or invalid reading in the newest rollout event fails closed (room 0),
	//    and a reading older than staleAfter (15m) also fails closed as stale.
	writeCodexRollout(codex.HomeSessionsDir(cfg.StateDir), "2026/10/28/rollout-d-stopped.jsonl", t0.Add(time.Minute),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":50,"window_minutes":10080,"resets_at":%d},"spend_control_reached":true}}}`,
			t0.Add(time.Minute).Format(time.RFC3339), reset7d.Unix()),
	)
	capOut.Reset()
	aStopped := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0.Add(time.Minute), out: &capOut, as: usageCaller}
	if err := aStopped.capacityCmd().Execute(); err != nil {
		t.Fatalf("capacity on stopped codex reading: %v", err)
	}
	if !strings.Contains(capOut.String(), "provider codex: 1 busy of floor 0, ceiling 0: AutoCodexWorker; usage unknown (codex spend control reached); no start: subscription usage unknown (codex spend control reached)") {
		t.Fatalf("expected codex spend_control_reached to fail closed, got:\n%s", capOut.String())
	}

	_ = os.Remove(filepath.Join(codex.HomeSessionsDir(cfg.StateDir), "2026/10/28/rollout-d-stopped.jsonl"))
	capOut.Reset()
	aStale := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0.Add(20 * time.Minute), out: &capOut, as: usageCaller}
	if err := aStale.capacityCmd().Execute(); err != nil {
		t.Fatalf("capacity on stale codex reading: %v", err)
	}
	if !strings.Contains(capOut.String(), "provider codex: 1 busy of floor 0, ceiling 0: AutoCodexWorker; usage unknown (usage reading is stale") {
		t.Fatalf("expected stale codex reading to fail closed, got:\n%s", capOut.String())
	}

	// 6. Future SourceAt timestamp (> now + 1m) is rejected by validateReadingAt and fails closed.
	tFuture := t0.Add(10 * time.Minute)
	writeCodexRollout(codex.HomeSessionsDir(cfg.StateDir), "2026/10/28/rollout-e-future.jsonl", tFuture,
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"window_minutes":10080,"resets_at":%d},"secondary":null,"plan_type":"pro","rate_limit_reached_type":null,"spend_control_reached":false}}}`,
			tFuture.Format(time.RFC3339), reset7d.Unix()),
	)
	capOut.Reset()
	aFuture := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &capOut, as: usageCaller}
	if err := aFuture.capacityCmd().Execute(); err != nil {
		t.Fatalf("capacity on future codex reading: %v", err)
	}
	if !strings.Contains(capOut.String(), "provider codex: 1 busy of floor 0, ceiling 0: AutoCodexWorker; usage unknown (usage reading timestamp 2026-10-28T15:10:00Z is in the future)") {
		t.Fatalf("expected future codex reading to fail closed, got:\n%s", capOut.String())
	}
}

// selfHostedRawCfg is a desk with one subscription provider held at ceiling 0
// (its subscription ahead of its weekly pace) and one self-hosted provider on
// a model served on the machine, whose ceiling is its own.
const selfHostedRawCfg = `capacity:
  floor: 0
  ceiling: 0
  providers:
    claude:
      floor: 1
      ceiling: 2
    local-model:
      kind: self-hosted
      ceiling: 2
  choices:
    - harness: claude
      model: claude-opus-4-7
    - harness: omp
      model: local-model/worker-model
      provider: local-model
      skipPace: true
omp:
  model: local-model/worker-model
`

// selfHostedTestModel is the model selector of the self-hosted provider.
const selfHostedTestModel = "local-model/worker-model"

// serveModeFull is the full-power serving mode of the test mode reader.
const serveModeFull = "full"

// selfHostedProv is the self-hosted provider name of the resolution tests.
const selfHostedProv = "local-model"

// newSelfHostedApp builds the app a self-hosted capacity test starts from.
func newSelfHostedApp(cfgPath string, cfg *config.Config, store state.Store, out *bytes.Buffer, now time.Time) *app {
	return &app{
		cfgPath: cfgPath,
		cfg:     cfg,
		store:   store,
		now:     now,
		out:     out,
		as:      usageCaller,
	}
}

func selfHostedAgents(t *testing.T, store *state.FileStore, names ...string) {
	t.Helper()
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		for _, n := range names {
			w := tasked(n)
			w.Harness, w.Model = omp.Harness, selfHostedTestModel
			st.Agents = append(st.Agents, w)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSelfHostedProviderCeiling covers the first two Expected lines of the
// issue: a desk whose subscription is held at capacity.ceiling 0 still starts
// on its self-hosted provider, the self-hosted ceiling alone refuses a start
// past its own sessions at once, and a machine guard refuses both providers.
func TestSelfHostedProviderCeiling(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte(selfHostedRawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Capacity.Providers["local-model"]; !got.SelfHosted() || got.Ceiling != 2 {
		t.Fatalf("local-model provider = %+v, want self-hosted with its own ceiling of 2", got)
	}
	if got := cfg.Capacity.Ceiling; got != 0 {
		t.Fatalf("capacity.ceiling = %d, want the configured 0", got)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	// 1. capacity.ceiling 0: the subscription provider's starts are paused,
	//    the self-hosted provider's are not. It reads no subscription usage,
	//    so the self-hosted choice is the one left standing.
	var out bytes.Buffer
	a := newSelfHostedApp(cfgPath, cfg, store, &out, t0)
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\nroom for 2 starts") {
		t.Fatalf("capacity at subscription ceiling 0:\n%s", got)
	}
	if !strings.Contains(got, "provider local-model: 0 busy of ceiling 2; room for 2 starts") {
		t.Fatalf("self-hosted provider line:\n%s", got)
	}
	if !strings.Contains(got, "provider claude: 0 busy of floor 1, ceiling 2; no start: ceiling 0") {
		t.Fatalf("subscription provider line:\n%s", got)
	}
	// The subscription's choice has no room, so the self-hosted one is what a
	// start would get.
	if !strings.Contains(got, "choice: omp / local-model/worker-model (provider local-model)") {
		t.Fatalf("choice line:\n%s", got)
	}

	// --json shows the self-hosted provider's sessions and ceiling.
	var jsonOut bytes.Buffer
	aJSON := newSelfHostedApp(cfgPath, cfg, store, &jsonOut, t0)
	aJSON.json = true
	if err := aJSON.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	var parsed capacityView
	if err := json.Unmarshal(jsonOut.Bytes(), &parsed); err != nil {
		t.Fatalf("capacity --json: %v\n%s", err, jsonOut.String())
	}
	pv := parsed.Providers["local-model"]
	if pv == nil || pv.Kind != config.KindSelfHosted || pv.Ceiling != 2 || pv.Room != 2 {
		t.Fatalf("self-hosted provider in --json = %+v, want kind self-hosted, ceiling 2, room 2", pv)
	}
	if parsed.Room != 2 {
		t.Fatalf("room at subscription ceiling 0 = %d, want 2", parsed.Room)
	}

	// agents start --harness omp goes through on the self-hosted ceiling: its
	// start is judged against the self-hosted provider, not the held
	// subscription's ceiling.
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeOmp := filepath.Join(dir, omp.Comm)
	if err := os.WriteFile(fakeOmp, []byte("#!/bin/sh\necho '{\"models\":[{\"selector\":\"local-model/worker-model\"}]}'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	origLauncher := plat.Launcher
	origProcMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origProcMachine
	})
	plat.Machine = tableMachine{plat.Machine}
	plat.Launcher = &agyTestLauncher{onStart: func(u platform.Unit) {
		go func() {
			var ompID string
			for _, e := range u.Env {
				if v, ok := strings.CutPrefix(e, omp.EnvAgent+"="); ok {
					ompID = v
				}
			}
			if ompID == "" {
				return
			}
			f, oerr := os.OpenFile(omp.InboxPath(cfg.StateDir, ompID), os.O_RDWR, 0) //nolint:gosec // test FIFO
			if oerr != nil {
				return
			}
			defer func() { _ = f.Close() }()
			_, _ = bufio.NewReader(f).ReadString('\n')
		}()
	}}
	var startOut bytes.Buffer
	aStart := newSelfHostedApp(cfgPath, cfg, store, &startOut, t0)
	cmdStart := aStart.agentStartCmd()
	cmdStart.SilenceErrors = true
	cmdStart.SilenceUsage = true
	cmdStart.SetArgs([]string{"OmpOnHeldSubscription", brief, usageDirFlag, dir, testHarnessFlag, omp.Harness})
	if err := cmdStart.Execute(); err != nil {
		t.Fatalf("agents start --harness omp at subscription ceiling 0: %v\n%s", err, startOut.String())
	}
	if !strings.Contains(startOut.String(), "started OmpOnHeldSubscription: omp agent omp_") ||
		!strings.Contains(startOut.String(), "on local-model/worker-model") {
		t.Fatalf("omp start output:\n%s", startOut.String())
	}

	// agents start --harness auto picks the self-hosted choice while the
	// subscription's provider is held at ceiling 0.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var autoOut bytes.Buffer
	aAuto := newSelfHostedApp(cfgPath, cfg, store, &autoOut, t0)
	cmdAuto := aAuto.agentStartCmd()
	cmdAuto.SilenceErrors = true
	cmdAuto.SilenceUsage = true
	cmdAuto.SetArgs([]string{"OmpByAuto", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto})
	if err := cmdAuto.Execute(); err != nil {
		t.Fatalf("agents start --harness auto at subscription ceiling 0: %v\n%s", err, autoOut.String())
	}
	if !strings.Contains(autoOut.String(), "choice: omp / local-model/worker-model (provider local-model)") ||
		!strings.Contains(autoOut.String(), "started OmpByAuto: omp agent omp_") {
		t.Fatalf("--harness auto start output:\n%s", autoOut.String())
	}

	// 2. The self-hosted ceiling refuses a start past its own sessions.
	selfHostedAgents(t, store, "OmpBusyOne", "OmpBusyTwo")
	var capOut bytes.Buffer
	aFull := newSelfHostedApp(cfgPath, cfg, store, &capOut, t0)
	if err := aFull.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capOut.String(), "provider local-model: 2 busy of ceiling 2: OmpBusyOne, OmpBusyTwo; no start: 2 busy at the ceiling of 2") {
		t.Fatalf("capacity at the self-hosted ceiling:\n%s", capOut.String())
	}
	capOut.Reset()
	aFullJSON := newSelfHostedApp(cfgPath, cfg, store, &capOut, t0)
	aFullJSON.json = true
	if err := aFullJSON.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	var fullParsed capacityView
	if err := json.Unmarshal(capOut.Bytes(), &fullParsed); err != nil {
		t.Fatalf("capacity --json at the ceiling: %v\n%s", err, capOut.String())
	}
	if fullParsed.Room != 0 || fullParsed.Providers["local-model"].Room != 0 {
		t.Fatalf("room at the self-hosted ceiling = %d, provider room %d; want 0", fullParsed.Room, fullParsed.Providers["local-model"].Room)
	}
	var refusOut bytes.Buffer
	aRefuse := newSelfHostedApp(cfgPath, cfg, store, &refusOut, t0)
	cmdRefuse := aRefuse.agentStartCmd()
	cmdRefuse.SilenceErrors = true
	cmdRefuse.SilenceUsage = true
	cmdRefuse.SetArgs([]string{"OmpOverCeiling", brief, usageDirFlag, dir, testHarnessFlag, omp.Harness})
	err = cmdRefuse.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider local-model: 2 busy at the ceiling of 2") {
		t.Fatalf("omp start at the self-hosted ceiling: code=%d err=%v", Code(err), err)
	}
	// A claude start still refuses on the held subscription, not on the
	// self-hosted ceiling.
	var claudeOut bytes.Buffer
	aClaude := newSelfHostedApp(cfgPath, cfg, store, &claudeOut, t0)
	cmdClaude := aClaude.agentStartCmd()
	cmdClaude.SilenceErrors = true
	cmdClaude.SilenceUsage = true
	cmdClaude.SetArgs([]string{"ClaudeWhileHeld", brief, usageDirFlag, dir, testHarnessFlag, claudeComm})
	err = cmdClaude.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: ceiling 0") {
		t.Fatalf("claude start while the subscription is held: code=%d err=%v", Code(err), err)
	}

	// 3. Only the self-hosted provider is full while the subscription's
	// ceiling is back: the choice auto takes is then the subscription's.
	rawOpen := `capacity:
  floor: 0
  ceiling: 10
  providers:
    claude:
      floor: 1
      ceiling: 2
    local-model:
      kind: self-hosted
      ceiling: 2
  choices:
    - harness: claude
      model: claude-opus-4-7
    - harness: omp
      model: local-model/worker-model
      provider: local-model
      skipPace: true
omp:
  model: local-model/worker-model
`
	if err := os.WriteFile(cfgPath, []byte(rawOpen), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgOpen, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// The two busy omp agents stay on the roster: local-model is at its
	// ceiling of 2, claude has room.
	var openOut bytes.Buffer
	aOpen := newSelfHostedApp(cfgPath, cfgOpen, store, &openOut, t0)
	if err := aOpen.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(openOut.String(), "choice: claude / claude-opus-4-7 (provider claude)") {
		t.Fatalf("auto choice with the self-hosted provider full:\n%s", openOut.String())
	}
	if !strings.Contains(openOut.String(), "provider local-model: 2 busy of ceiling 2: OmpBusyOne, OmpBusyTwo; no start: 2 busy at the ceiling of 2") {
		t.Fatalf("self-hosted provider full, subscription open:\n%s", openOut.String())
	}
	if !strings.Contains(openOut.String(), "room for") {
		t.Fatalf("room with the subscription open:\n%s", openOut.String())
	}
}

// TestSelfHostedMachineGuardRefusesBoth covers the last Expected line: a
// machine guard refuses a start on the self-hosted provider and on the
// subscription alike.
func TestSelfHostedMachineGuardRefusesBoth(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte(selfHostedRawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The machine is short of MemAvailable: every start refuses, whichever
	// provider it would run on.
	orig := plat
	tight := platform.Stub()
	tight.Machine = tightMachine{Machine: tight.Machine}
	plat = tight
	t.Cleanup(func() { plat = orig })

	var out bytes.Buffer
	a := newSelfHostedApp(cfgPath, cfg, store, &out, t0)
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no start: MemAvailable 10.0 GiB under 20 GiB") {
		t.Fatalf("capacity on a tight machine:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "provider local-model: 0 busy of ceiling 2; no start: MemAvailable 10.0 GiB under 20 GiB") {
		t.Fatalf("self-hosted provider on a tight machine:\n%s", out.String())
	}

	var ompOut bytes.Buffer
	aOmp := newSelfHostedApp(cfgPath, cfg, store, &ompOut, t0)
	cmdOmp := aOmp.agentStartCmd()
	cmdOmp.SilenceErrors = true
	cmdOmp.SilenceUsage = true
	cmdOmp.SetArgs([]string{"OmpOnTightMachine", brief, usageDirFlag, dir, testHarnessFlag, omp.Harness})
	err = cmdOmp.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider local-model: MemAvailable 10.0 GiB under 20 GiB") {
		t.Fatalf("omp start on a tight machine: code=%d err=%v", Code(err), err)
	}

	var claudeOut bytes.Buffer
	aClaude := newSelfHostedApp(cfgPath, cfg, store, &claudeOut, t0)
	cmdClaude := aClaude.agentStartCmd()
	cmdClaude.SilenceErrors = true
	cmdClaude.SilenceUsage = true
	cmdClaude.SetArgs([]string{"ClaudeOnTightMachine", brief, usageDirFlag, dir, testHarnessFlag, claudeComm})
	err = cmdClaude.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: MemAvailable 10.0 GiB under 20 GiB") {
		t.Fatalf("claude start on a tight machine: code=%d err=%v", Code(err), err)
	}
}

// TestSelfHostedProviderServeMode covers the serving mode deriving a
// self-hosted provider's ceiling: the mode's ceiling replaces the configured
// one, an unknown mode falls back to it, and a provider without a mode reader
// keeps its ceiling as configured.
func TestSelfHostedProviderServeMode(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	rawCfg := `capacity:
  ceiling: 0
  providers:
    local-model:
      kind: self-hosted
      ceiling: 3
      serveModeCommand: [read-serve-mode]
      serveModeCeilings:
        full: 2
        tiny: 1
  choices:
    - {harness: omp, model: local-model/worker-model, provider: local-model, skipPace: true}
omp:
  model: local-model/worker-model
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	mode := serveModeFull
	var modeErr error
	reads := 0
	newApp := func(out *bytes.Buffer) *app {
		a := newSelfHostedApp(cfgPath, cfg, store, out, t0)
		a.readServeMode = func(_ context.Context, _ []string, _ time.Duration) (config.ServeModeReading, error) {
			reads++
			if modeErr != nil {
				return config.ServeModeReading{}, modeErr
			}
			return config.ServeModeReading{Mode: mode}, nil
		}
		return a
	}
	// clearServeMode drops the cached reading so the next call runs the reader
	// again; servingModeStaleAfter is the other way a stale mode re-reads.
	clearServeMode := func() {
		if err := os.Remove(filepath.Join(cfg.StateDir, serveModeFile("local-model"))); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}

	// The mode's ceiling bounds the provider: full runs 2 sessions at once.
	var out bytes.Buffer
	if err := newApp(&out).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider local-model: 0 busy of ceiling 2; serving mode "+serveModeFull+"; room for 2 starts") {
		t.Fatalf("capacity in mode full:\n%s", out.String())
	}

	// A cached reading serves the next call without running the reader again.
	out.Reset()
	if err := newApp(&out).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("serving mode read %d times, want the cached one reused", reads)
	}

	// An unknown mode falls back to the configured ceiling, naming the mode.
	mode = "turbo"
	clearServeMode()
	out.Reset()
	if err := newApp(&out).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider local-model: 0 busy of ceiling 3; serving mode turbo; room for 3 starts") {
		t.Fatalf("capacity in an unlisted mode:\n%s", out.String())
	}

	// A failed read keeps the configured ceiling and says the mode is unknown.
	modeErr = errors.New("mode reader down")
	clearServeMode()
	out.Reset()
	if err := newApp(&out).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider local-model: 0 busy of ceiling 3; serving mode unknown (mode reader down); room for 3 starts") {
		t.Fatalf("capacity on a failed mode read:\n%s", out.String())
	}
	modeErr = nil

	// The tiny mode's ceiling of 1 bounds the starts: the second one on the
	// one busy session refuses.
	mode = "tiny"
	clearServeMode()
	out.Reset()
	if err := newApp(&out).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider local-model: 0 busy of ceiling 1; serving mode tiny; room for 1 start") {
		t.Fatalf("capacity in mode tiny:\n%s", out.String())
	}
	selfHostedAgents(t, store, "OmpTinyBusy")
	out.Reset()
	if err := newApp(&out).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider local-model: 1 busy of ceiling 1: OmpTinyBusy; serving mode tiny; no start: 1 busy at the ceiling of 1") {
		t.Fatalf("capacity in mode tiny at its ceiling:\n%s", out.String())
	}
}

// TestSelfHostedParkedNotCounted covers the counting rule: busy agents on the
// self-hosted model count against its ceiling, parked and waiting ones do not.
func TestSelfHostedParkedNotCounted(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte(selfHostedRawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	// One busy, one kept (parked) and one a timer wakes, all on the
	// self-hosted model: only the busy one holds a slot.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		busy := tasked("OmpBusy")
		busy.Harness, busy.Model = omp.Harness, selfHostedTestModel
		kept := tasked("OmpKept")
		kept.Harness, kept.Model = omp.Harness, selfHostedTestModel
		kept.Keep = &state.Keep{Reason: "on a decision"}
		woken := tasked("OmpWoken")
		woken.Harness, woken.Model = omp.Harness, selfHostedTestModel
		st.Agents = append(st.Agents, busy, kept, woken)
		st.Timers = []state.Timer{{ID: 3, Due: t0.Add(time.Hour), What: "the batch ended", Wake: "OmpWoken"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := newSelfHostedApp(cfgPath, cfg, store, &out, t0)
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider local-model: 1 busy of ceiling 2: OmpBusy; room for 1 start") {
		t.Fatalf("capacity with parked and waiting omp agents:\n%s", out.String())
	}
}

// TestSelfHostedSubscriptionHarnessOrdering covers the resolution rule: a
// subscription harness never resolves to a self-hosted provider, whichever
// order capacity.providers lists them in, and an omp model resolves by its
// <provider>/ prefix even with no capacity.choices configured.
func TestSelfHostedSubscriptionHarnessOrdering(t *testing.T) {
	// claude sorts before local-model, but the self-hosted ceiling test's
	// order is what guards the alphabetical-first fallback: use a desk where
	// the self-hosted provider is the one a naive names[0] fallback picks.
	dir, cfgPath := isolateUsageTestEnv(t)
	const provLocal = "a-local"
	raw := `capacity:
  floor: 0
  ceiling: 0
  providers:
    ` + provLocal + `:
      kind: self-hosted
      ceiling: 2
    claude:
      floor: 1
      ceiling: 2
omp:
  model: ` + provLocal + `/worker-model
`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Capacity.ProviderNames(); len(got) == 0 || got[0] != provLocal {
		t.Fatalf("provider names %v, want the self-hosted one first", got)
	}
	k := cfg.Capacity
	// A claude harness keeps its own provider; agy and codex, unconfigured,
	// fall back to the claude subscription provider — never to the
	// self-hosted provider listed first.
	if p, _ := resolveAgentProviderBucket(k, claudeComm, "claude-opus-4-7"); p != claudeComm {
		t.Fatalf("claude harness resolved to %q, want claude", p)
	}
	if p, _ := resolveAgentProviderBucket(k, harnessAgy, "gemini-3-1-pro-medium"); p != claudeComm {
		t.Fatalf("agy harness resolved to %q, want claude", p)
	}
	if p, _ := resolveAgentProviderBucket(k, codex.Harness, "gpt-5.4"); p != claudeComm {
		t.Fatalf("codex harness resolved to %q, want claude", p)
	}
	// An omp model resolves by its <provider>/ prefix, without choices.
	if p, _ := resolveAgentProviderBucket(k, omp.Harness, provLocal+"/worker-model"); p != provLocal {
		t.Fatalf("omp model resolved to %q, want %s", p, provLocal)
	}
	// With only a self-hosted provider in the pool, even the omp harness's
	// fallback leaves the provider unresolved rather than borrowing it.
	ompOnly := config.Capacity{Providers: map[string]config.Provider{provLocal: {Kind: config.KindSelfHosted, Ceiling: 2}}}
	if p, _ := resolveAgentProviderBucket(ompOnly, harnessAgy, "gemini-3-1-pro-medium"); p != harnessAgy {
		t.Fatalf("agy harness with only a self-hosted provider resolved to %q, want none", p)
	}
	// A claude, agy or codex start refuses on the held subscription at
	// ceiling 0 — never on the self-hosted room the fallback would lend.
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		harness string
		model   string
	}{
		{"ClaudeAtCeilingZero", "", ""},
		{"AgyAtCeilingZero", harnessAgy, "gemini-3-1-pro-medium"},
		{"CodexAtCeilingZero", codex.Harness, "gpt-5.4"},
	} {
		var out bytes.Buffer
		a := newSelfHostedApp(cfgPath, cfg, store, &out, t0)
		sc := a.agentStartCmd()
		sc.SilenceErrors = true
		sc.SilenceUsage = true
		args := []string{tc.name, brief, usageDirFlag, dir}
		if tc.harness != "" {
			args = append(args, testHarnessFlag, tc.harness)
		}
		if tc.model != "" {
			args = append(args, "--model", tc.model)
		}
		sc.SetArgs(args)
		err := sc.Execute()
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: ceiling 0") {
			t.Fatalf("%s start at subscription ceiling 0: code=%d err=%v", tc.name, Code(err), err)
		}
	}
	// An omp start goes through on the self-hosted provider, no choices and
	// capacity.ceiling 0 aside.
	fakeOmp := filepath.Join(dir, omp.Comm)
	if err := os.WriteFile(fakeOmp, []byte("#!/bin/sh\necho '{\"models\":[{\"selector\":\""+provLocal+"/worker-model\"}]}'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	origLauncher := plat.Launcher
	origProcMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origProcMachine
	})
	plat.Machine = tableMachine{plat.Machine}
	plat.Launcher = &agyTestLauncher{onStart: func(u platform.Unit) {
		go func() {
			var ompID string
			for _, e := range u.Env {
				if v, ok := strings.CutPrefix(e, omp.EnvAgent+"="); ok {
					ompID = v
				}
			}
			if ompID == "" {
				return
			}
			f, oerr := os.OpenFile(omp.InboxPath(cfg.StateDir, ompID), os.O_RDWR, 0) //nolint:gosec // test FIFO
			if oerr != nil {
				return
			}
			defer func() { _ = f.Close() }()
			_, _ = bufio.NewReader(f).ReadString('\n')
		}()
	}}
	var startOut bytes.Buffer
	aStart := newSelfHostedApp(cfgPath, cfg, store, &startOut, t0)
	cmdStart := aStart.agentStartCmd()
	cmdStart.SilenceErrors = true
	cmdStart.SilenceUsage = true
	cmdStart.SetArgs([]string{"OmpNoChoices", brief, usageDirFlag, dir, testHarnessFlag, omp.Harness})
	if err := cmdStart.Execute(); err != nil {
		t.Fatalf("agents start --harness omp without choices: %v\n%s", err, startOut.String())
	}
	if !strings.Contains(startOut.String(), "started OmpNoChoices: omp agent omp_") {
		t.Fatalf("omp start output:\n%s", startOut.String())
	}
}

// TestSelfHostedWatchSilentAtCeilingZero covers the watch: with
// capacity.ceiling configured 0, busy agents reaching the self-hosted
// provider's own ceiling raise neither CAPACITY LOW nor CAPACITY FULL — the
// derived ceiling carries the self-hosted room, the configured hold stands.
func TestSelfHostedWatchSilentAtCeilingZero(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte(selfHostedRawCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	selfHostedAgents(t, store, "OmpBusyOne", "OmpBusyTwo")
	var watchOut bytes.Buffer
	aWatch := &app{cfgPath: cfgPath, cfg: cfg, store: store, now: t0, out: &watchOut}
	w := aWatch.newWatcher(false, true)
	w.now = t0
	w.readHeadroom = func(context.Context, *swapReading) *headroom { return roomy() }
	// The derived ceiling is the self-hosted one (2); busy is 2, so a naive
	// check against the derived total would raise CAPACITY FULL.
	if k, u := w.currentUsage(w.liveCapacity(), t0); k.Ceiling == 0 {
		t.Fatalf("derived ceiling = %d, want the self-hosted room above the held total (u %+v)", k.Ceiling, u)
	}
	w.capacity(context.Background(), supervised(2), nil)
	if s := watchOut.String(); strings.Contains(s, "CAPACITY FULL") || strings.Contains(s, "CAPACITY LOW") {
		t.Fatalf("watch at configured ceiling 0 with busy at the self-hosted ceiling:\n%s", s)
	}
}

// TestSelfHostedUsageUnknownKeepsProviderBlock covers the no-choices verdict:
// with a positive capacity.ceiling and a subscription provider whose usage
// read failed (its own ceiling 0), the verdict names that provider's block,
// not a bare "ceiling 0", and the self-hosted provider's room stands.
func TestSelfHostedUsageUnknownKeepsProviderBlock(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	providerRaw := `capacity:
  floor: 0
  ceiling: 10
  providers:
    claude:
      ceiling: 4
      command: [read-claude]
    local-model:
      kind: self-hosted
      ceiling: 2
omp:
  model: local-model/worker-model
`
	if err := os.WriteFile(cfgPath, []byte(providerRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	a := newSelfHostedApp(cfgPath, cfg, store, &out, t0)
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) {
		return usageReading{}, errors.New("endpoint timeout")
	}
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "no start: ceiling 0") {
		t.Fatalf("provider-pool verdict hides the provider's own block:\n%s", got)
	}
	if !strings.Contains(got, "provider claude: 0 busy of floor 0, ceiling 0; usage unknown (endpoint timeout)") {
		t.Fatalf("claude line on the failed read:\n%s", got)
	}
	if !strings.Contains(got, "provider local-model: 0 busy of ceiling 2; room for 2 starts") {
		t.Fatalf("self-hosted line stays open:\n%s", got)
	}
	if !strings.Contains(got, "room for 2 starts") {
		t.Fatalf("verdict with the self-hosted provider open:\n%s", got)
	}

	// With the self-hosted provider full, its verdict names its own block
	// and the subscription's, never a bare ceiling 0.
	selfHostedAgents(t, store, "OmpBusyOne", "OmpBusyTwo")
	out.Reset()
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if strings.Contains(got, "no start: ceiling 0") {
		t.Fatalf("verdict with the self-hosted provider full hides its block:\n%s", got)
	}
	if !strings.Contains(got, "claude: subscription usage unknown (endpoint timeout)") || !strings.Contains(got, "local-model: 2 busy at the ceiling of 2") {
		t.Fatalf("verdict with the self-hosted provider full:\n%s", got)
	}

	// At capacity.ceiling 0 the same holds: the subscription's ceiling 0
	// rides its own provider's name, the self-hosted block stays apart.
	selfHostedAgents(t, store)
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(providerRaw, "ceiling: 10", "ceiling: 0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	selfHostedAgents(t, store, "OmpBusyOne", "OmpBusyTwo")
	out.Reset()
	a = newSelfHostedApp(cfgPath, cfg, store, &out, t0)
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) {
		return usageReading{}, errors.New("endpoint timeout")
	}
	if err := a.capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if strings.Contains(got, "no start: ceiling 0") {
		t.Fatalf("verdict at ceiling 0 with the self-hosted provider full hides its block:\n%s", got)
	}
	// The failed read stands at claude's own ceiling 0: the verdict names
	// each provider's block, the total's ceiling 0 never overrides it.
	if !strings.Contains(got, "claude: subscription usage unknown (endpoint timeout)") || !strings.Contains(got, "local-model: 2 busy at the ceiling of 2") {
		t.Fatalf("verdict at ceiling 0 with the self-hosted provider full:\n%s", got)
	}
}

// TestSelfHostedNoChoicesFallback covers an omp start with no
// capacity.choices: the model's <provider>/ prefix wins when it names a
// provider, and with a prefix that names none (models.yml's provider need
// not carry the capacity.providers key), the sole self-hosted provider
// takes the agent.
func TestSelfHostedNoChoicesFallback(t *testing.T) {
	k := config.Capacity{Providers: map[string]config.Provider{selfHostedProv: {Kind: config.KindSelfHosted, Ceiling: 2}}}
	if p, _ := resolveAgentProviderBucket(k, omp.Harness, "local/worker-model"); p != selfHostedProv {
		t.Fatalf("omp model with an unknown prefix resolved to %q, want the sole self-hosted provider", p)
	}
	if p, _ := resolveAgentProviderBucket(k, omp.Harness, selfHostedTestModel); p != selfHostedProv {
		t.Fatalf("omp model with its own prefix resolved to %q, want %s", p, selfHostedProv)
	}
	// Two self-hosted providers and no prefix match: unresolved, not guessed.
	two := config.Capacity{Providers: map[string]config.Provider{
		"a-local": {Kind: config.KindSelfHosted, Ceiling: 2},
		"b-local": {Kind: config.KindSelfHosted, Ceiling: 2},
	}}
	if p, _ := resolveAgentProviderBucket(two, omp.Harness, "other/worker-model"); p != omp.Harness {
		t.Fatalf("ambiguous omp model resolved to %q, want no provider", p)
	}
	if p, _ := resolveAgentProviderBucket(two, omp.Harness, "b-local/x"); p != "b-local" {
		t.Fatalf("omp model by prefix resolved to %q, want b-local", p)
	}
	// A self-hosted provider named omp never shadows the model selector: the
	// <provider>/ prefix of the model resolves first, and a model naming no
	// provider falls to the sole self-hosted provider other than omp.
	named := config.Capacity{Providers: map[string]config.Provider{
		omp.Harness:    {Kind: config.KindSelfHosted, Ceiling: 2},
		selfHostedProv: {Kind: config.KindSelfHosted, Ceiling: 2},
	}}
	if p, _ := resolveAgentProviderBucket(named, omp.Harness, selfHostedTestModel); p != selfHostedProv {
		t.Fatalf("omp model shadowed by a provider named omp: resolved to %q, want %s", p, selfHostedProv)
	}
	if p, _ := resolveAgentProviderBucket(named, omp.Harness, omp.Harness+"/worker-model"); p != omp.Harness {
		t.Fatalf("omp model naming the omp provider resolved to %q, want %s", p, omp.Harness)
	}
	// A subscription harness never resolves to a self-hosted provider named
	// after it: with no subscription provider in the pool the harness's own
	// name stands, and the self-hosted provider keeps its room to omp.
	subOnly := config.Capacity{Providers: map[string]config.Provider{
		claudeComm:     {Ceiling: 4, Usage: config.Usage{Command: []string{"read-claude"}}},
		selfHostedProv: {Kind: config.KindSelfHosted, Ceiling: 2},
	}}
	if p, _ := resolveAgentProviderBucket(subOnly, claudeComm, "opus"); p != claudeComm {
		t.Fatalf("claude harness resolved to %q, want claude", p)
	}
	sh := config.Capacity{Providers: map[string]config.Provider{
		claudeComm: {Kind: config.KindSelfHosted, Ceiling: 2},
	}}
	if p, _ := resolveAgentProviderBucket(sh, claudeComm, "opus"); p != claudeComm {
		t.Fatalf("claude harness against a self-hosted provider named claude resolved to %q, want claude (no room, held)", p)
	}
}

// loadOnlySelfHosted loads the only-self-hosted desk at the given
// capacity.ceiling and opens its state: a desk whose capacity.providers
// holds only a self-hosted provider, so capacity.ceiling bounds the starts
// of the harnesses that have no provider entry, and the self-hosted ceiling
// bounds the omp starts alone. With choices, capacity.choices holds the one
// omp choice; without, the desk configures no choices at all.
func loadOnlySelfHosted(t *testing.T, cfgPath string, ceiling int, choices bool) (*config.Config, *state.FileStore) {
	t.Helper()
	const onlySelfHostedRaw = `capacity:
  floor: 0
  ceiling: CEILING
  providers:
    local-model:
      kind: self-hosted
      ceiling: 3
`
	raw := onlySelfHostedRaw
	if choices {
		raw += `  choices:
    - {harness: omp, model: local-model/worker-model, provider: local-model, skipPace: true}
`
	}
	raw += `omp:
  model: local-model/worker-model
`
	raw = strings.Replace(raw, "CEILING", strconv.Itoa(ceiling), 1)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	return cfg, store
}

// startHarness runs one agents start of the harness ("" the default claude)
// and returns its error; dryRun adds --dry-run.
func startHarness(a *app, name, brief, dir, harness, model string, dryRun bool) error {
	sc := a.agentStartCmd()
	sc.SilenceErrors = true
	sc.SilenceUsage = true
	args := []string{name, brief, usageDirFlag, dir}
	if dryRun {
		args = append(args, testDryRunFlag)
	}
	if harness != "" {
		args = append(args, testHarnessFlag, harness)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	sc.SetArgs(args)
	return sc.Execute()
}

// TestSelfHostedOnlyCeilingBoundsOthers covers the desk with only a
// self-hosted provider: capacity.ceiling 0 refuses every claude, agy and
// codex start with its own hold, never the self-hosted room, while the omp
// choice keeps its room up to the provider's ceiling; at ceiling 5 the
// unconfigured harnesses keep a room of 5 minus their own busy sessions, and
// a full self-hosted provider blocks none of them.
func TestSelfHostedOnlyCeilingBoundsOthers(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	cfg, store := loadOnlySelfHosted(t, cfgPath, 0, false)
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// capacity.ceiling 0, nothing busy: the verdict is the self-hosted room,
	// yet a claude, agy or codex start refuses on ceiling 0 — not with the
	// self-hosted room lent by the empty-provider fallback, and not refused
	// only once the self-hosted provider fills.
	var out bytes.Buffer
	if err := newSelfHostedApp(cfgPath, cfg, store, &out, t0).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "room for 3 starts") {
		t.Fatalf("capacity at ceiling 0 with only a self-hosted provider:\n%s", out.String())
	}
	for _, tc := range []struct{ name, harness, model string }{
		{"ClaudeCeilingZero", "", "claude-opus-4-7"},
		{"AgyCeilingZero", harnessAgy, "gemini-3-1-pro-medium"},
		{"CodexCeilingZero", codex.Harness, "gpt-5.4"},
	} {
		var startOut bytes.Buffer
		a := newSelfHostedApp(cfgPath, cfg, store, &startOut, t0)
		err := startHarness(a, tc.name, brief, dir, tc.harness, tc.model, false)
		if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: ceiling 0") {
			t.Fatalf("%s start at ceiling 0: code=%d err=%v", tc.name, Code(err), err)
		}
	}
	// omp keeps its own room up to the provider ceiling of 3.
	fakeOmp := filepath.Join(dir, omp.Comm)
	if err := os.WriteFile(fakeOmp, []byte("#!/bin/sh\necho '{\"models\":[{\"selector\":\""+selfHostedTestModel+"\"}]}'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	origLauncher := plat.Launcher
	origProcMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origProcMachine
	})
	plat.Machine = tableMachine{plat.Machine}
	plat.Launcher = &agyTestLauncher{onStart: func(u platform.Unit) {
		go func() {
			var ompID string
			for _, e := range u.Env {
				if v, ok := strings.CutPrefix(e, omp.EnvAgent+"="); ok {
					ompID = v
				}
			}
			if ompID == "" {
				return
			}
			f, oerr := os.OpenFile(omp.InboxPath(cfg.StateDir, ompID), os.O_RDWR, 0) //nolint:gosec // test FIFO
			if oerr != nil {
				return
			}
			defer func() { _ = f.Close() }()
			_, _ = bufio.NewReader(f).ReadString('\n')
		}()
	}}
	ompApp := func(out *bytes.Buffer) *app {
		return newSelfHostedApp(cfgPath, cfg, store, out, t0)
	}
	out.Reset()
	if err := startHarness(ompApp(&out), "OmpRoomOne", brief, dir, omp.Harness, "", false); err != nil {
		t.Fatalf("omp start at ceiling 0: %v\n%s", err, out.String())
	}
	// Its busy sessions never shrink the omp room below the provider ceiling.
	out.Reset()
	if err := startHarness(ompApp(&out), "OmpRoomTwo", brief, dir, omp.Harness, "", false); err != nil {
		t.Fatalf("second omp start at ceiling 0: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := startHarness(ompApp(&out), "OmpRoomThree", brief, dir, omp.Harness, "", false); err != nil {
		t.Fatalf("third omp start at ceiling 0: %v\n%s", err, out.String())
	}
	// The provider is full at 3; omp refuses on its own ceiling.
	out.Reset()
	err := startHarness(ompApp(&out), "OmpOverProvider", brief, dir, omp.Harness, "", false)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider local-model: 3 busy at the ceiling of 3") {
		t.Fatalf("fourth omp start: code=%d err=%v", Code(err), err)
	}
	// The verdict a claude start reads then still holds ceiling 0, and a
	// claude start on it refuses with the hold's own words.
	var capOut bytes.Buffer
	if err := newSelfHostedApp(cfgPath, cfg, store, &capOut, t0).capacityCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	var claudeOut bytes.Buffer
	err = startHarness(newSelfHostedApp(cfgPath, cfg, store, &claudeOut, t0), "ClaudeProviderFull", brief, dir, claudeComm, "claude-opus-4-7", false)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: ceiling 0") ||
		strings.Contains(err.Error(), "no choice has room") {
		t.Fatalf("claude start with the self-hosted provider full: code=%d err=%v\n%s", Code(err), err, capOut.String())
	}

	// At ceiling 5 with the self-hosted provider at its own full 3 and no
	// claude session busy, a claude start still has the whole room of 5:
	// the self-hosted provider's busy sessions weigh on its own ceiling,
	// never on capacity.ceiling or on a harness none of its providers claims.
	cfg5, store5 := loadOnlySelfHosted(t, cfgPath, 5, false)
	if err := store5.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		for _, n := range []string{"OmpBusyOne", "OmpBusyTwo", "OmpBusyThree"} {
			w := tasked(n)
			w.Harness, w.Model = omp.Harness, selfHostedTestModel
			st.Agents = append(st.Agents, w)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var claudeRoom bytes.Buffer
	aClaude := newSelfHostedApp(cfgPath, cfg5, store5, &claudeRoom, t0)
	aClaude.json = true
	_, v, err := aClaude.startGateWithOptions(context.Background(), "ClaudeAtFive", claudeComm, "claude-opus-4-7", false, true, nil, forWorker)
	if err != nil {
		t.Fatalf("claude gate at ceiling 5 with the self-hosted provider full: %v", err)
	}
	if v.Room != 5 {
		t.Fatalf("claude target room = %d, want the whole ceiling of 5 with its own sessions free", v.Room)
	}
	// One claude session busy then takes one of that room: 4 left.
	if err := store5.Update(func(st *state.State) ([]state.Event, error) {
		busy := tasked("ClaudeBusy")
		st.Agents = append(st.Agents, busy)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	aClaude2 := newSelfHostedApp(cfgPath, cfg5, store5, &claudeRoom, t0)
	aClaude2.json = true
	_, v2, err := aClaude2.startGateWithOptions(context.Background(), "ClaudeAtFiveBusy", claudeComm, "claude-opus-4-7", false, true, nil, forWorker)
	if err != nil {
		t.Fatalf("claude gate at ceiling 5 with its own session busy: %v", err)
	}
	if v2.Room != 4 {
		t.Fatalf("claude target room with one claude busy = %d, want 5 minus its own busy session", v2.Room)
	}
}

// TestSelfHostedOnlyDryRunPrintsVerdict covers the dry run of a desk with
// only a self-hosted provider: with an explicit --harness it runs the start
// gate and prints its verdict — the refused harnesses exit 3 on the very
// words the real start refuses with, the omp choice prints its room — and
// starts nothing.
func TestSelfHostedOnlyDryRunPrintsVerdict(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	cfg, store := loadOnlySelfHosted(t, cfgPath, 0, true)
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	brief := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An agy dry run with a model refuses on the gate: the held ceiling, the
	// same words the real start refuses with.
	var agyOut bytes.Buffer
	aAgy := newSelfHostedApp(cfgPath, cfg, store, &agyOut, t0)
	err := startHarness(aAgy, "DryAgyHeld", brief, dir, harnessAgy, "gemini-3-1-pro-medium", true)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: ceiling 0") {
		t.Fatalf("--dry-run agy at ceiling 0: code=%d err=%v", Code(err), err)
	}
	// A claude dry run refuses alike.
	var claudeOut bytes.Buffer
	err = startHarness(newSelfHostedApp(cfgPath, cfg, store, &claudeOut, t0), "DryClaudeHeld", brief, dir, claudeComm, "claude-opus-4-7", true)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: ceiling 0") {
		t.Fatalf("--dry-run claude at ceiling 0: code=%d err=%v", Code(err), err)
	}
	// The dry run of the harness with no room starts nothing, while the omp
	// dry run with room prints its turn.
	fakeOmp := filepath.Join(dir, omp.Comm)
	if err := os.WriteFile(fakeOmp, []byte("#!/bin/sh\necho '{\"models\":[{\"selector\":\""+selfHostedTestModel+"\"}]}'\n"), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	origLauncher := plat.Launcher
	origProcMachine := plat.Machine
	t.Cleanup(func() {
		plat.Launcher = origLauncher
		plat.Machine = origProcMachine
	})
	// The gate reads the machine's guards for real: roomyMachine's spare
	// memory and load keep the verdict on the desk's numbers, not the host's.
	plat.Machine = roomyMachine{plat.Machine}
	plat.Launcher = &agyTestLauncher{onStart: func(u platform.Unit) {
		go func() {
			var ompID string
			for _, e := range u.Env {
				if v, ok := strings.CutPrefix(e, omp.EnvAgent+"="); ok {
					ompID = v
				}
			}
			if ompID == "" {
				return
			}
			f, oerr := os.OpenFile(omp.InboxPath(cfg.StateDir, ompID), os.O_RDWR, 0) //nolint:gosec // test FIFO
			if oerr != nil {
				return
			}
			defer func() { _ = f.Close() }()
			_, _ = bufio.NewReader(f).ReadString('\n')
		}()
	}}
	var ompOut bytes.Buffer
	aOmp := newSelfHostedApp(cfgPath, cfg, store, &ompOut, t0)
	if err := startHarness(aOmp, "DryOmpRoom", brief, dir, omp.Harness, "", true); err != nil {
		t.Fatalf("--dry-run omp at ceiling 0: %v\n%s", err, ompOut.String())
	}
	if !strings.Contains(ompOut.String(), "room for 3 starts") ||
		!strings.Contains(ompOut.String(), "dry run: DryOmpRoom") ||
		!strings.Contains(ompOut.String(), "as an omp agent on "+selfHostedTestModel) {
		t.Fatalf("--dry-run omp output:\n%s", ompOut.String())
	}
	// The same with no --harness: the auto choice prints its line, and the
	// gate's verdict with it.
	var autoOut bytes.Buffer
	aAuto := newSelfHostedApp(cfgPath, cfg, store, &autoOut, t0)
	if err := startHarness(aAuto, "DryOmpAuto", brief, dir, harnessAuto, "", true); err != nil {
		t.Fatalf("--dry-run auto at ceiling 0: %v\n%s", err, autoOut.String())
	}
	if !strings.Contains(autoOut.String(), "room for 3 starts") {
		t.Fatalf("--dry-run auto output:\n%s", autoOut.String())
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 0 || len(st.Starts) != 0 {
		t.Fatalf("dry run started something: %d agents, %d starts", len(st.Agents), len(st.Starts))
	}
}

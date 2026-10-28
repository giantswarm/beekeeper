package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
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
	if err := os.WriteFile(agyFile, []byte(tsv), 0o600); err != nil {
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
    - {harness: agy, model: gemini-3-1-pro-medium, skipPace: true}
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
	v, err := a.startCapacityWithOptions(context.Background(), false, nil, false)
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
	v2, err := a.startCapacityWithOptions(context.Background(), false, nil, false)
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
	v3, err := a.startCapacityWithOptions(context.Background(), false, nil, false)
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
)

// TestProvidersChoicesPaceMarginExcludeAndMachineGuards exercises per-provider
// capacity verdicts, choice order, paceMargin, worker sevenDayMax vs --review,
// --exclude, busy agent attribution per provider/bucket, machine guards capping
// the total, watch once-per-tick USAGE output, and agents start --harness auto.
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

	claude5h, claude7d := 20.0, 54.0 // 54% <= 50% + 5% margin => OK
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

	// 1. claude 7d 54% is within pace 50% + 5% margin: first choice claude/claude-opus-4-7 wins.
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

	// 3. claude 7d moves to 56% (> 50% + 5% margin) -> ahead of pace, so agy/claude-opus-4-6-thinking wins.
	claude7d = 56.0
	a2, _ := newApp(t0, false)
	v2, err := a2.startCapacityWithOptions(context.Background(), false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Choice == nil || v2.Choice.Harness != harnessAgy || v2.Choice.Model != testModelOpus46 {
		t.Fatalf("step 3 choice = %+v, want agy/claude-opus-4-6-thinking", v2.Choice)
	}

	// 4. agy Claude bucket 7d moves to 52% (> 50% + 0% margin) -> falls through to
	//    agy/gemini-3-1-pro-medium (skipPace: true, 72% < 80% worker cap).
	agyClaude7d = 52.0
	a3, _ := newApp(t0, false)
	v3, err := a3.startCapacityWithOptions(context.Background(), false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Choice == nil || v3.Choice.Harness != harnessAgy || v3.Choice.Model != testModelGemini ||
		v3.Choice.Bucket != config.DefaultAgyGeminiBucket {
		t.Fatalf("step 4 choice = %+v, want agy/gemini-3-1-pro-medium", v3.Choice)
	}

	// 5. agy Gemini bucket 7d reaches 82% (>= 80% worker cap, < 95% provider stop):
	//    - worker start gets no choice and room 0;
	//    - --review skips pace and worker cap, picking claude/claude-opus-4-7 (or agy/gemini-3-1-pro-medium when claude + opus-4-6 are excluded).
	agyGemini7d = 82.0
	a4, _ := newApp(t0, false)
	vWorker, err := a4.startCapacityWithOptions(context.Background(), false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if vWorker.Room != 0 || vWorker.Choice == nil || vWorker.Choice.Harness != "" {
		t.Fatalf("worker at 82%% gemini = room %d, choice %+v; want room 0 and no choice", vWorker.Room, vWorker.Choice)
	}
	a4Rev, _ := newApp(t0, false)
	vReview, err := a4Rev.startCapacityWithOptions(context.Background(), false, []string{claudeComm, testModelOpus46}, true)
	if err != nil {
		t.Fatal(err)
	}
	if vReview.Room == 0 || vReview.Choice == nil || vReview.Choice.Harness != harnessAgy || vReview.Choice.Model != testModelGemini || !vReview.Choice.Review {
		t.Fatalf("review at 82%% gemini = room %d, choice %+v; want agy/gemini-3-1-pro-medium", vReview.Room, vReview.Choice)
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
	vCap, err := a5.startCapacityWithOptions(context.Background(), false, nil, false)
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
	vOpen, err := a6.startCapacityWithOptions(context.Background(), false, nil, false)
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
	vTight, err := aTight.startCapacityWithOptions(context.Background(), false, nil, false)
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
	//      skips the pace check and worker weekly cap and starts the session through the agy harness on gemini-3-1-pro-medium.
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
	aRevStart, outRevStart := newApp(t0, false)
	revCmd := aRevStart.agentStartCmd()
	revCmd.SilenceErrors = true
	revCmd.SilenceUsage = true
	revCmd.SetArgs([]string{"ReviewAgy", brief, usageDirFlag, dir, testHarnessFlag, harnessAuto, "--review", testExcludeFlag, claudeComm, testExcludeFlag, testModelOpus46})
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
//   - an explicit --harness start (including default --harness claude) is gated
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

	// 1a. claude reader fails while agy has Room > 0: default `agents start` (--harness claude) refuses with exit 3.
	claudeErr = fmt.Errorf("claude endpoint down")
	aFail, _ := newApp()
	cmdFail := aFail.agentStartCmd()
	cmdFail.SilenceErrors = true
	cmdFail.SilenceUsage = true
	cmdFail.SetArgs([]string{"ClaudeOnFail", brief, usageDirFlag, dir, modeFlag, config.ModeBackground})
	err = cmdFail.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: subscription usage unknown (claude endpoint down)") {
		t.Fatalf("explicit claude start on failed read: code=%d, err=%v", Code(err), err)
	}

	// 1b. claude in USAGE HIGH (96%) while agy has Room > 0: default `agents start` refuses with exit 3.
	claudeErr = nil
	claude5h = 96.0
	aHigh, _ := newApp()
	cmdHigh := aHigh.agentStartCmd()
	cmdHigh.SilenceErrors = true
	cmdHigh.SilenceUsage = true
	cmdHigh.SetArgs([]string{"ClaudeOnHigh", brief, usageDirFlag, dir, modeFlag, config.ModeBackground})
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
	cmdCeil.SetArgs([]string{"ClaudeAtCeil", brief, usageDirFlag, dir, modeFlag, config.ModeBackground})
	err = cmdCeil.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "no start: provider claude: 1 busy at the ceiling of 1") {
		t.Fatalf("explicit claude start at ceiling: code=%d, err=%v", Code(err), err)
	}

	stubBackground(t, "backgrounded · "+bgShortID+" · ClaudeForced\n", bgSession{ID: bgShortID, SessionID: bgID, Name: "ClaudeForced"})
	aForce, outForce := newApp()
	cmdForce := aForce.agentStartCmd()
	cmdForce.SilenceErrors = true
	cmdForce.SilenceUsage = true
	cmdForce.SetArgs([]string{"ClaudeForced", brief, usageDirFlag, dir, modeFlag, config.ModeBackground, usageForceFlag})
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
	_, _, err = aBucket.startGateWithOptions(context.Background(), "AgyClaudeTry", harnessAgy, testModelOpus46, false, false, nil, false)
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
	cmdDryAgy.SetArgs([]string{"DryAgy", brief, usageDirFlag, dir, "--dry-run", testHarnessFlag, harnessAuto})
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
	cmdDryOmp.SetArgs([]string{"DryOmp", brief, usageDirFlag, dir, "--dry-run", testHarnessFlag, harnessAuto, testExcludeFlag, harnessAgy})
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

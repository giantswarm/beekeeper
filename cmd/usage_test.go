package cmd

import (
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

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	usageDirFlag   = "--dir"
	usageForceFlag = "--force"
	usageVerbStart = "agents.start"
	usageCaller    = "usage-sup"
)

func isolateUsageTestEnv(t *testing.T) (dir, cfgPath string) {
	t.Helper()
	orig := plat
	plat.Machine = roomyMachine{Machine: plat.Machine}
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

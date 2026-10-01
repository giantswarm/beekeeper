package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Fixture names shared by the model and view tests.
const (
	tBee        = "bee"
	tWasp       = "wasp"
	tSup        = "sup"
	tGuide      = "guide"
	tSupervisor = "supervisor"
	tOrg        = "giantswarm"
	tClaim      = "claim"
	tGrant      = "grant"
	tHoldSet    = "hold-set"
	tRepo       = "repo/beekeeper"
	tIssue      = "giantswarm/beekeeper#1"
)

// testAt is the clock every fixture shares, so ages and clocks in the
// renders are fixed.
var testAt = time.Date(2026, 9, 26, 15, 4, 5, 0, time.Local)

// fakeSource answers with fixed data and tails, failing when told to.
type fakeSource struct {
	data    *Data
	err     error
	turns   []Turn
	tailErr error

	dataCalls int
	tailCall  string
	tailN     int
}

// Data records the call and answers with what the test set up.
func (f *fakeSource) Data(context.Context) (*Data, error) {
	f.dataCalls++
	return f.data, f.err
}

// Tail records which session was asked for and answers likewise.
func (f *fakeSource) Tail(_ context.Context, session string, turns int) ([]Turn, error) {
	f.tailCall, f.tailN = session, turns
	return f.turns, f.tailErr
}

// fixtureData is one refresh with one of everything the tabs render.
func fixtureData() *Data {
	cost := 1.23
	return &Data{
		At: testAt,
		Status: Status{
			Supervisor: tSup, SupervisorLive: true,
			LeaseCount: 1, HoldCount: 1, Due: 2,
		},
		Machine: Machine{
			Load: [3]float64{1.5, 1, 0.5}, PSIFull60: 2.5,
			Mem:      Mem{TotalMiB: 32768, AvailableMiB: 12288, SwapTotalMiB: 8192, SwapUsedMiB: 512, ShmemMiB: 256},
			Scope:    &Scope{CurrentMiB: 4096, High: "12G", Max: "16G", OOMKills: 1},
			Tmp:      Disk{Path: "/tmp", UsedMiB: 100, FreeMiB: 19000},
			Root:     Disk{Path: "/", UsedMiB: 9000, FreeMiB: 20000},
			Slots:    []Slot{{N: 1, Holder: tBee}, {N: 2, Free: true}},
			Clusters: []Cluster{{Name: "giantswarm-yolo", Nodes: 3, MemMiB: 2048, RunningFor: "2h"}},
			Waits:    []Wait{{PID: 42, Session: tBee, Args: "go test ./...", Elapsed: 13 * time.Second}},
			OOM:      []OOMKill{{At: testAt.Add(-1 * time.Hour), PID: 7, Task: "chrome", AnonMiB: 8000, Constraint: "memcg limit", Memcg: "user.slice", Owner: "scope"}},
			Oomd:     []string{"oomd: dry run"},
		},
		Budget: Budget{
			At: testAt, Limit: 5000, Remaining: 4000, Reset: testAt.Add(time.Hour), Floor: 500,
			Held: true, HoldReason: "release window",
			Pollers: []Poller{{PID: 9, Session: tBee, Args: "gh pr list", Elapsed: 4 * time.Second}},
		},
		Sessions: []Session{
			{
				PID: 100, Name: tBee, Role: "worker", Cwd: "/home/me/bee", Repo: "giantswarm/beekeeper",
				Branch: "main", Model: "opus", Permission: "acceptEdits", Started: testAt.Add(-3 * time.Hour),
				LastActive: testAt.Add(-30 * time.Second), MemMiB: 1500, Waiting: "answer to the question",
				Work: []string{tIssue}, Serves: "#1", Idle: 4 * time.Second,
				Leases: []string{tRepo}, Context: 90000, ContextWindow: 200000, ContextFill: 0.45,
				LastHour: Counter{Turns: 10, ToolErrors: 1, Cost: &cost},
				Total:    Counter{Turns: 99, ToolCalls: 50, ToolErrors: 3, Cost: &cost},
				Commands: []Command{{PID: 55, Args: "sleep 60", Elapsed: 2 * time.Second, Remaining: 58 * time.Second}},
				Scopes:   []RunScope{{Unit: "beekeeper-run-1.scope", Command: "go build", MemMiB: 500}},
				Merges:   Merges{Queued: 1, Failed: 1}, GitHubProcesses: 2,
			},
			{
				PID: 101, Name: tWasp, Role: "spare", Idle: 90 * time.Minute, MemMiB: 300,
				LastHour: Counter{Turns: 0, ToolErrors: 0},
			},
		},
		Totals:   Totals{LastHour: Counter{Turns: 10, ToolErrors: 1}, GitHubProcesses: 2},
		Overlaps: []Overlap{{Kind: "ref", Key: tIssue, Sessions: []string{tBee, tWasp}}},
		Leases: Leases{
			Held:   []Lease{{Resource: tRepo, Holder: tBee, Purpose: "push", State: "live", Since: testAt.Add(-2 * time.Hour), UpgradeUnblock: "beekeeper"}},
			Free:   []string{"repo/fork"},
			Queues: map[string][]Grant{tRepo: {{Resource: tRepo, To: tWasp, By: tSup, At: testAt.Add(-time.Minute)}}},
		},
		Holds: []Hold{{Target: tRepo, Reason: "release", By: "me", At: testAt, Until: testAt.Add(3 * time.Hour), Except: "notes", Tool: "helm-apps", ToolFrom: "5.2.0", ToolRelease: "5.3.0"}},
		Lanes: []Lane{{
			Name: "main", Installation: tOrg,
			Running:  &Merge{Key: "giantswarm/beekeeper#2", By: tBee, Started: testAt.Add(-time.Minute)},
			Settling: []*Merge{{Key: "giantswarm/beekeeper#3"}},
			Waiting:  []Merge{{Key: "giantswarm/beekeeper#4"}, {Key: "giantswarm/beekeeper#5"}, {Key: "#6"}, {Key: "#7"}},
			Stall:    "#4 waits behind #5",
		}},
		Roles: []Role{
			{Name: tSupervisor, Holder: tSup, Live: true, Since: testAt.Add(-24 * time.Hour), Context: 120000, RelayAt: 400000},
			{Name: tGuide, Holder: "gui", Gone: testAt.Add(-5 * time.Minute), Context: 1000},
		},
		Agents:  []Agent{{Name: tWasp, Task: "fix #9", AssignedAt: testAt.Add(-time.Hour), Reachable: "waiting on person"}},
		Notes:   []Note{{ID: 3, For: "me", Text: "approve the bump", Default: "yes", Due: testAt.Add(-time.Hour), By: tBee}},
		Timers:  []Timer{{ID: 4, Due: testAt.Add(30 * time.Minute), What: "check the rollout", By: "me"}},
		Records: []Record{{Session: tBee, Issue: tIssue, Waits: "review"}},
		Alerts: []AlertsFor{
			{Installation: tOrg, Reachable: true, Alerts: []Alert{
				{Severity: "warning", Team: "tiger", Alertname: "KubePodCrashLooping", Cluster: "w-1", Since: "2h"},
				{Severity: "critical", Team: "atlas", Alertname: "TargetDown", Cluster: "w-2", Since: "10m"},
			}},
			{Installation: "mgmt", Reachable: false},
		},
		Upgrades: []string{"devctl 1.2.0 -> 1.3.0 on giantswarm"},
		Events: []Event{
			{At: testAt.Add(-time.Minute), Verb: tClaim, By: tBee, Detail: tRepo},
			{At: testAt, Verb: tGrant, By: tSup, Detail: "repo/beekeeper to wasp"},
			{At: testAt.Add(-2 * time.Minute), Verb: tHoldSet, By: "me", Detail: "repo/beekeeper release"},
		},
	}
}

// newTestModel builds a model that has seen one fixture refresh at
// 100x30.
func newTestModel(t *testing.T, src Source) *model {
	t.Helper()
	m := newModel(src, Options{}).(*model)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if _, cmd := m.Update(refreshMsg{data: src.(*fakeSource).data}); cmd != nil {
		t.Fatalf("refreshMsg asked for a command")
	}
	return m
}

func TestRefreshKeepsLastGoodDataOnError(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)
	first := m.data

	want := errors.New("kubectl timed out")
	m.Update(refreshMsg{err: want})

	if m.data != first {
		t.Errorf("a failed refresh replaced the data")
	}
	if m.err == nil || m.err.Error() != want.Error() {
		t.Errorf("error = %v, want %v", m.err, want)
	}
	if !strings.Contains(m.View(), want.Error()) {
		t.Errorf("footer does not show the error:\n%s", m.View())
	}

	fresh := fixtureData()
	fresh.Status.Due = 9
	m.Update(refreshMsg{data: fresh})
	if m.data != fresh || m.err != nil {
		t.Errorf("a good refresh did not swap the data and clear the error")
	}
}
func TestTicksDoNotStack(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)
	// Swap the ticker for one that fires at once, so the commands the
	// model hands out can be run without waiting on the real interval.
	m.tickFn = func() tea.Cmd {
		return func() tea.Msg { return tickMsg{} }
	}

	// While a read is outstanding a tick may only re-arm the ticker.
	m.refreshing = true
	_, c := m.Update(tickMsg{})
	if c == nil {
		t.Fatal("a tick while refreshing dropped the next tick entirely")
	}
	for _, msg := range msgs(c) {
		if _, ok := msg.(refreshMsg); ok {
			t.Error("a tick while refreshing handed out a second read")
		}
	}
	if src.dataCalls != 0 {
		t.Errorf("dataCalls = %d while a read was outstanding, want 0", src.dataCalls)
	}
	if !m.refreshing {
		t.Error("a tick while refreshing cleared the outstanding flag")
	}

	// With nothing outstanding the tick hands out the read and the
	// next tick, and the read is exactly one call.
	m.refreshing = false
	_, c = m.Update(tickMsg{})
	var reads int
	for _, msg := range msgs(c) {
		if _, ok := msg.(refreshMsg); ok {
			reads++
		}
	}
	if reads != 1 || src.dataCalls != 1 {
		t.Errorf("tick handed out %d reads, dataCalls %d, want one of each", reads, src.dataCalls)
	}
	if !m.refreshing {
		t.Error("a handed-out read did not mark itself outstanding")
	}
}

// msgs runs c and collects its messages, expanding a BatchMsg the way
// the program would. Tests swap in a stub tickFn first, so no command
// here waits on a clock.
func msgs(c tea.Cmd) []tea.Msg {
	if c == nil {
		return nil
	}
	msg := c()
	if bm, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, f := range bm {
			if f != nil {
				out = append(out, f())
			}
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestSelectionClampsWhenDataShrinks(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	m.move(5, m.rows())
	if m.sel[1] != 1 {
		t.Fatalf("sel = %d, want the last of two sessions", m.sel[1])
	}

	small := fixtureData()
	small.Sessions = small.Sessions[:1]
	m.Update(refreshMsg{data: small})
	if m.sel[1] != 0 {
		t.Errorf("sel = %d after the list shrank, want 0", m.sel[1])
	}
}

func TestQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
	} {
		m := newTestModel(t, &fakeSource{data: fixtureData()})
		_, quit := m.Update(key)
		if quit == nil {
			t.Fatalf("%s did not return a quit command", key)
		}
		if _, ok := quit().(tea.QuitMsg); !ok {
			t.Errorf("%s cmd did not return a quit message", key)
		}
		if !m.quitting {
			t.Errorf("%s did not mark the model quitting", key)
		}
		if got := m.View(); got != "" {
			t.Errorf("%s: View after quit = %q, want empty", key, got)
		}
		if err := m.ctx.Err(); err == nil {
			t.Errorf("%s did not cancel the source context", key)
		}
	}
}

func TestTabKeys(t *testing.T) {
	m := newTestModel(t, &fakeSource{data: fixtureData()})

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("5")})
	if m.tab != 4 {
		t.Errorf("5 selected tab %d, want alerts", m.tab)
	}
	m.key(tea.KeyMsg{Type: tea.KeyTab})
	if m.tab != 5 {
		t.Errorf("tab cycled to %d, want events", m.tab)
	}
	m.key(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.tab != 4 {
		t.Errorf("shift+tab cycled to %d, want alerts", m.tab)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	m.key(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.tab != len(tabs)-1 {
		t.Errorf("shift+tab from the first tab went to %d, want the last", m.tab)
	}
	if _, cmd := m.Update(refreshMsg{data: fixtureData()}); cmd != nil {
		t.Errorf("refresh asked for a command")
	}
}

func TestSelectionMovement(t *testing.T) {
	m := newTestModel(t, &fakeSource{data: fixtureData()})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("6")})

	m.key(tea.KeyMsg{Type: tea.KeyEnd})
	if m.sel[5] != 2 {
		t.Fatalf("end sel = %d, want the last of three events", m.sel[5])
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.sel[5] != 1 {
		t.Errorf("k sel = %d, want 1", m.sel[5])
	}
	m.key(tea.KeyMsg{Type: tea.KeyHome})
	if m.sel[5] != 0 {
		t.Errorf("home sel = %d, want 0", m.sel[5])
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if m.sel[5] != 2 {
		t.Errorf("G sel = %d, want 2", m.sel[5])
	}
	m.key(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.sel[5] != 0 {
		t.Errorf("pgup sel = %d, want 0 (clamped)", m.sel[5])
	}
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	if m.sel[5] != 0 {
		t.Errorf("up at the top moved to %d", m.sel[5])
	}
}

func TestEnterOpensSessionPaneOnlyThere(t *testing.T) {
	src := &fakeSource{data: fixtureData(), turns: []Turn{
		{At: testAt, Role: "assistant", Text: "on it"},
		{At: testAt, Role: "user", Text: "the second turn"},
	}}
	m := newTestModel(t, src)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.detail != "" {
		t.Fatal("enter on the watching tab opened a pane")
	}

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.detail != tBee || m.tailState != 1 {
		t.Fatalf("detail = %q state %d, want bee loading", m.detail, m.tailState)
	}
	got := msgs(c)
	if len(got) != 1 {
		t.Fatalf("enter asked for %d messages, want one tail", len(got))
	}
	if src.tailCall != tBee || src.tailN != tailTurns {
		t.Errorf("Tail(%q, %d), want bee, %d", src.tailCall, src.tailN, tailTurns)
	}
	if body := m.View(); !strings.Contains(body, "transcript") || !strings.Contains(body, "…") {
		t.Errorf("the loading pane shows no transcript header and ellipsis:\n%s", body)
	}

	m.Update(got[0])
	body := m.View()
	for _, want := range []string{tBee, "on it", "answer to the question", "$1.23", "sleep 60", "45%"} {
		if !strings.Contains(body, want) {
			t.Errorf("the pane shows no %q:\n%s", want, body)
		}
	}

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.tailOff != 1 {
		t.Errorf("j scrolled to %d, want 1", m.tailOff)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.tailOff != 0 {
		t.Errorf("k below zero scrolled to %d", m.tailOff)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.detail != "" {
		t.Errorf("esc left the pane open")
	}

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, c = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	src.tailErr = errors.New("no transcript")
	for _, msg := range msgs(c) {
		m.Update(msg)
	}
	if m.tailState != 3 {
		t.Fatalf("tail state = %d after a failed read, want failed", m.tailState)
	}
	if !strings.Contains(m.View(), "no transcript") {
		t.Errorf("the pane shows no error line:\n%s", m.View())
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.detail != "" {
		t.Errorf("enter did not close the pane")
	}
}

func TestForceRefresh(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)

	_, c := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	for _, msg := range msgs(c) {
		m.Update(msg)
	}
	if src.dataCalls != 1 {
		t.Errorf("dataCalls = %d after r, want 1", src.dataCalls)
	}
}

func TestLateTailForClosedPaneIsDropped(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	m.Update(tailMsg{session: tBee, turns: []Turn{{Text: "late"}}})
	if m.tailState != 0 || len(m.tail) != 0 {
		t.Errorf("a late tail reopened the pane: state %d, %d turns", m.tailState, len(m.tail))
	}
}

func TestLoadingBeforeFirstRefresh(t *testing.T) {
	m := newModel(&fakeSource{}, Options{}).(*model)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(m.View(), "loading…") {
		t.Errorf("the first View is not a loading body:\n%s", m.View())
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.detail != "" {
		t.Errorf("enter without data opened a pane")
	}
}

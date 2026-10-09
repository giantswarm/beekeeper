package tui

import (
	"context"
	"errors"
	"image/color"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

	sendTo, sendText string
	sendErr          error

	took, released []string
	answers        []string
	answerErr      error
}

// TakeOver records the take-over and marks the session in the data.
func (f *fakeSource) TakeOver(_ context.Context, id string) error {
	f.took = append(f.took, id)
	for i := range f.data.Sessions {
		if f.data.Sessions[i].ID == id {
			f.data.Sessions[i].TakenOver = true
		}
	}
	return nil
}

// Release records the hand-back and unmarks the session.
func (f *fakeSource) Release(_ context.Context, id string) error {
	f.released = append(f.released, id)
	for i := range f.data.Sessions {
		if f.data.Sessions[i].ID == id {
			f.data.Sessions[i].TakenOver, f.data.Sessions[i].Approvals = false, nil
		}
	}
	return nil
}

// Answer records "id/request allow|deny".
func (f *fakeSource) Answer(_ context.Context, id, request string, allow bool) error {
	verb := "deny"
	if allow {
		verb = "allow"
	}
	f.answers = append(f.answers, id+"/"+request+" "+verb)
	return f.answerErr
}

// Send records the message and answers as told.
func (f *fakeSource) Send(_ context.Context, session, text string) (string, error) {
	f.sendTo, f.sendText = session, text
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "queued in its CLI", nil
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
	if !strings.Contains(m.screen(), want.Error()) {
		t.Errorf("footer does not show the error:\n%s", m.screen())
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

	m.key(keyText("2"))
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
	for _, key := range []tea.KeyPressMsg{
		keyText("q"),
		tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
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
		if got := m.screen(); got != "" {
			t.Errorf("%s: View after quit = %q, want empty", key, got)
		}
		if err := m.ctx.Err(); err == nil {
			t.Errorf("%s did not cancel the source context", key)
		}
	}
}

func TestTabKeys(t *testing.T) {
	m := newTestModel(t, &fakeSource{data: fixtureData()})

	m.key(keyText("5"))
	if m.tab != 4 {
		t.Errorf("5 selected tab %d, want alerts", m.tab)
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.tab != 5 {
		t.Errorf("tab cycled to %d, want events", m.tab)
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.tab != 4 {
		t.Errorf("shift+tab cycled to %d, want alerts", m.tab)
	}
	m.key(keyText("1"))
	m.key(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.tab != len(tabs)-1 {
		t.Errorf("shift+tab from the first tab went to %d, want the last", m.tab)
	}
	if _, cmd := m.Update(refreshMsg{data: fixtureData()}); cmd != nil {
		t.Errorf("refresh asked for a command")
	}
}

func TestSelectionMovement(t *testing.T) {
	m := newTestModel(t, &fakeSource{data: fixtureData()})
	m.key(keyText("6"))

	m.key(tea.KeyPressMsg{Code: tea.KeyEnd})
	if m.sel[5] != 2 {
		t.Fatalf("end sel = %d, want the last of three events", m.sel[5])
	}
	m.key(keyText("k"))
	if m.sel[5] != 1 {
		t.Errorf("k sel = %d, want 1", m.sel[5])
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyHome})
	if m.sel[5] != 0 {
		t.Errorf("home sel = %d, want 0", m.sel[5])
	}
	m.key(keyText("G"))
	if m.sel[5] != 2 {
		t.Errorf("G sel = %d, want 2", m.sel[5])
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.sel[5] != 0 {
		t.Errorf("pgup sel = %d, want 0 (clamped)", m.sel[5])
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.sel[5] != 0 {
		t.Errorf("up at the top moved to %d", m.sel[5])
	}
}

func TestEnterOpensSessionPaneOnlyThere(t *testing.T) {
	src := &fakeSource{data: fixtureData(), turns: []Turn{
		{At: testAt, Role: roleAssistant, Text: "on it"},
		{At: testAt, Role: roleUser, Text: "the second turn"},
	}}
	m := newTestModel(t, src)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.detail != "" {
		t.Fatal("enter on the watching tab opened a pane")
	}

	m.key(keyText("2"))
	_, c := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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
	if body := m.screen(); !strings.Contains(body, "transcript") || !strings.Contains(body, "…") {
		t.Errorf("the loading pane shows no transcript header and ellipsis:\n%s", body)
	}

	m.Update(got[0])
	body := m.screen()
	for _, want := range []string{tBee, "on it", "answer to the question", "$1.23", "sleep 60", "45%"} {
		if !strings.Contains(body, want) {
			t.Errorf("the pane shows no %q:\n%s", want, body)
		}
	}

	m.key(keyText("k"))
	m.key(keyText("k"))
	if m.tailBack != 1 {
		t.Errorf("k past the oldest turn scrolled back %d, want 1", m.tailBack)
	}
	m.key(keyText("j"))
	m.key(keyText("j"))
	if m.tailBack != 0 {
		t.Errorf("j past the newest turn scrolled back %d", m.tailBack)
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.detail != "" {
		t.Errorf("esc left the pane open")
	}

	m.key(keyText("j"))
	_, c = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	src.tailErr = errors.New("no transcript")
	for _, msg := range msgs(c) {
		m.Update(msg)
	}
	if m.tailState != 3 {
		t.Fatalf("tail state = %d after a failed read, want failed", m.tailState)
	}
	if !strings.Contains(m.screen(), "no transcript") {
		t.Errorf("the pane shows no error line:\n%s", m.screen())
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.detail != "" {
		t.Errorf("enter did not close the pane")
	}
}

func TestOpenPaneFollowsLive(t *testing.T) {
	first := []Turn{
		{At: testAt, Role: roleUser, Text: "fix it"},
		{At: testAt, Role: roleAssistant, Text: "reading"},
	}
	src := &fakeSource{data: fixtureData(), turns: first}
	m := newTestModel(t, src)
	m.tickFn = func() tea.Cmd { return nil }
	m.key(keyText("2"))
	_, c := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, msg := range msgs(c) {
		m.Update(msg)
	}
	if !strings.Contains(m.screen(), "live") {
		t.Errorf("a pane at the newest turn is not marked live:\n%s", m.screen())
	}

	// A tick while the pane is open re-reads its tail; the new turn shows
	// at the bottom.
	src.turns = append(slices.Clone(first), Turn{At: testAt, Role: roleTool, Text: "Bash: go test ./..."})
	src.tailCall = ""
	_, c = m.Update(tickMsg{})
	if c == nil {
		t.Fatal("a tick with the pane open asked for nothing")
	}
	got := msgs(c)
	if src.tailCall != tBee {
		t.Fatalf("the tick did not re-read the tail: Tail(%q)", src.tailCall)
	}
	for _, msg := range got {
		m.Update(msg)
	}
	lines := strings.Split(m.screen(), "\n")
	if body := strings.Join(lines[len(lines)-3:], "\n"); !strings.Contains(body, "Bash: go test ./...") {
		t.Errorf("the new tool call is not at the pane's bottom:\n%s", m.screen())
	}

	// A second tick while a read is out starts no second read.
	m.tailing = true
	src.tailCall = ""
	_, c = m.Update(tickMsg{})
	_ = msgs(c)
	if src.tailCall != "" {
		t.Errorf("a tick started a second tail read while one was out")
	}
	m.tailing = false

	// Scrolled back, the view keeps its place as turns arrive.
	m.key(keyText("k"))
	src.turns = append(slices.Clone(src.turns), Turn{At: testAt, Role: roleAssistant, Text: "green"})
	m.Update(tailMsg{session: tBee, turns: src.turns})
	if m.tailBack != 2 {
		t.Errorf("scrolled back %d after one new turn, want 2 (the place kept)", m.tailBack)
	}
	if v := m.screen(); !strings.Contains(v, "2 back") || strings.Contains(v, "green") {
		t.Errorf("the scrolled-back pane moved or lost its mark:\n%s", v)
	}
	m.key(keyText("G"))
	if m.tailBack != 0 || !strings.Contains(m.screen(), "green") {
		t.Errorf("G did not follow live again: back %d", m.tailBack)
	}

	// A failed re-read keeps the turns shown and says why.
	m.Update(tailMsg{session: tBee, err: errors.New("transcript moved")})
	if v := m.screen(); m.tailState != 2 || !strings.Contains(v, "green") || !strings.Contains(v, "transcript moved") {
		t.Errorf("a failed re-read dropped the turns or the reason (state %d):\n%s", m.tailState, v)
	}
}

func TestPasteLandsInTheDraft(t *testing.T) {
	m := newTestModel(t, &fakeSource{data: fixtureData()})
	m.Update(tea.PasteMsg{Content: "q ignored"})
	if m.quitting || len(m.draft) != 0 {
		t.Fatalf("a paste outside the message line acted (draft %q, quitting %v)", string(m.draft), m.quitting)
	}
	m.composing = true
	m.key(keyText("a"))
	m.Update(tea.PasteMsg{Content: " pasted ü"})
	if string(m.draft) != "a pasted ü" {
		t.Errorf("draft = %q, want the typed key and the paste", string(m.draft))
	}
}

func TestBackgroundPicksThePalette(t *testing.T) {
	t.Cleanup(func() { style = newStyles(true) })
	m := newTestModel(t, &fakeSource{data: fixtureData()})
	dark := style.Dim.Render("x")
	m.Update(tea.BackgroundColorMsg{Color: color.White})
	if light := style.Dim.Render("x"); light == dark || !strings.Contains(light, "98;98;98") {
		t.Errorf("a light background kept the dark palette: %q", light)
	}
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if again := style.Dim.Render("x"); again != dark {
		t.Errorf("a dark background did not bring the dark palette back: %q", again)
	}
}

func TestMessageFromThePane(t *testing.T) {
	src := &fakeSource{data: fixtureData(), turns: []Turn{{At: testAt, Role: roleAssistant, Text: "working"}}}
	m := newTestModel(t, src)
	m.key(keyText("2"))
	_, c := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, msg := range msgs(c) {
		m.Update(msg)
	}
	m.key(keyText("m"))
	if !m.composing {
		t.Fatal("m did not open the message line")
	}
	// Every key is the message's now: q, j and G are letters, not
	// commands.
	for _, k := range []tea.KeyPressMsg{
		keyText("q"), tea.KeyPressMsg{Code: tea.KeySpace, Text: " "},
		keyText("jGx"), tea.KeyPressMsg{Code: tea.KeyBackspace},
	} {
		m.key(k)
	}
	if m.quitting || string(m.draft) != "q jG" {
		t.Fatalf("draft = %q (quitting %v), want the keys typed", string(m.draft), m.quitting)
	}
	if v := ansi.Strip(m.screen()); !strings.Contains(v, "message › q jG") || !strings.Contains(v, "enter send") {
		t.Errorf("the pane shows no message line or hints:\n%s", v)
	}
	_, c = m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.composing || !m.sending || c == nil {
		t.Fatalf("enter did not send: composing %v sending %v", m.composing, m.sending)
	}
	if !strings.Contains(m.screen(), "sending…") {
		t.Errorf("a message on its way is not shown:\n%s", m.screen())
	}
	m.key(keyText("m"))
	if m.composing {
		t.Error("m opened a second message while one is on its way")
	}
	m.Update(c())
	if src.sendTo != tBee || src.sendText != "q jG" {
		t.Errorf("Send(%q, %q), want bee and the draft", src.sendTo, src.sendText)
	}
	if v := m.screen(); m.sending || !strings.Contains(v, "sent: queued in its CLI") {
		t.Errorf("the outcome is not shown (sending %v):\n%s", m.sending, v)
	}

	// An empty draft sends nothing; esc drops a draft; a refusal says why.
	m.key(keyText("m"))
	if _, c := m.key(tea.KeyPressMsg{Code: tea.KeyEnter}); c != nil || !m.composing {
		t.Error("an empty draft was sent")
	}
	m.key(keyText("hi"))
	m.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.composing || len(m.draft) != 0 || m.detail == "" {
		t.Errorf("esc did not drop just the draft: composing %v draft %q pane %q", m.composing, string(m.draft), m.detail)
	}
	src.sendErr = errors.New("an omp session beekeeper did not start takes no message")
	m.key(keyText("m"))
	m.key(keyText("hi"))
	_, c = m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(c())
	if !strings.Contains(m.screen(), "not sent: an omp session") {
		t.Errorf("a refusal is not shown:\n%s", m.screen())
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.outcome != "" {
		t.Error("closing the pane kept the last outcome")
	}
}

// run feeds a command's messages back into the model, and the commands
// those answer with, as the program does; the test's tickFn arms no tick.
func run(m *model, c tea.Cmd) {
	for queue := []tea.Cmd{c}; len(queue) > 0; queue = queue[1:] {
		for _, msg := range msgs(queue[0]) {
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func TestTakeOverFromThePane(t *testing.T) {
	src := &fakeSource{data: fixtureData(), turns: []Turn{{At: testAt, Role: roleAssistant, Text: "about to clean"}}}
	src.data.Sessions[0].ID = "sid-bee"
	m := newTestModel(t, src)
	m.tickFn = func() tea.Cmd { return nil }
	m.key(keyText("2"))
	_, c := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	run(m, c)

	_, c = m.key(keyText("t"))
	run(m, c)
	if len(src.took) != 1 || src.took[0] != "sid-bee" || !m.taken["sid-bee"] {
		t.Fatalf("t took %v (screen holds %v), want bee's id", src.took, m.taken)
	}
	if v := m.screen(); !strings.Contains(v, "taken over") || !strings.Contains(v, "t hand back") {
		t.Errorf("the pane does not show the take-over:\n%s", v)
	}

	// An approval arrives: the pane shows it with its input and the keys.
	ap := Approval{ID: "req-1", At: testAt, Gist: "Bash: Clean the build", Detail: []string{"command: rm -r build"}}
	src.data.Sessions[0].Approvals = []Approval{ap}
	run(m, m.refreshCmd())
	v := m.screen()
	for _, want := range []string{"approve?", "Bash: Clean the build", "command: rm -r build", "a allow", "about to clean"} {
		if !strings.Contains(v, want) {
			t.Errorf("the pane misses %q:\n%s", want, v)
		}
	}
	_, c = m.key(keyText("a"))
	run(m, c)
	if len(src.answers) != 1 || src.answers[0] != "sid-bee/req-1 allow" {
		t.Fatalf("a answered %v", src.answers)
	}
	if !strings.Contains(m.screen(), "allowed: Bash: Clean the build") {
		t.Errorf("the answer is not shown:\n%s", m.screen())
	}
	// The hook lets go of the answered request: that is no hand-back.
	src.data.Sessions[0].Approvals = nil
	run(m, m.refreshCmd())
	if strings.Contains(m.screen(), "handed back") {
		t.Errorf("an answered request read as handed back:\n%s", m.screen())
	}

	// One the screen did not answer goes away: it went back to the window.
	ap.ID, ap.Gist = "req-2", "Write: /w/notes.md"
	src.data.Sessions[0].Approvals = []Approval{ap}
	run(m, m.refreshCmd())
	src.data.Sessions[0].Approvals = nil
	run(m, m.refreshCmd())
	if !strings.Contains(m.screen(), "handed back to its window: Write: /w/notes.md") {
		t.Errorf("the hand-back is not said:\n%s", m.screen())
	}

	// d denies; a late answer says why it did not land.
	ap.ID = "req-3"
	src.data.Sessions[0].Approvals = []Approval{ap}
	run(m, m.refreshCmd())
	src.answerErr = errors.New("the request is no longer held")
	_, c = m.key(keyText("d"))
	run(m, c)
	if src.answers[len(src.answers)-1] != "sid-bee/req-3 deny" || !strings.Contains(m.screen(), "no longer held") {
		t.Errorf("d answered %v; pane:\n%s", src.answers, m.screen())
	}

	// t again hands back; quitting hands back what is still taken.
	_, c = m.key(keyText("t"))
	run(m, c)
	if len(src.released) != 1 || m.taken["sid-bee"] {
		t.Fatalf("t did not hand back: released %v, taken %v", src.released, m.taken)
	}
	m.taken = map[string]bool{"sid-wasp": true}
	m.releaseAll()
	if len(src.released) != 2 || src.released[1] != "sid-wasp" {
		t.Errorf("closing released %v, want wasp's too", src.released)
	}
}

func TestSelectionFollowsTheSessionAcrossReorders(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)
	m.key(keyText("2"))
	m.key(keyText("j"))
	picked := m.selectedSession()
	d := *src.data
	d.Sessions = slices.Clone(d.Sessions)
	slices.Reverse(d.Sessions)
	m.Update(refreshMsg{data: &d})
	if got := m.selectedSession(); got != picked {
		t.Errorf("after a reorder the selection is on %q, want %q", got, picked)
	}
}

func TestStateOf(t *testing.T) {
	cases := []struct {
		s    Session
		want string
	}{
		{Session{Harness: "omp", State: stateIdle, Idle: time.Second}, stateIdle},
		{Session{Waiting: "approve the merge", Idle: time.Second}, stateWaiting},
		{Session{Idle: 10 * time.Second}, stateBusy},
		{Session{Idle: time.Hour, Commands: []Command{{Args: "devctl pr wait"}}}, stateBusy},
		{Session{Idle: time.Hour}, stateIdle},
		{Session{TakenOver: true, Idle: time.Second}, stateTaken},
		{Session{TakenOver: true, Approvals: []Approval{{ID: "r"}}, Waiting: "x"}, stateApproval},
	}
	for _, c := range cases {
		if got := stateOf(c.s); got != c.want {
			t.Errorf("stateOf(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestForceRefresh(t *testing.T) {
	src := &fakeSource{data: fixtureData()}
	m := newTestModel(t, src)

	_, c := m.Update(keyText("r"))
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
	m.key(keyText("2"))
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.Update(tailMsg{session: tBee, turns: []Turn{{Text: "late"}}})
	if m.tailState != 0 || len(m.tail) != 0 {
		t.Errorf("a late tail reopened the pane: state %d, %d turns", m.tailState, len(m.tail))
	}
}

func TestLoadingBeforeFirstRefresh(t *testing.T) {
	m := newModel(&fakeSource{}, Options{}).(*model)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(m.screen(), "loading…") {
		t.Errorf("the first View is not a loading body:\n%s", m.screen())
	}
	m.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.detail != "" {
		t.Errorf("enter without data opened a pane")
	}
}

// keyText is the key press that types text.
func keyText(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(text)[0], Text: text}
}

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// tabAlerts is the alerts tab's name.
const tabAlerts = "alerts"

// tabs are the six views in key order: the digit keys pick one and
// tab/shift+tab cycle through them.
var tabs = [...]string{"watching", "sessions", "sharing", "supervising", tabAlerts, "events"}

// tailTurns is how many transcript turns the detail pane asks the Source
// for: the history above the current turn.
const tailTurns = 60

// Messages between the program and the screen's data source. A tick only
// starts a refresh while none is outstanding, so slow reads never stack.
type (
	// tickMsg asks for a refresh; the model drops it while one runs.
	tickMsg time.Time
	// refreshMsg carries one refresh's result; the model keeps the last
	// good Data when err is set.
	refreshMsg struct {
		data *Data
		err  error
	}
	// tailMsg carries a detail pane's transcript; session names whose
	// transcript it is, so a late answer about a closed pane is dropped.
	// The open pane asks again at every tick: it follows the turn live.
	tailMsg struct {
		session string
		turns   []Turn
		err     error
	}
	// sentMsg carries a message's delivery: where it went, or why not.
	sentMsg struct {
		session string
		where   string
		err     error
	}
	// actMsg carries a take-over's, a release's or an answer's result:
	// done says what happened, err why it did not.
	actMsg struct {
		done string
		err  error
		// took is the session id a take-over took, "" for anything else.
		took string
	}
)

// model is the screen's state: which tab is up, what it shows, the open
// detail pane and the context every in-flight read runs under. It
// implements tea.Model.
type model struct {
	src  Source
	intv time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	// tickFn arms the next interval tick; it is a field so tests can
	// tell the ticker from a read without waiting on either.
	tickFn func() tea.Cmd

	data       *Data
	err        error
	updated    time.Time
	refreshing bool

	tab int
	sel [len(tabs)]int

	detail    string
	tail      []Turn
	tailState int // 0 closed, 1 loading, 2 ready, 3 failed
	tailErr   string
	// tailBack is how many turns the pane is scrolled back from the
	// newest: 0 follows the session live, more holds the view where the
	// person scrolled to while new turns arrive below it.
	tailBack int
	// tailing says a tail read is outstanding: a tick starts no second.
	tailing bool

	// composing says the pane's message line has the keys; draft is
	// what the person typed. sending says a message is on its way.
	composing bool
	draft     []rune
	sending   bool
	// outcome is the pane's last action's result (a message, a take-over,
	// an answer, a request handed back), outcomeErr set when it failed.
	outcome    string
	outcomeErr bool
	// acting says a take-over, release or answer is on its way.
	acting bool

	// taken are the sessions (by id) this screen took over: quitting
	// hands them back. seen are the approvals the screen showed, by
	// request id, that it did not answer: one that goes away went back to
	// its session's window.
	taken map[string]bool
	seen  map[string]Approval
	// answered are the requests the screen answered and the hook has
	// not yet let go of.
	answered map[string]bool

	width, height int
	quitting      bool
}

// newModel builds the screen's model; Run calls it, and tests build one
// around a fake Source.
func newModel(src Source, opts Options) tea.Model {
	intv := opts.Interval
	if intv <= 0 {
		intv = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &model{src: src, intv: intv, ctx: ctx, cancel: cancel}
	m.tickFn = m.waitTick
	return m
}

// Init starts the first refresh and the ticker.
func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.tick(), tea.RequestBackgroundColor)
}

// waitTick waits one interval before asking for the next refresh.
func (m *model) waitTick() tea.Cmd {
	return tea.Tick(m.intv, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// tick arms the next refresh tick.
func (m *model) tick() tea.Cmd {
	return m.tickFn()
}

// refreshCmd reads one Data under the model's context and marks the
// refresh outstanding until its result lands.
func (m *model) refreshCmd() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	return func() tea.Msg {
		d, err := m.src.Data(m.ctx)
		return refreshMsg{data: d, err: err}
	}
}

// tailCmd reads one session's transcript tail under the model's context
// and marks the read outstanding until its result lands.
func (m *model) tailCmd(session string) tea.Cmd {
	if m.tailing {
		return nil
	}
	m.tailing = true
	return func() tea.Msg {
		turns, err := m.src.Tail(m.ctx, session, tailTurns)
		return tailMsg{session: session, turns: turns, err: err}
	}
}

// Update handles every message: ticks and results, keys, resize.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.BackgroundColorMsg:
		style = newStyles(msg.IsDark())
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{m.refreshCmd(), m.tick()}
		if m.detail != "" {
			cmds = append(cmds, m.tailCmd(m.detail))
		}
		return m, tea.Batch(cmds...)
	case refreshMsg:
		m.refreshing = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			picked := m.selectedSession()
			m.data = msg.data
			m.err = nil
			m.keepSelection(picked)
		}
		m.updated = time.Now()
		m.clampAll()
		m.noticeHandedBack()
		return m, nil
	case tailMsg:
		if msg.session != m.detail {
			return m, nil
		}
		m.tailing = false
		m.takeTail(msg)
		return m, nil
	case sentMsg:
		m.sending = false
		if msg.err != nil {
			m.outcome, m.outcomeErr = "not sent: "+msg.err.Error(), true
		} else {
			m.outcome, m.outcomeErr = "sent: "+msg.where, false
		}
		return m, nil
	case actMsg:
		m.acting = false
		if msg.err != nil {
			m.outcome, m.outcomeErr = msg.err.Error(), true
			return m, m.refreshCmd()
		}
		m.outcome, m.outcomeErr = msg.done, false
		if msg.took != "" {
			if m.taken == nil {
				m.taken = map[string]bool{}
			}
			m.taken[msg.took] = true
		}
		return m, m.refreshCmd()
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg:
		if m.composing {
			m.draft = append(m.draft, []rune(msg.Content)...)
		}
		return m, nil
	}
	return m, nil
}

// takeTail lands a tail read in the open pane. A failed re-read keeps the
// turns already shown and says why; a pane scrolled back keeps its place
// by the turns that arrived after its newest.
func (m *model) takeTail(msg tailMsg) {
	if msg.err != nil {
		m.tailErr = msg.err.Error()
		if m.tailState != 2 {
			m.tailState, m.tail = 3, nil
		}
		return
	}
	if m.tailBack > 0 && len(m.tail) > 0 {
		m.tailBack += newer(msg.turns, m.tail[len(m.tail)-1])
	}
	m.tailState, m.tail, m.tailErr = 2, msg.turns, ""
	m.tailBack = min(m.tailBack, maxRow(len(m.tail)))
}

// newer counts the turns of ts after last: the ones a re-read added.
func newer(ts []Turn, last Turn) int {
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i] == last {
			return len(ts) - 1 - i
		}
	}
	return 0
}

// sendCmd delivers text to session under the model's context.
func (m *model) sendCmd(session, text string) tea.Cmd {
	m.sending = true
	return func() tea.Msg {
		where, err := m.src.Send(m.ctx, session, text)
		return sentMsg{session: session, where: where, err: err}
	}
}

// key routes a key press. With the detail pane open j/k scroll its
// transcript and enter/esc close it, and while the person writes a
// message every key but ctrl+c is the message's; otherwise they move the
// tab's selection, which also drives the body's scroll.
func (m *model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.composing && msg.String() != "ctrl+c" {
		return m.compose(msg)
	}
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		m.cancel()
		return m, tea.Quit
	case "esc":
		if m.detail != "" {
			m.closeDetail()
		}
		return m, nil
	case "enter":
		return m.enter()
	}
	if m.detail != "" {
		return m.scrollDetail(msg)
	}
	return m.navigate(msg)
}

// enter opens the selected session's pane on the sessions tab and closes
// an open one; elsewhere it does nothing.
func (m *model) enter() (tea.Model, tea.Cmd) {
	if m.detail != "" {
		m.closeDetail()
		return m, nil
	}
	s := m.sessions()
	if m.tab != sessionsTab || m.sel[m.tab] >= len(s) {
		return m, nil
	}
	name := s[m.sel[m.tab]].Name
	m.detail, m.tail, m.tailErr, m.tailBack, m.tailState, m.tailing = name, nil, "", 0, 1, false
	return m, m.tailCmd(name)
}

// compose edits the message line: enter sends a non-empty draft, esc
// drops it, backspace and ctrl+u take back a character or everything;
// a pasted text lands in the draft through Update.
func (m *model) compose(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Code == tea.KeyEnter:
		text := strings.TrimSpace(string(m.draft))
		if text == "" {
			return m, nil
		}
		m.composing, m.draft, m.outcome = false, nil, ""
		return m, m.sendCmd(m.detail, text)
	case msg.Code == tea.KeyEscape:
		m.composing, m.draft = false, nil
	case msg.Code == tea.KeyBackspace:
		if len(m.draft) > 0 {
			m.draft = m.draft[:len(m.draft)-1]
		}
	case msg.String() == "ctrl+u":
		m.draft = nil
	case msg.Text != "":
		m.draft = append(m.draft, []rune(msg.Text)...)
	}
	return m, nil
}

// scrollDetail moves the transcript window of an open pane: k and up go
// back to older turns, j and down forward; G and end follow live again,
// g and home go to the oldest turn read.
func (m *model) scrollDetail(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	oldest := maxRow(len(m.tail))
	switch msg.String() {
	case "k", "up":
		m.tailBack = min(m.tailBack+1, oldest)
	case "j", "down":
		m.tailBack = max(m.tailBack-1, 0)
	case "pgup":
		m.tailBack = min(m.tailBack+m.page(), oldest)
	case "pgdown":
		m.tailBack = max(m.tailBack-m.page(), 0)
	case "g", "home":
		m.tailBack = oldest
	case "G", "end":
		m.tailBack = 0
	case "r":
		return m, m.refreshCmd()
	case "m":
		if !m.sending {
			m.composing, m.draft = true, nil
		}
	case "t":
		return m, m.toggleTakeOver()
	case "a", "d":
		return m, m.answer(msg.String() == "a")
	}
	return m, nil
}

// paneSession is the open pane's session in the current data, nil when it
// is gone.
func (m *model) paneSession() *Session {
	if m.data == nil || m.detail == "" {
		return nil
	}
	return sessionNamed(m.data.Sessions, m.detail)
}

// toggleTakeOver takes the pane's session over, or hands it back when this
// screen holds it.
func (m *model) toggleTakeOver() tea.Cmd {
	s := m.paneSession()
	if s == nil || m.acting {
		return nil
	}
	m.acting = true
	id, name := s.ID, s.Name
	if s.TakenOver {
		delete(m.taken, id)
		return func() tea.Msg {
			if err := m.src.Release(m.ctx, id); err != nil {
				return actMsg{err: err}
			}
			return actMsg{done: "handed back: " + name + "'s approvals go to its own window"}
		}
	}
	return func() tea.Msg {
		if err := m.src.TakeOver(m.ctx, id); err != nil {
			return actMsg{err: err}
		}
		return actMsg{done: "taken over: " + name + "'s approvals come here", took: id}
	}
}

// answer allows or denies the oldest approval the pane's session waits on.
func (m *model) answer(allow bool) tea.Cmd {
	s := m.paneSession()
	if s == nil || len(s.Approvals) == 0 || m.acting {
		return nil
	}
	m.acting = true
	ap := s.Approvals[0]
	if m.answered == nil {
		m.answered = map[string]bool{}
	}
	m.answered[ap.ID] = true
	verb := "denied"
	if allow {
		verb = "allowed"
	}
	id := s.ID
	return func() tea.Msg {
		if err := m.src.Answer(m.ctx, id, ap.ID, allow); err != nil {
			return actMsg{err: fmt.Errorf("%s: %w", ap.Gist, err)}
		}
		return actMsg{done: verb + ": " + ap.Gist}
	}
}

// noticeHandedBack keeps the approvals of this refresh and says so when one
// the screen showed went away without its answer: its session's window has
// it now (the hook gave up, the person answered it there, or the take-over
// ended).
func (m *model) noticeHandedBack() {
	now := map[string]Approval{}
	if m.data != nil {
		for _, s := range m.data.Sessions {
			for _, ap := range s.Approvals {
				now[ap.ID] = ap
			}
		}
	}
	for id, ap := range m.seen {
		if _, ok := now[id]; !ok && !m.answered[id] {
			m.outcome, m.outcomeErr = "handed back to its window: "+ap.Gist, true
		}
	}
	for id := range m.answered {
		if _, ok := now[id]; !ok {
			delete(m.answered, id)
		}
	}
	m.seen = now
}

// navigate moves between tabs and within the current tab's list.
func (m *model) navigate(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	rows := m.rows()
	sel := &m.sel[m.tab]
	switch msg.String() {
	case "r":
		return m, m.refreshCmd()
	case "tab":
		m.setTab((m.tab + 1) % len(tabs))
	case "shift+tab":
		m.setTab((m.tab + len(tabs) - 1) % len(tabs))
	case "1", "2", "3", "4", "5", "6":
		m.setTab(int(msg.String()[0] - '1'))
	case "j", "down":
		m.move(1, rows)
	case "k", "up":
		m.move(-1, rows)
	case "pgdown":
		m.move(m.page(), rows)
	case "pgup":
		m.move(-m.page(), rows)
	case "home", "g":
		*sel = 0
	case "end", "G":
		*sel = maxRow(rows)
	}
	return m, nil
}

// setTab moves to a tab and pulls its selection into range; the pane
// belongs to the sessions tab and closes.
func (m *model) setTab(t int) {
	m.tab = t
	m.closeDetail()
	m.clamp(t)
}

// move shifts the current tab's selection by n, clamped to its rows.
func (m *model) move(n, rows int) {
	sel := &m.sel[m.tab]
	*sel += n
	if *sel < 0 {
		*sel = 0
	}
	if *sel > maxRow(rows) {
		*sel = maxRow(rows)
	}
}

// page is how many rows a page turn moves.
func (m *model) page() int {
	if h := m.bodyH(); h > 1 {
		return h - 1
	}
	return 1
}

// pane is what the open pane shows beyond the session's facts.
type pane struct {
	tail        []Turn
	state, back int
	err         string
	composing   bool
	draft       string
	sending     bool
	outcome     string
	outcomeErr  bool
	acting      bool
}

// pane is the open pane's state for the view.
func (m *model) pane() pane {
	return pane{tail: m.tail, state: m.tailState, back: m.tailBack, err: m.tailErr,
		composing: m.composing, draft: string(m.draft), sending: m.sending, outcome: m.outcome, outcomeErr: m.outcomeErr, acting: m.acting}
}

// closeDetail dismisses the pane; a read still out lands nowhere.
func (m *model) closeDetail() {
	m.detail, m.tail, m.tailBack, m.tailState, m.tailing = "", nil, 0, 0, false
	m.composing, m.draft, m.outcome, m.outcomeErr = false, nil, "", false
}

// releaseAll hands back every session this screen took over: the screen
// is closing.
func (m *model) releaseAll() {
	for id := range m.taken {
		_ = m.src.Release(context.Background(), id)
	}
	m.taken = nil
}

// clampAll pulls every tab's selection into range after a refresh, when
// the data may have shrunk.
func (m *model) clampAll() {
	for t := range tabs {
		m.clamp(t)
	}
}

// clamp pulls one tab's selection into its row count.
func (m *model) clamp(t int) {
	m.sel[t] = min(m.sel[t], maxRow(m.rowsFor(t)))
}

// rowsFor is the selectable row count of a tab: its primary list. The
// watching tab moves over its waits, the sharing tab over held leases,
// the supervising tab over the roles, the alerts tab over installations,
// the events tab over the events.
func (m *model) rowsFor(t int) int {
	if m.data == nil {
		return 0
	}
	switch t {
	case 0:
		return len(m.data.Machine.Waits)
	case 1:
		return len(m.data.Sessions)
	case 2:
		return len(m.data.Leases.Held)
	case 3:
		return len(m.data.Roles)
	case 4:
		return len(m.data.Alerts)
	case 5:
		return len(m.data.Events)
	}
	return 0
}

// sessionsTab is the sessions tab's index.
const sessionsTab = 1

// selectedSession is the name of the session the sessions tab selects, ""
// when it selects none.
func (m *model) selectedSession() string {
	if s := m.sessions(); m.sel[sessionsTab] < len(s) {
		return s[m.sel[sessionsTab]].Name
	}
	return ""
}

// keepSelection moves the sessions tab's selection to name's new row: the
// list reorders as sessions get busy, and enter, t and a must act on the
// session the person picked, not on whichever took its place.
func (m *model) keepSelection(name string) {
	if name == "" {
		return
	}
	for i, s := range m.sessions() {
		if s.Name == name {
			m.sel[sessionsTab] = i
			return
		}
	}
}

// sessions is the current data's session list, empty before the first
// refresh.
func (m *model) sessions() []Session {
	if m.data == nil {
		return nil
	}
	return m.data.Sessions
}

// rows is the current tab's row count.
func (m *model) rows() int { return m.rowsFor(m.tab) }

// bodyH is the window the body shows in: the height minus the two header
// lines and the footer.
func (m *model) bodyH() int {
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

// maxRow is the last selectable row index of n rows.
func maxRow(n int) int {
	if n <= 0 {
		return 0
	}
	return n - 1
}

// View renders the whole window.
func (m *model) View() tea.View {
	return tea.NewView(m.screen())
}

// screen is the window's text. Before the first resize it assumes a
// standard terminal so a program can ask for it at any time.
func (m *model) screen() string {
	if m.quitting {
		return ""
	}
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return render(m, w, h)
}

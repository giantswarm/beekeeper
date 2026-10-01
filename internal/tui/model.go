package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// tabAlerts is the alerts tab's name.
const tabAlerts = "alerts"

// tabs are the six views in key order: the digit keys pick one and
// tab/shift+tab cycle through them.
var tabs = [...]string{"watching", "sessions", "sharing", "supervising", tabAlerts, "events"}

// tailTurns is how many transcript turns the detail pane asks the Source
// for.
const tailTurns = 10

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
	tailMsg struct {
		session string
		turns   []Turn
		err     error
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
	tailOff   int

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
	return tea.Batch(m.refreshCmd(), m.tick())
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

// tailCmd reads one session's transcript tail under the model's context.
func (m *model) tailCmd(session string) tea.Cmd {
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
	case tickMsg:
		if m.refreshing {
			return m, m.tick()
		}
		return m, tea.Batch(m.refreshCmd(), m.tick())
	case refreshMsg:
		m.refreshing = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.data = msg.data
			m.err = nil
		}
		m.updated = time.Now()
		m.clampAll()
		return m, nil
	case tailMsg:
		if msg.session != m.detail {
			return m, nil
		}
		if msg.err != nil {
			m.tailState, m.tail, m.tailErr = 3, nil, msg.err.Error()
		} else {
			m.tailState, m.tail, m.tailErr = 2, msg.turns, ""
		}
		m.tailOff = 0
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// key routes a key press. With the detail pane open j/k scroll its
// transcript and enter/esc close it; otherwise they move the tab's
// selection, which also drives the body's scroll.
func (m *model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	if m.tab != 1 || m.sel[m.tab] >= len(s) {
		return m, nil
	}
	name := s[m.sel[m.tab]].Name
	m.detail, m.tail, m.tailErr, m.tailOff, m.tailState = name, nil, "", 0, 1
	return m, m.tailCmd(name)
}

// scrollDetail moves the transcript window of an open pane.
func (m *model) scrollDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	turns := len(m.tail)
	switch msg.String() {
	case "j", "down":
		if m.tailOff < maxRow(turns) {
			m.tailOff++
		}
	case "k", "up":
		if m.tailOff > 0 {
			m.tailOff--
		}
	}
	return m, nil
}

// navigate moves between tabs and within the current tab's list.
func (m *model) navigate(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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

// closeDetail dismisses the pane.
func (m *model) closeDetail() {
	m.detail, m.tail, m.tailOff, m.tailState = "", nil, 0, 0
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

// View renders the whole window. Before the first resize it assumes a
// standard terminal so a program can ask for it at any time.
func (m *model) View() string {
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

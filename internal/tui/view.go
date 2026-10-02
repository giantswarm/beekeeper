package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// noneLabel is what a section without entries shows.
const noneLabel = "none"

// The screen's whole palette lives here. lipgloss v1 carries adaptive
// light/dark colours instead of a LightDark helper, so every colour is an
// AdaptiveColor and lipgloss picks the pair the terminal's background
// needs and downgrades it to what the terminal can show. Colour marks
// state — gone, stale, held, breach — never decoration, and its order is
// red > amber > green > dim. Bold is only for the selected row, the
// waiting flag and paging alerts.
var (
	colDim   = lipgloss.AdaptiveColor{Light: "#626262", Dark: "#949494"}
	colWarn  = lipgloss.AdaptiveColor{Light: "#9c6400", Dark: "#d7a54a"}
	colFaint = lipgloss.AdaptiveColor{Light: "#857a55", Dark: "#9a8a5a"}
	colGone  = lipgloss.AdaptiveColor{Light: "#a10000", Dark: "#cd5c5c"}
	colOK    = lipgloss.AdaptiveColor{Light: "#00701f", Dark: "#5faf5f"}
	colRun   = lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafd7"}

	style = struct {
		Title, Tab, TabSel, Head, Dim, Sel, Warn, Faint, Gone, OK, Run, Waiting, Hint lipgloss.Style
	}{
		Title:  lipgloss.NewStyle().Bold(true),
		Tab:    lipgloss.NewStyle().Foreground(colDim),
		TabSel: lipgloss.NewStyle().Bold(true).Reverse(true),
		Head:   lipgloss.NewStyle().Bold(true),
		Dim:    lipgloss.NewStyle().Foreground(colDim),
		Sel:    lipgloss.NewStyle().Reverse(true),
		Warn:   lipgloss.NewStyle().Foreground(colWarn),
		// Faint is a paler amber: severities that warn but do not
		// need the eye yet.
		Faint: lipgloss.NewStyle().Foreground(colFaint),
		Gone:  lipgloss.NewStyle().Foreground(colGone),
		OK:    lipgloss.NewStyle().Foreground(colOK),
		Run:   lipgloss.NewStyle().Foreground(colRun),
		// Waiting is the one thing on the screen that always shouts:
		// a session parked on its person.
		Waiting: lipgloss.NewStyle().Bold(true).Foreground(colWarn),
		Hint:    lipgloss.NewStyle().Foreground(colDim),
	}
)

// tabShort names the tabs when the window is too narrow for the full
// names; the digits stay while they fit, because they are the keys.
var tabShort = [...]string{"watch", "sess", "share", "supvise", tabAlerts, "events"}

// render assembles the window: two header lines, the scrolling body and
// a footer, each cut to the terminal's width so nothing wraps.
func render(m *model, w, h int) string {
	body := bodyView(m, w, max(1, h-3))
	return strings.Join([]string{
		fit(titleView(m.tab, w), w),
		fit(statusView(m.data), w),
		body,
		footerView(m, w),
	}, "\n")
}

// bodyView picks the current tab's view, the open session's detail pane
// or the placeholder before the first refresh.
func bodyView(m *model, w, h int) string {
	if m.data == nil {
		return style.Dim.Render("loading…")
	}
	d := m.data
	if m.detail != "" {
		if s := sessionNamed(d.Sessions, m.detail); s != nil {
			return detailView(d, *s, w, h, m.pane())
		}
	}
	switch m.tab {
	case 0:
		return watchingView(d, w, h, m.sel[0])
	case 1:
		return sessionsView(d, w, h, m.sel[1])
	case 2:
		return sharingView(d, w, h, m.sel[2])
	case 3:
		return supervisingView(d, w, h, m.sel[3])
	case 4:
		return alertsView(d, w, h, m.sel[4])
	default:
		return eventsView(d, w, h, m.sel[5])
	}
}

// titleView is the first header line: the name and the tab bar. The
// active tab is a reversed block; the rest sit dim in brackets. At
// narrow widths the names abbreviate, then the brand and the digits
// drop away — in that order — before anything would wrap.
func titleView(tab int, w int) string {
	build := func(names []string, brand, digits bool) string {
		var parts []string
		if brand {
			parts = append(parts, style.Title.Render("beekeeper"), " ")
		}
		for i := range tabs {
			cell := " "
			if digits {
				cell += fmt.Sprintf("%d ", i+1)
			}
			cell += names[i] + " "
			if i == tab {
				parts = append(parts, style.TabSel.Render(cell))
				continue
			}
			parts = append(parts, style.Tab.Render("["+cell+"]"))
		}
		return strings.Join(parts, "")
	}
	for _, stage := range []struct {
		names  []string
		brand  bool
		digits bool
	}{
		{tabs[:], true, true},
		{tabShort[:], true, true},
		{tabShort[:], false, true},
		{tabShort[:], false, false},
	} {
		if line := build(stage.names, stage.brand, stage.digits); ansiWidth(line) <= w {
			return line
		}
	}
	return build(tabShort[:], false, false)
}

// statusView is the second header line: the supervisor as a status dot
// and name, then the pending counts as compact chips.
func statusView(d *Data) string {
	if d == nil {
		return style.Dim.Render("…")
	}
	st := d.Status
	name, cell := "no supervisor", style.Dim
	if st.Supervisor != "" {
		name, cell = st.Supervisor, style.OK
		switch {
		case st.SupervisorGone:
			name += " gone"
			cell = style.Gone
		case !st.SupervisorLive:
			name += " not running"
			cell = style.Warn
		}
	}
	due := fmt.Sprintf("%d due", st.Due)
	if st.Due > 0 {
		due = style.Warn.Render(due)
	} else {
		due = style.Dim.Render(due)
	}
	return cell.Render("* "+name) +
		style.Dim.Render(" · ") +
		strings.Join([]string{
			chip(st.LeaseCount, "lease", "leases"),
			chip(st.HoldCount, "hold", "holds"),
			due,
		}, style.Dim.Render(" · "))
}

// chip counts n things with the right gram and a dim noun.
func chip(n int, one, many string) string {
	word := many
	if n == 1 {
		word = one
	}
	return fmt.Sprintf("%d ", n) + style.Dim.Render(word)
}

// footerView is the last line: the age of the data, then the last
// refresh error when there is one, then the key hints. Hints are added
// whole in priority order, so a narrow window loses the last of them
// and never the age, the error or the keys a person needs first.
func footerView(m *model, w int) string {
	when := "never"
	if !m.updated.IsZero() {
		when = "updated " + dur(time.Since(m.updated)) + " ago"
	}
	line := style.Hint.Render(when)
	if err := m.err; err != nil {
		line += "  " + style.Warn.Render(fit("err: "+err.Error(), max(10, w-ansiWidth(line)-2)))
	} else if d := m.data; d != nil && len(d.Errors) > 0 {
		more := ""
		if len(d.Errors) > 1 {
			more = fmt.Sprintf(" (+%d)", len(d.Errors)-1)
		}
		line += "  " + style.Warn.Render(fit("err: "+d.Errors[0], max(10, w-ansiWidth(line)-2-len(more)))+more)
	}
	hints := []string{"1-6 tabs", "j/k move", "enter details", "r refresh", "q quit",
		"g/G ends", "pgup/pgdn page"}
	if m.detail != "" {
		hints = []string{"m message", "t take over", "k/j older/newer", "G live", "esc close", "q quit", "g oldest"}
		if s := m.paneSession(); s != nil && s.TakenOver {
			hints[1] = "t hand back"
			if len(s.Approvals) > 0 {
				hints = append([]string{"a allow", "d deny"}, hints...)
			}
		}
		if m.composing {
			hints = []string{"enter send", "esc drop", "ctrl+u clear"}
		}
	}
	used := ansiWidth(line)
	for _, hint := range hints {
		if used+2+ansiWidth(hint) > w {
			break
		}
		line += style.Hint.Render("  " + hint)
		used += 2 + ansiWidth(hint)
	}
	return line
}

// tabLines collects one tab's lines and windows them into the body's
// height, keeping the line the selection sits on visible. Head lines are
// not selectable; row lines are numbered by their place in the tab's
// primary list.
type tabLines struct {
	all     []string
	width   int
	sel     int
	selLine int
	rows    int
}

// newTabLines starts the lines of a tab shown in w columns with sel
// selected.
func newTabLines(w, sel int) *tabLines {
	return &tabLines{all: []string{}, width: w, sel: sel, selLine: -1}
}

// head adds a non-selectable line.
func (t *tabLines) head(s string) { t.all = append(t.all, fit(s, t.width)) }

// dim adds a quiet non-selectable line.
func (t *tabLines) dim(s string) { t.all = append(t.all, fit(style.Dim.Render(s), t.width)) }

// rule adds a section title: a dim rule with the title set into it
// ("── waits · none ────"), so an empty section is one line and not two.
// The title is plain weight — bold belongs to selection and alerts.
func (t *tabLines) rule(title, badge string) {
	label := "── " + title
	if badge != "" {
		label += " " + style.Dim.Render("· "+badge)
	}
	label += " "
	if fill := t.width - ansiWidth(label); fill >= 0 {
		t.all = append(t.all, label+style.Dim.Render(strings.Repeat("─", fill)))
		return
	}
	t.head(fit(strings.TrimRight(label, " "), t.width))
}

// row adds data row i of the tab's primary list. The selected row gets
// a plain > in its gutter — a mark that survives even a terminal
// without reverse video — and is reversed as far as the palette allows.
func (t *tabLines) row(i int, line string) {
	t.rows = i + 1
	if i == t.sel {
		t.selLine = len(t.all)
		t.all = append(t.all, style.Sel.Render(pad("> "+fit(line, t.width-2), t.width)))
		return
	}
	t.all = append(t.all, fit("  "+line, t.width))
}

// show returns the window of at most h lines.
func (t *tabLines) show(h int) string { return t.window(t.all, h) }

// window returns the h lines of all centred on the selected line. With
// the last row selected the window shows the end of the list, so the
// sections after the list are readable; before the first row it shows
// the beginning.
func (t *tabLines) window(all []string, h int) string {
	if len(all) <= h {
		return strings.Join(all, "\n")
	}
	start := t.selLine - h/2
	switch {
	case t.selLine < 0:
	case t.selLine == 0 || t.sel > t.rows-1:
		start = 0
	case t.sel >= t.rows-1:
		start = len(all) - h
	}
	if start > len(all)-h {
		start = len(all) - h
	}
	if start < 0 {
		start = 0
	}
	return strings.Join(all[start:start+h], "\n")
}

// barColor draws an n-cell bar filled to frac - the share the line's
// number names, so the fill and the figure always agree - coloured by
// how much the fill should worry the person. good says whether a full
// bar is a good thing: free room and free slots turn amber when they
// run low; used memory, swap and disk go amber under pressure and red
// near the breach.
func barColor(frac float64, n int, good bool) string {
	switch {
	case good && frac <= 0.10, !good && frac >= 0.90:
		return style.Gone.Render(bar(frac01(frac), n))
	case good && frac <= 0.30, !good && frac >= 0.70:
		return style.Warn.Render(bar(frac01(frac), n))
	default:
		return style.OK.Render(bar(frac01(frac), n))
	}
}

// frac01 clamps a share to 0..1.
func frac01(f float64) float64 { return min(1, max(0, f)) }

// share is part over whole, 0 when whole is 0.
func share(part, whole int) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// watchingView is the machine tab: memory and pressure as bars, disks,
// build slots and clusters, then the waits sessions sit on (the
// selectable rows) and the OOM kills.
func watchingView(d *Data, w, h, sel int) string {
	m := d.Machine
	t := newTabLines(w, sel)
	t.rule("memory", "")
	t.head(cols(
		style.Dim.Render(pad("mem", 6)),
		barColor(share(m.Mem.AvailableMiB, m.Mem.TotalMiB), 10, true),
		rpad(mib(m.Mem.AvailableMiB)+" free of "+mib(m.Mem.TotalMiB), 24),
		style.Dim.Render(" swap"),
		barColor(share(m.Mem.SwapUsedMiB, m.Mem.SwapTotalMiB), 6, false),
		mib(m.Mem.SwapUsedMiB)+"/"+mib(m.Mem.SwapTotalMiB),
	))
	t.dim(cols(
		style.Dim.Render(pad("load", 6)),
		fmt.Sprintf("%.2f %.2f %.2f", m.Load[0], m.Load[1], m.Load[2]),
		style.Dim.Render("psi"), fmt.Sprintf("%.1f%%", m.PSIFull60),
		style.Dim.Render("shmem"), mib(m.Mem.ShmemMiB),
		style.Dim.Render("cli"), mib(m.CLIMemMiB),
	))
	if s := m.Scope; s != nil {
		t.head(cols(
			style.Dim.Render(pad("scope", 6)),
			barColor(share(s.CurrentMiB, miB(s.High)), 10, false),
			rpad(mib(s.CurrentMiB)+" of "+limitGiB(s.High), 24),
			style.Dim.Render("max "+limitGiB(s.Max)),
			style.Dim.Render("swapmax "+limitGiB(s.SwapMax)),
		))
		t.dim(cols(style.Dim.Render(pad("", 6)),
			style.Dim.Render("anon"), mib(s.AnonMiB),
			style.Dim.Render("swap"), mib(s.SwapMiB),
			style.Dim.Render("oomkills"), fmt.Sprintf("%d", s.OOMKills),
			style.Dim.Render("high events"), fmt.Sprintf("%d", s.HighEvents),
		))
	} else {
		t.dim(cols(style.Dim.Render(pad("scope", 6)), "no Claude Desktop scope"))
	}
	t.dim("")
	t.rule("disks", "")
	t.head(diskRow(m.Tmp))
	t.head(diskRow(m.Root))
	t.dim("")
	free := 0
	for _, s := range m.Slots {
		if s.Free {
			free++
		}
	}
	badge := noneLabel
	if len(m.Slots) > 0 {
		badge = fmt.Sprintf("%d/%d free", free, len(m.Slots))
	}
	t.rule("slots", badge)
	if len(m.Slots) > 0 {
		var busy []string
		for _, s := range m.Slots {
			if !s.Free && len(busy) < 3 {
				busy = append(busy, fmt.Sprintf("%d:%s", s.N, slotHolder(s.Holder)))
			}
		}
		if n := len(m.Slots) - free; n > 3 {
			busy = append(busy, fmt.Sprintf("+%d", n-3))
		}
		t.head(cols(
			style.Dim.Render(pad("slots", 6)),
			barColor(share(free, len(m.Slots)), 10, true),
			badge,
			style.Dim.Render(strings.Join(busy, " ")),
		))
	}
	t.dim("")
	switch {
	case m.ClustersErr != "":
		t.rule("clusters", "")
		t.head(style.Warn.Render("cannot ask docker: " + m.ClustersErr))
	case len(m.Clusters) == 0:
		t.rule("clusters", noneLabel)
	default:
		t.rule("clusters", fmt.Sprintf("%d", len(m.Clusters)))
		for _, c := range m.Clusters {
			node := "nodes"
			if c.Nodes == 1 {
				node = "node"
			}
			t.dim(cols(fit(c.Name, 20),
				style.Dim.Render(rpad(fmt.Sprintf("%d %s", c.Nodes, node), 7)),
				rpad(mib(c.MemMiB), 9),
				style.Dim.Render(c.RunningFor)))
		}
	}
	t.dim("")
	badge = fmt.Sprintf("%d", len(m.Waits))
	if len(m.Waits) == 0 {
		badge = noneLabel
	}
	t.rule("waits", badge)
	for i, wt := range m.Waits {
		t.row(i, cols(style.Dim.Render(rpad(fmt.Sprintf("%d", wt.PID), 6)), fit(wt.Session, 20),
			rpad(dur(wt.Elapsed), 6), fit(wt.Args, max(4, w-40))))
	}
	badge = fmt.Sprintf("%d", len(m.OOM))
	if len(m.OOM) == 0 {
		badge = noneLabel
	}
	t.rule("oom kills", badge)
	for _, o := range m.OOM {
		t.head(style.Gone.Render(cols(clock(o.At, d.At),
			style.Dim.Render(fmt.Sprintf("pid %d", o.PID)),
			fit(o.Task, 18), mib(o.AnonMiB), fit(o.Constraint, 14),
			fit(o.Memcg, 18), fit(o.Owner, 12))))
	}
	for _, l := range m.Oomd {
		t.dim(l)
	}
	return t.show(h)
}

// diskRow is one disk line: where, a usage bar, the share used and how
// much room is left.
func diskRow(dk Disk) string {
	used := share(dk.UsedMiB, dk.UsedMiB+dk.FreeMiB)
	return cols(
		style.Dim.Render(pad(fitLeft(dk.Path, 6), 6)),
		barColor(used, 10, false),
		rpad(pct(used), 4),
		rpad(mib(dk.FreeMiB)+" free", 15),
		style.Dim.Render(mib(dk.UsedMiB)+" used"),
	)
}

// miB reads a cgroup limit given in whole MiB; "max" and anything
// unparsable count as no limit (0).
func miB(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// slotHolder names a build slot's holder legibly. The collector hands
// over memcap's raw holder record (a JSON line); the screen shows the
// command being built, falling back to the session or the pid.
func slotHolder(raw string) string {
	var rec struct {
		PID     int    `json:"pid"`
		Session string `json:"session"`
		Cmd     string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return trimWord(raw, 14)
	}
	switch {
	case rec.Cmd != "":
		return trimWord(rec.Cmd, 14)
	case rec.Session != "":
		return trimWord(rec.Session, 14)
	default:
		return fmt.Sprintf("pid %d", rec.PID)
	}
}

// tableCols is the sessions table's column layout: the optional columns
// drop out of a too-narrow window, the least useful first.
type tableCols struct {
	role, state, idle, mem, ctx, hour int
}

func sessionCols(w int) tableCols {
	c := tableCols{role: 5, state: 8, idle: 6, mem: 8, ctx: 9, hour: 12}
	for c.rest(w) < 26 {
		switch {
		case c.hour > 0 && w < 96:
			c.hour = 0
		case c.ctx > 0 && w < 84:
			c.ctx = 0
		case c.mem > 0 && w < 64:
			c.mem = 0
		default:
			return c
		}
	}
	return c
}

// rest is what the name and the what columns share after the gutter,
// the live columns and the two-column gaps between all of them.
func (c tableCols) rest(w int) int {
	sum, gaps := 0, 14
	for _, v := range []int{c.role, c.state, c.idle, c.mem, c.ctx, c.hour} {
		if v == 0 {
			gaps -= 2
			continue
		}
		sum += v
	}
	return w - 2 - sum - gaps
}

func (c tableCols) nameW(w int) int {
	if a := c.rest(w) - 12; a >= 10 {
		return min(30, a)
	}
	return max(8, c.rest(w))
}

func (c tableCols) whatW(w, nameW int) int { return max(0, c.rest(w)-nameW) }

// sessionsView is the session tab: an aligned table of the sessions,
// then what more than one session is on.
func sessionsView(d *Data, w, h, sel int) string {
	t := newTabLines(w, sel)
	t.rule("sessions", fmt.Sprintf("%d", len(d.Sessions)))
	lh := d.Totals.LastHour
	t.dim(cols(
		style.Dim.Render("last hour"),
		fmt.Sprintf("%dt %de %s", lh.Turns, lh.ToolErrors, cost(lh.Cost)),
		style.Dim.Render("gh procs"), fmt.Sprintf("%d", d.Totals.GitHubProcesses),
	))
	c := sessionCols(w)
	var head []string
	if c.role > 0 {
		head = append(head, pad(style.Dim.Render("role"), c.role))
	}
	if c.state > 0 {
		head = append(head, pad(style.Dim.Render("state"), c.state))
	}
	if c.idle > 0 {
		head = append(head, pad(style.Dim.Render("idle"), c.idle))
	}
	if c.mem > 0 {
		head = append(head, rpad(style.Dim.Render("mem"), c.mem))
	}
	if c.ctx > 0 {
		head = append(head, rpad(style.Dim.Render("ctx"), c.ctx))
	}
	if c.hour > 0 {
		head = append(head, rpad(style.Dim.Render("hour"), c.hour))
	}
	if len(head) > 0 {
		t.dim("  " + pad("name", c.nameW(w)) + "  " + strings.Join(head, "  "))
	}
	for i, s := range d.Sessions {
		line := sessionRow(s, w, d.At)
		if s.Waiting != "" || len(s.Approvals) > 0 {
			line = style.Waiting.Render(line)
		}
		t.row(i, line)
	}
	if len(d.Overlaps) > 0 {
		t.dim("")
		t.rule("overlap", fmt.Sprintf("%d", len(d.Overlaps)))
		for _, o := range d.Overlaps {
			t.dim(cols(style.Dim.Render(o.Kind), o.Key, strings.Join(o.Sessions, ", ")))
		}
	}
	return t.show(h)
}

// sessionRow lays out one session in the table's columns: name, role
// badge, idle age coloured by how stale it is, memory, context fill,
// the last hour's activity (quiet when it is all zero), then what it is
// on. A session waiting on its person is shouted by the caller: the
// whole row goes amber bold and the what column carries the [WARN] flag
// and the waiting text, cut with an ellipsis but never hidden.
func sessionRow(s Session, w int, now time.Time) string {
	c := sessionCols(w)
	nameW := c.nameW(w)
	cells := []string{pad(trimWord(s.Name, nameW), nameW)}
	if c.role > 0 {
		cells = append(cells, pad(roleBadge(s.Role), c.role))
	}
	if c.state > 0 {
		cells = append(cells, pad(stateCell(s), c.state))
	}
	if c.idle > 0 {
		idle := dur(s.Idle)
		switch {
		case s.Idle < 5*time.Minute:
			idle = style.OK.Render(idle)
		case s.Idle > 2*time.Hour:
			idle = style.Dim.Render(idle)
		}
		cells = append(cells, pad(idle, c.idle))
	}
	if c.mem > 0 {
		cells = append(cells, rpad(mib(s.MemMiB), c.mem))
	}
	if c.ctx > 0 {
		ctx := "—"
		if s.ContextFill > 0 || s.Context > 0 {
			ctx = pct(s.ContextFill) + "·" + tokens(s.Context)
			switch {
			case s.ContextFill > 0.9:
				ctx = style.Gone.Render(ctx)
			case s.ContextFill > 0.6:
				ctx = style.Warn.Render(ctx)
			}
		}
		cells = append(cells, rpad(ctx, c.ctx))
	}
	if c.hour > 0 {
		hour := ""
		if s.LastHour.Turns > 0 || s.LastHour.ToolErrors > 0 ||
			(s.LastHour.Cost != nil && *s.LastHour.Cost > 0) {
			hour = fmt.Sprintf("%dt %de %s", s.LastHour.Turns, s.LastHour.ToolErrors, cost(s.LastHour.Cost))
		}
		cells = append(cells, rpad(hour, c.hour))
	}
	if what := c.whatW(w, nameW); what >= 6 {
		var tail []string
		left := what
		room := func(n int) int {
			n = min(n, left)
			left = max(0, left-n-2)
			return n
		}
		waiting := s.Waiting
		if len(s.Approvals) > 0 {
			waiting = "approve? " + s.Approvals[0].Gist
		}
		if waiting != "" && left >= 12 {
			tail = append(tail, style.Waiting.Render("[WARN] ")+
				trimWord(waiting, max(0, left-7)))
		} else if waiting != "" {
			// No room for the text: the flag still says someone
			// is waiting on this one.
			tail = append(tail, style.Waiting.Render("[WARN]"))
		} else {
			if len(s.Leases) > 0 && left >= 8 {
				n := room(left)
				tail = append(tail, style.Run.Render("hold "+trimWord(strings.Join(s.Leases, ","), n-5)))
			}
			if mg := mergeCell(s.Merges); mg != "" && left >= 6 {
				tail = append(tail, fit(mg, room(left)))
			}
			if len(s.Commands) > 0 && left >= 10 {
				more := ""
				if len(s.Commands) > 1 {
					more = fmt.Sprintf(" +%d", len(s.Commands)-1)
				}
				n := room(left)
				tail = append(tail, style.Run.Render(fit(s.Commands[0].Args, max(1, n-ansiWidth(more)))+more))
			}
		}
		cells = append(cells, strings.Join(tail, "  "))
	}
	return strings.Join(cells, "  ")
}

// The roles of the transcript turns the pane shows.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// The states a session row shows.
const (
	stateBusy    = "busy"
	stateIdle    = "idle"
	stateWaiting = "waiting"
	// stateApproval is a taken-over session waiting on the screen's
	// answer; stateTaken one whose approvals come to the screen.
	stateApproval = "approval"
	stateTaken    = "taken"
	stateEnded    = "ended"
)

// busyWithin is how recently a Claude Code session's transcript changed
// for the screen to call it busy.
const busyWithin = time.Minute

// stateOf is what a session does: one taken over waits on the screen's
// answer or is just taken; an omp session says so itself; a
// Claude Code session waits on its person when the desktop says so, is
// busy while it runs a command or wrote its transcript within busyWithin,
// and idle otherwise.
func stateOf(s Session) string {
	switch {
	case len(s.Approvals) > 0:
		return stateApproval
	case s.TakenOver:
		return stateTaken
	case s.State != "":
		return s.State
	case s.Waiting != "":
		return stateWaiting
	case len(s.Commands) > 0 || s.Idle < busyWithin:
		return stateBusy
	}
	return stateIdle
}

// stateCell is a row's state, coloured, with omp's harness ahead of it.
func stateCell(s Session) string {
	st := stateOf(s)
	word := st
	if s.Harness != "" {
		word = s.Harness + "·" + st
	}
	switch st {
	case stateBusy:
		return style.OK.Render(word)
	case stateWaiting, stateApproval:
		return style.Waiting.Render(word)
	case stateTaken:
		return style.Run.Render(word)
	case stateEnded:
		return style.Gone.Render(word)
	}
	return style.Dim.Render(word)
}

// roleBadge is the session's role as a short, legible badge.
func roleBadge(role string) string {
	switch {
	case role == "supervisor":
		return "sup"
	case role == "guide":
		return "guide"
	case strings.HasPrefix(role, "agent"):
		return "agent"
	case role == "":
		return "-"
	}
	return fit(role, 5)
}

// mergeCell counts a session's gated merges in a legible line, empty
// when it has none: merge 3q/2m/2f is three queued, two merged, two
// failed.
func mergeCell(m Merges) string {
	if m == (Merges{}) {
		return ""
	}
	var parts []string
	if m.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%dq", m.Queued))
	}
	if m.Merged > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m.Merged))
	}
	if m.Refused > 0 {
		parts = append(parts, style.Gone.Render(fmt.Sprintf("%dr", m.Refused)))
	}
	if m.Failed > 0 {
		parts = append(parts, style.Gone.Render(fmt.Sprintf("%df", m.Failed)))
	}
	return style.Dim.Render("merge ") + strings.Join(parts, "/")
}

// detailView is a session's pane: its facts in two aligned columns
// under section rules, absolute time with the age beside it, and the
// transcript in the rest of the height, newest at the bottom: the
// history, then the current turn as it runs, and under it the message
// line while the person writes, sends or has sent one.
func detailView(d *Data, s Session, w, h int, p pane) string {
	tail, state, back, err := p.tail, p.state, p.back, p.err
	t := newTabLines(w, -1)
	t.rule(style.Head.Render(fit(s.Name, w-30)), "esc close  k/j scroll")
	lab := func(l string) string { return style.Dim.Render(pad(l, 9)) }
	t.head(cols(lab("pid"), fmt.Sprintf("%d", s.PID),
		pad(fit(roleBadge(s.Role), 5), 5), stateCell(s), fit(s.Model, 18), fit(s.Permission, 12)))
	t.dim(cols(lab("role"), plainOrDim(s.Role)))
	t.dim(cols(lab("cwd"), fitLeft(s.Cwd, w-14)))
	t.dim(cols(lab("repo"), fitLeft(s.Repo, 30), fit(s.Branch, 20),
		style.Dim.Render("serves"), plainOrDim(s.Serves)))
	t.dim(cols(lab("started"), stamp(s.Started), style.Dim.Render("("+age(s.Started, d.At)+")"),
		style.Dim.Render("active"), stamp(s.LastActive), style.Dim.Render("("+age(s.LastActive, d.At)+")"),
		style.Dim.Render("idle"), dur(s.Idle)))
	t.rule("context", "")
	ctx := "unknown"
	if s.ContextWindow > 0 {
		ctx = tokens(s.Context) + "/" + tokens(int64(s.ContextWindow)) + "  " + pct(s.ContextFill)
	}
	t.dim(cols(lab("used"), ctx))
	t.dim(cols(lab("activity"),
		fmt.Sprintf("%d turns", s.Total.Turns),
		fmt.Sprintf("%d/%d tools ok", s.Total.ToolCalls-s.Total.ToolErrors, s.Total.ToolCalls),
		cost(s.Total.Cost)))
	if mg := mergeCell(s.Merges); mg != "" {
		t.dim(cols(lab("merges"), mg, style.Dim.Render(fmt.Sprintf("%d gh procs", s.GitHubProcesses))))
	}
	t.dim("")
	n := len(s.Work) + len(s.Leases) + len(s.Commands) + len(s.Scopes)
	badge := noneLabel
	if n > 0 {
		badge = fmt.Sprintf("%d", n)
	}
	t.rule("on it now", badge)
	if len(s.Work) > 0 {
		t.dim(cols(lab("refs"), fit(strings.Join(s.Work, ", "), w-14)))
	}
	if len(s.Leases) > 0 {
		t.dim(cols(lab("leases"), style.Run.Render(strings.Join(s.Leases, ", "))))
	}
	if s.Waits != "" {
		t.dim(cols(lab("waits"), fit(s.Waits, w-14)))
	}
	if s.Waiting != "" {
		t.head(style.Waiting.Render("[WARN] waiting on person: " + trimWord(s.Waiting, w-30)))
	}
	for _, c := range s.Commands {
		cell := cols(style.Dim.Render("run"), fmt.Sprintf("%d", c.PID),
			fit(c.Args, max(4, w-44)), rpad(dur(c.Elapsed), 6))
		if c.Remaining > 0 {
			cell = cols(cell, style.Dim.Render("left "+dur(c.Remaining)))
		}
		t.dim(cell)
	}
	for _, sc := range s.Scopes {
		t.dim(cols(style.Dim.Render("run"), fit(sc.Unit, 30), fit(sc.Command, 30), mib(sc.MemMiB)))
	}
	if s.TakenOver || len(s.Approvals) > 0 {
		approvalLines(t, s, w, d.At)
	}
	t.dim("")
	badge = "…"
	switch {
	case state == 2 && back > 0:
		badge = fmt.Sprintf("%d back · G live", back)
	case state == 2:
		badge = style.OK.Render("live")
	case state == 3:
		badge = "failed"
	}
	t.rule("transcript", badge)
	msg := messageLine(p, w)
	switch state {
	case 1:
		t.dim("loading…")
	case 3:
		t.head(style.Warn.Render("tail: " + err))
	case 2:
		if err != "" {
			t.head(style.Warn.Render("tail: " + trimWord(err, w-8)))
		}
		if len(tail) == 0 {
			t.dim("empty")
		}
		room := h - len(t.all)
		if msg != "" {
			room--
		}
		t.all = append(t.all, turnLines(tail[:len(tail)-min(back, len(tail))], w, room, d.At)...)
	}
	if msg != "" {
		// The message line stays in sight: the pane's last line, even
		// when the facts above fill the window.
		lines := t.all[:min(len(t.all), max(0, h-1))]
		return strings.Join(append(lines, msg), "\n")
	}
	return t.show(h)
}

// approvalLines is the take-over section: that the session's approvals
// come to this screen, and the oldest one it waits on with its input, the
// call the person answers with a or d; any later ones as one line each.
func approvalLines(t *tabLines, s Session, w int, now time.Time) {
	badge := "approvals come here"
	if n := len(s.Approvals); n > 0 {
		badge = style.Waiting.Render(fmt.Sprintf("%d waiting · a allow  d deny", n))
	}
	t.rule("taken over", badge)
	for i, ap := range s.Approvals {
		if i > 0 {
			t.dim(cols(clock(ap.At, now), pad("then", 10), trimWord(ap.Gist, w-24)))
			continue
		}
		t.head(style.Waiting.Render(cols(clock(ap.At, now), pad("approve?", 10), trimWord(ap.Gist, w-24))))
		for _, l := range ap.Detail[:min(len(ap.Detail), 6)] {
			t.dim(pad("", 20) + trimWord(l, w-24))
		}
	}
}

// messageLine is the pane's last line: the draft with its cursor while
// the person writes, the delivery while it runs, then its outcome; ""
// when there is none of these.
func messageLine(p pane, w int) string {
	switch {
	case p.composing:
		draft := fitLeft(p.draft, max(1, w-12))
		return fit(style.Head.Render("message › ")+draft+"█", w)
	case p.sending:
		return fit(style.Dim.Render("message › sending…"), w)
	case p.acting:
		return fit(style.Dim.Render("…"), w)
	case p.outcome != "" && p.outcomeErr:
		return fit(style.Warn.Render(trimWord(p.outcome, w)), w)
	case p.outcome != "":
		return fit(style.OK.Render(trimWord(p.outcome, w)), w)
	}
	return ""
}

// turnLines lays out turns in at most room lines, the newest last: each
// turn's first line with its time and role, up to three more of its own
// lines under it, and a tool call as one quiet line. The oldest turns
// give way when the window is short.
func turnLines(turns []Turn, w, room int, now time.Time) []string {
	var out []string
	for i := len(turns) - 1; i >= 0 && len(out) < room; i-- {
		tu := turns[i]
		role := style.Dim.Render(pad(tu.Role, 10))
		switch tu.Role {
		case roleAssistant:
			role = style.OK.Render(pad(tu.Role, 10))
		case roleTool:
			role = style.Run.Render(pad(tu.Role, 10))
		}
		// A turn's text can carry its own newlines (a pasted block, a
		// tool payload); each physical line goes out as its own fitted
		// line so nothing ever wraps.
		lines := strings.Split(tu.Text, "\n")
		block := []string{fit(style.Dim.Render(cols(clock(tu.At, now), role, trimWord(lines[0], w-24))), w)}
		for _, extra := range lines[1:min(len(lines), 4)] {
			block = append(block, fit(style.Dim.Render(pad("", 20)+trimWord(extra, w-24)), w))
		}
		out = append(block, out...)
	}
	return out[max(0, len(out)-max(room, 0)):]
}

// sharingView is the sharing tab: held leases (the selectable rows),
// free resources, the grant queues, holds and the merge lanes.
func sharingView(d *Data, w, h, sel int) string {
	t := newTabLines(w, sel)
	badge := noneLabel
	if len(d.Leases.Held) > 0 {
		badge = fmt.Sprintf("%d", len(d.Leases.Held))
	}
	t.rule("leases held", badge)
	for i, l := range d.Leases.Held {
		dot, cell := "*", style.OK
		switch {
		case l.State == "person":
			dot, cell = "*", style.Run
		case l.State != "live":
			dot, cell = "*", style.Warn
		}
		cells := []string{
			cell.Render(dot),
			pad(fit(l.Resource, 24), 24),
			pad(fit(l.Holder, 16), 16),
			rpad(age(l.Since, d.At), 7),
			trimWord(l.Purpose, max(4, w-78)),
		}
		if l.UpgradeUnblock != "" {
			cells = append(cells, style.Run.Render("unblocks "+fit(l.UpgradeUnblock, 16)))
		}
		t.row(i, strings.Join(cells, "  "))
	}
	badge = noneLabel
	if len(d.Leases.Free) > 0 {
		badge = fmt.Sprintf("%d", len(d.Leases.Free))
	}
	t.rule("free", badge)
	if len(d.Leases.Free) > 0 {
		t.dim(fit(strings.Join(d.Leases.Free, ", "), w-2))
	}
	if len(d.Leases.Queues) > 0 {
		t.rule("grants waiting", fmt.Sprintf("%d", len(d.Leases.Queues)))
		for _, res := range sortedKeys(d.Leases.Queues) {
			var gs []string
			for _, g := range d.Leases.Queues[res] {
				gs = append(gs, style.Warn.Render("<- "+fit(g.To, 16))+
					style.Dim.Render(" by "+fit(g.By, 12)+" · "+age(g.At, d.At)))
			}
			t.dim(cols(fit(res, 24), strings.Join(gs, "  ")))
		}
	}
	badge = noneLabel
	if len(d.Holds) > 0 {
		badge = fmt.Sprintf("%d", len(d.Holds))
	}
	t.rule("holds", badge)
	for _, hd := range d.Holds {
		until, cell := "lifted by hand", style.Warn
		if !hd.Until.IsZero() {
			if hd.Until.Before(d.At) {
				until, cell = "expired "+clock(hd.Until, d.At), style.Gone
			} else {
				until = "until " + clock(hd.Until, d.At) + " (" + dur(hd.Until.Sub(d.At)) + ")"
			}
		}
		cells := []string{
			cell.Render("(!)"),
			pad(fit(hd.Target, 22), 22),
			trimWord(hd.Reason, 28),
			style.Dim.Render("by " + fit(hd.By, 12)),
			until,
		}
		if hd.Except != "" {
			cells = append(cells, style.Dim.Render("except "+fit(hd.Except, 16)))
		}
		if hd.Tool != "" {
			cells = append(cells, style.Dim.Render("window "+hd.Tool+" "+hd.ToolFrom+" -> "+hd.ToolRelease))
		}
		t.head(cell.Render(strings.Join(cells, "  ")))
	}
	badge = noneLabel
	if len(d.Lanes) > 0 {
		badge = fmt.Sprintf("%d", len(d.Lanes))
	}
	t.rule("lanes", badge)
	for _, ln := range d.Lanes {
		head := style.Head.Render(ln.Name)
		if ln.Installation != "" {
			head = cols(head, style.Dim.Render("("+ln.Installation+")"))
		}
		if ln.Hold != "" {
			head = cols(head, style.Warn.Render("hold: "+trimWord(ln.Hold, 28)))
		}
		t.head(head)
		if r := ln.Running; r != nil {
			t.dim(cols(style.Dim.Render("running"), fit(r.Key, 26),
				style.Dim.Render("by "+fit(r.By, 12)), rpad(durAge(r.Started, r.Finished, d.At), 6)))
		}
		if len(ln.Settling) > 0 {
			t.dim(cols(style.Dim.Render("settling"), fmt.Sprintf("%d", len(ln.Settling))))
		}
		if len(ln.Waiting) > 0 {
			t.dim(cols(style.Dim.Render("waiting"), fmt.Sprintf("%d", len(ln.Waiting)),
				fit(mergeKeys(ln.Waiting), max(4, w-40))))
		}
		if ln.Stall != "" {
			t.head(style.Warn.Render("! stalled: " + trimWord(ln.Stall, w-14)))
		}
	}
	return t.show(h)
}

// supervisingView is the supervision tab: the supervisor's and the
// guide's records (the selectable rows), the agents, the notes, the
// timers and which session serves which issue.
func supervisingView(d *Data, w, h, sel int) string {
	t := newTabLines(w, sel)
	badge := noneLabel
	if len(d.Roles) > 0 {
		badge = fmt.Sprintf("%d", len(d.Roles))
	}
	t.rule("roles", badge)
	for i, r := range d.Roles {
		dot, state, cell := "*", "live", style.OK
		switch {
		case !r.Gone.IsZero():
			dot, state, cell = "*", "gone "+age(r.Gone, d.At), style.Gone
		case !r.RestartUntil.IsZero():
			dot, state, cell = "*", "restarting until "+clock(r.RestartUntil, d.At), style.Warn
		case !r.Live:
			dot, state, cell = "o", "not running", style.Dim
		}
		cells := []string{
			cell.Render(dot),
			pad(fit(r.Name, 10), 10),
			pad(fit(r.Holder, 16), 16),
			cell.Render(state),
			style.Dim.Render("since " + stamp(r.Since)),
			style.Dim.Render("ctx"), tokens(r.Context),
		}
		if r.RelayAt > 0 {
			cells = append(cells, style.Dim.Render("relay at"), tokens(r.RelayAt))
		}
		if r.Relay != "" {
			cells = append(cells, style.Run.Render("relay: "+fit(r.Relay, 26)))
		}
		t.row(i, strings.Join(cells, "  "))
	}
	badge = noneLabel
	if len(d.Agents) > 0 {
		badge = fmt.Sprintf("%d", len(d.Agents))
	}
	t.rule("agents", badge)
	for _, a := range d.Agents {
		reach := a.Reachable
		switch {
		case strings.HasPrefix(a.Reachable, "not"):
			reach = style.Gone.Render(a.Reachable)
		case !strings.HasPrefix(a.Reachable, "live"):
			reach = style.Warn.Render(a.Reachable)
		}
		t.dim(cols(pad(fit(a.Name, 14), 14), pad(reach, 18), trimWord(a.Task, 34),
			style.Dim.Render("assigned "+clock(a.AssignedAt, d.At))))
	}
	badge = noneLabel
	if len(d.Notes) > 0 {
		badge = fmt.Sprintf("%d", len(d.Notes))
	}
	t.rule("notes", badge)
	for _, n := range d.Notes {
		due := ""
		if !n.Due.IsZero() {
			if n.Due.Before(d.At) {
				due = style.Warn.Render("overdue " + dur(d.At.Sub(n.Due)))
			} else {
				due = "due " + clock(n.Due, d.At)
			}
		}
		cell := cols(style.Dim.Render(fmt.Sprintf("#%d", n.ID)), pad(fit(n.For, 10), 10), due,
			trimWord(n.Text, max(8, w-56)))
		if n.Default != "" {
			cell = cols(cell, style.Dim.Render("(default: "+fit(n.Default, 24)+")"))
		}
		t.head(cell)
	}
	badge = noneLabel
	if len(d.Timers) > 0 {
		badge = fmt.Sprintf("%d", len(d.Timers))
	}
	t.rule("timers", badge)
	for _, tm := range d.Timers {
		cell := cols(style.Dim.Render(fmt.Sprintf("#%d", tm.ID)), rpad(clock(tm.Due, d.At), 13),
			trimWord(tm.What, max(8, w-56)), style.Dim.Render("by "+fit(tm.By, 12)))
		switch {
		case tm.Fired:
			cell = style.Dim.Render(cell)
		case tm.Due.Before(d.At):
			cell = style.Warn.Render("! " + cell)
		}
		t.head(cell)
	}
	badge = noneLabel
	if len(d.Records) > 0 {
		badge = fmt.Sprintf("%d", len(d.Records))
	}
	t.rule("records", badge)
	for _, r := range d.Records {
		cell := cols(pad(fit(r.Session, 14), 14), fit(r.Issue, 34))
		if r.Waits != "" {
			cell = cols(cell, style.Warn.Render("waits: "+trimWord(r.Waits, 24)))
		}
		t.dim(cell)
	}
	return t.show(h)
}

// alertsView is the alert tab: each installation (the selectable row)
// with its firing alerts deduplicated underneath, then the running
// upgrades and the GitHub budget.
func alertsView(d *Data, w, h, sel int) string {
	t := newTabLines(w, sel)
	badge := noneLabel
	if len(d.Alerts) > 0 {
		badge = fmt.Sprintf("%d", len(d.Alerts))
	}
	t.rule("installations", badge)
	for i, a := range d.Alerts {
		rows := dedupAlerts(a.Alerts)
		cell := fit(a.Installation, 24)
		if !a.Reachable {
			cell = cols(cell, style.Gone.Render("unreachable"))
		} else {
			noun := tabAlerts
			if len(rows) == 1 {
				noun = "alert"
			}
			cell = cols(cell, style.Dim.Render(fmt.Sprintf("%d %s", len(rows), noun)), sevChips(rows))
		}
		t.row(i, cell)
		// The alert lines sit dim under the installation, aligned in
		// the alertname column: the row above is what a person selects.
		nameW := min(34, max(14, w-56))
		whereW := max(4, w-108)
		for _, al := range rows {
			count := ""
			if al.n > 1 {
				count = style.Faint.Render(fmt.Sprintf("×%d", al.n))
			}
			t.dim("    " + cols(pad(fit(al.Alertname, nameW), nameW),
				pad(fit(al.Cluster, 14), 14),
				pad(fit(al.Team, 10), 10),
				fit(al.Where, whereW),
				style.Dim.Render("since "+sinceLabel(al.Since, d.At)),
				count))
		}
	}
	badge = noneLabel
	if len(d.Upgrades) > 0 {
		badge = fmt.Sprintf("%d", len(d.Upgrades))
	}
	t.dim("")
	t.rule("upgrades", badge)
	for _, u := range d.Upgrades {
		t.head(style.Run.Render(fit(u, w-2)))
	}
	t.dim("")
	t.rule("github budget", "")
	b := d.Budget
	if b.At.IsZero() {
		t.dim("no reading yet")
	} else {
		line := cols(
			barColor(share(b.Remaining, b.Limit), 10, true),
			rpad(fmt.Sprintf("%d/%d", b.Remaining, b.Limit), 13),
			style.Dim.Render("reset"), clock(b.Reset, d.At),
			style.Dim.Render("floor"), fmt.Sprintf("%d", b.Floor),
		)
		if b.Floor > 0 && b.Remaining < b.Floor {
			line = style.Gone.Render(line + "  under floor")
		}
		if b.Held {
			line = cols(line, style.Warn.Render("held: "+fit(b.HoldReason, 30)))
		}
		t.head(line)
	}
	if b.Err != "" {
		t.head(style.Warn.Render("probe: " + b.Err))
	}
	for _, p := range b.Pollers {
		t.dim(cols(style.Dim.Render("poller"), rpad(fmt.Sprintf("%d", p.PID), 5),
			pad(fit(p.Session, 14), 14), rpad(dur(p.Elapsed), 6), fit(p.Args, max(4, w-44))))
	}
	return t.show(h)
}

// alertRow is one deduplicated alert: a row of the screen, counting how
// many identical firings it stands for.
type alertRow struct {
	Alert
	n int
}

// dedupAlerts collapses alerts that are the same firing — name,
// cluster, place and severity — into one row keeping the earliest
// Since, and orders them page first. Deduping lives in the view: the
// collector still reports every firing it was given.
func dedupAlerts(as []Alert) []alertRow {
	idx := map[string]int{}
	var out []alertRow
	for _, a := range as {
		key := strings.ToLower(a.Severity) + "\x00" + a.Alertname + "\x00" + a.Cluster + "\x00" + a.Where
		if i, ok := idx[key]; ok {
			out[i].n++
			if a.Since < out[i].Since {
				out[i].Since = a.Since
			}
			continue
		}
		idx[key] = len(out)
		out = append(out, alertRow{Alert: a, n: 1})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if r := sevRank(out[i].Severity) - sevRank(out[j].Severity); r != 0 {
			return r < 0
		}
		if out[i].Alertname != out[j].Alertname {
			return out[i].Alertname < out[j].Alertname
		}
		return out[i].Cluster < out[j].Cluster
	})
	return out
}

// sevRank orders severities by how much they hurt; unknown ones sit
// with the warnings.
func sevRank(sev string) int {
	switch strings.ToLower(sev) {
	case "critical", "fatal", "page":
		return 0
	case "high", "warning", "warn":
		return 1
	case "medium":
		return 2
	case "low", "info":
		return 3
	}
	return 1
}

// sevChips is the installation row's severity summary: one chip per
// severity present, coloured by how much it hurts. Only a paging
// severity gets the bold.
func sevChips(rows []alertRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Severity]++
	}
	var sevs []string
	for sev := range counts {
		sevs = append(sevs, sev)
	}
	sort.SliceStable(sevs, func(i, j int) bool { return sevRank(sevs[i]) < sevRank(sevs[j]) })
	var chips []string
	for _, sev := range sevs {
		chip := fmt.Sprintf("%s %d", sev, counts[sev])
		switch strings.ToLower(sev) {
		case "page", "fatal":
			chip = style.Head.Render(style.Gone.Render(chip))
		case "critical":
			chip = style.Warn.Render(chip)
		case "high", "warning", "warn":
			chip = style.Faint.Render(chip)
		default:
			chip = style.Dim.Render(chip)
		}
		chips = append(chips, chip)
	}
	return strings.Join(chips, "  ")
}

// eventsView is the event log, newest first, the verbs coloured by what
// they do to the work.
func eventsView(d *Data, w, h, sel int) string {
	evs := make([]Event, len(d.Events))
	copy(evs, d.Events)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.After(evs[j].At) })
	t := newTabLines(w, sel)
	badge := noneLabel
	if len(evs) > 0 {
		badge = fmt.Sprintf("%d", len(evs))
	}
	t.rule("events", badge)
	for i, e := range evs {
		t.row(i, cols(rpad(clock(e.At, d.At), 13), verbStyle(e.Verb).Render(pad(fit(e.Verb, 18), 18)),
			pad(fit(e.By, 16), 16), fit(e.Detail, max(4, w-54))))
	}
	return t.show(h)
}

// verbStyle colours a log verb by what it does to the work: refusals,
// failures and kills shout red; holds and due items warn amber; grants,
// claims and finished notes please green; everything else is quiet
// progress and stays dim.
func verbStyle(verb string) lipgloss.Style {
	l := strings.ToLower(verb)
	switch {
	case strings.Contains(l, "refus"), strings.Contains(l, "fail"),
		strings.Contains(l, "kill"), strings.Contains(l, "revoke"):
		return style.Gone
	case strings.Contains(l, "hold"), strings.Contains(l, "due"):
		return style.Warn
	case strings.Contains(l, "grant"), strings.Contains(l, "claim"),
		strings.Contains(l, "merged"), strings.Contains(l, "done"),
		strings.Contains(l, "answered"), strings.Contains(l, "settle"),
		strings.Contains(l, "clear"):
		return style.OK
	default:
		return style.Dim
	}
}

// cols joins the non-empty cells with two spaces.
func cols(cells ...string) string {
	parts := make([]string, 0, len(cells))
	for _, s := range cells {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "  ")
}

// mergeKeys names the first few merges of a lane's queue. After the
// first key, a merge from the same repo drops the owner: the queue of
// one repo reads #704, #707, #675, +4, not a wall of identical
// prefixes.
func mergeKeys(ms []Merge) string {
	var out []string
	last := ""
	for i, m := range ms {
		if i == 3 {
			out = append(out, fmt.Sprintf("+%d", len(ms)-3))
			break
		}
		key := m.Key
		if i > 0 {
			if repo, num, ok := strings.Cut(m.Key, "#"); ok && repo == last {
				key = "#" + num
			}
		}
		last, _, _ = strings.Cut(m.Key, "#")
		out = append(out, key)
	}
	return strings.Join(out, ", ")
}

// durAge is how long something ran: until it finished, or until now.
func durAge(started, finished, now time.Time) string {
	if started.IsZero() {
		return "-"
	}
	if finished.IsZero() {
		finished = now
	}
	return dur(finished.Sub(started))
}

// sortedKeys orders a resource-keyed map's keys for a deterministic
// render.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sessionNamed finds a session by name.
func sessionNamed(sessions []Session, name string) *Session {
	for i := range sessions {
		if sessions[i].Name == name {
			return &sessions[i]
		}
	}
	return nil
}

// plainOrDim renders empty strings quietly.
func plainOrDim(s string) string {
	if s == "" {
		return style.Dim.Render("nothing")
	}
	return s
}

// pad fills s with spaces to w columns.
func pad(s string, w int) string {
	if n := w - ansiWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// ansiWidth is s's printed width, escape sequences aside.
func ansiWidth(s string) int {
	return ansi.StringWidth(s)
}

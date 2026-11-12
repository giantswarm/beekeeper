package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

// kindBlocks are the kinds of ks at a cap now: maxBusy against the busy
// agents capacity counts, minInterval against the kind's last recorded
// start. A kind at both says both.
func kindBlocks(cfg *config.Config, st *state.State, sessions []*claude.Session, ks []config.AgentKind, now time.Time) []capacity.KindBlock {
	if len(ks) == 0 {
		return nil
	}
	busy := countAgents(st, sessions, now, cfg).Busy
	var out []capacity.KindBlock
	for _, k := range ks {
		var reasons []string
		if k.MaxBusy > 0 {
			var of []string
			for _, n := range busy {
				if k.Matches(n) {
					of = append(of, n)
				}
			}
			if len(of) >= k.MaxBusy {
				reasons = append(reasons, fmt.Sprintf("%d busy (%s) at maxBusy %d", len(of), strings.Join(of, ", "), k.MaxBusy))
			}
		}
		if iv := k.MinInterval.Duration; iv > 0 {
			if last, ok := lastKindStart(st, k); ok && now.Sub(last.At) < iv {
				reasons = append(reasons, fmt.Sprintf("last start %s %s ago, minInterval %s: next at %s",
					last.Name, dur(now.Sub(last.At)), dur(iv), last.At.Add(iv).Local().Format("15:04")))
			}
		}
		if len(reasons) > 0 {
			out = append(out, capacity.KindBlock{Kind: k.Name, Reason: strings.Join(reasons, "; ")})
		}
	}
	return out
}

// lastKindStart is the latest recorded start of one of k's agents.
func lastKindStart(st *state.State, k config.AgentKind) (state.Start, bool) {
	var last state.Start
	found := false
	for _, s := range st.Starts {
		if k.Matches(s.Name) && (!found || s.At.After(last.At)) {
			last, found = s, true
		}
	}
	return last, found
}

// kindGate refuses, exit 3, a start of name while one of its kinds is at a
// cap; --force passes no kind cap, a kind's caps are the desk's batching.
func (a *app) kindGate(name string) error {
	ks := a.cfg.Agents.KindsOf(name)
	if len(ks) == 0 {
		return nil
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil && !platform.Missing(err) {
		return err
	}
	blocks := kindBlocks(a.cfg, st, sessions, ks, a.now)
	if len(blocks) == 0 {
		return nil
	}
	lines := make([]string, len(blocks))
	for i, b := range blocks {
		lines[i] = b.Line()
	}
	return refused("%s: %s", name, strings.Join(lines, "; "))
}

// tzFlag names the time zone of a report's days.
const tzFlag = "--tz"

// kindLockWait bounds the wait for another start of a kind to be recorded.
const kindLockWait = 15 * time.Minute

// kindLockFile serializes the starts of agents of any kind: the gate and
// the start's record are one step, so two concurrent starts never both
// pass a cap.
const kindLockFile = "kinds.lock"

// fatalAdmission stops ordinary work only for execution infrastructure outages.
func (a *app) fatalAdmission(name string, review bool) error {
	q, err := fatal.Read(cmp.Or(a.store.Dir(), a.cfg.StateDir))
	if err != nil {
		return fmt.Errorf("fatal admission: %w", err)
	}
	// Reviews finish already admitted work, including the outage's own fix.
	if err := q.Barrier(name); err != nil && !review {
		return refused("%v", err)
	}
	return nil
}

// withKindAdmission runs start under the kind lock once kindGate admits
// name; a name of no kind starts without that lock.
func (a *app) withKindAdmission(ctx context.Context, name string, start func() error) error {
	if err := a.fatalAdmission(name, a.selectionKind == capacity.TaskKindReview); err != nil {
		return err
	}
	if len(a.cfg.Agents.KindsOf(name)) == 0 {
		return start()
	}
	l := flock.New(filepath.Join(cmp.Or(a.store.Dir(), a.cfg.StateDir), kindLockFile))
	wctx, cancel := context.WithTimeout(ctx, kindLockWait)
	defer cancel()
	ok, err := l.TryLockContext(wctx, 50*time.Millisecond)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !ok {
		return refused("%s: another start of a task kind is still under way after %s", name, dur(kindLockWait))
	}
	defer func() { _ = l.Unlock() }()
	if err := a.kindGate(name); err != nil {
		return err
	}
	// An outage may have opened while this start waited for its kind lock.
	if err := a.fatalAdmission(name, a.selectionKind == capacity.TaskKindReview); err != nil {
		return err
	}
	return start()
}

// kindDay is one kind's figures over a window: its recorded starts, the
// hours its sessions ran requests (each from its first to its last request
// in the window) and their spend.
type kindDay struct {
	Kind     string   `json:"kind"`
	Starts   int      `json:"starts"`
	Sessions int      `json:"sessions"`
	Hours    float64  `json:"hours"`
	USD      float64  `json:"usd"`
	Unpriced []string `json:"unpriced,omitempty"`
	// DailySpend is the kind's configured figure; 0: none.
	DailySpend float64 `json:"dailySpend,omitempty"`
}

// kindReport is the kinds' figures over [From, To); Unattributed is the
// spend of the transcripts no start of a kind's agent names (another
// agent, the person's own sessions, or a harness whose files name no
// session beekeeper recorded).
type kindReport struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Kinds        []kindDay `json:"kinds"`
	Unattributed float64   `json:"unattributedUSD"`
}

// kindSpend sums ts per kind of cfg over [from, to): a transcript belongs
// to the name its session was started or registered under.
func kindSpend(cfg *config.Config, st *state.State, ts []claude.SpendTranscript, from, to time.Time) kindReport {
	names := map[string]string{}
	for _, ag := range st.Agents {
		if ag.Session != "" {
			names[ag.Session] = ag.Name
		}
	}
	for _, s := range st.Starts {
		if s.Session != "" {
			names[s.Session] = s.Name
		}
	}
	r := kindReport{From: from, To: to}
	attributed := make([]bool, len(ts))
	for _, k := range cfg.Agents.Kinds {
		d := kindDay{Kind: k.Name, DailySpend: k.DailySpend}
		for _, s := range st.Starts {
			if !s.At.Before(from) && s.At.Before(to) && k.Matches(s.Name) {
				d.Starts++
			}
		}
		var sel []claude.SpendTranscript
		span := map[string][2]time.Time{}
		for i, t := range ts {
			n, ok := names[t.Session]
			if !ok || !k.Matches(n) || len(t.Requests) == 0 {
				continue
			}
			attributed[i] = true
			sel = append(sel, t)
			sp := span[t.Session]
			for _, q := range t.Requests {
				if sp[0].IsZero() || q.At.Before(sp[0]) {
					sp[0] = q.At
				}
				if q.At.After(sp[1]) {
					sp[1] = q.At
				}
			}
			span[t.Session] = sp
		}
		for _, sp := range span {
			d.Hours += sp[1].Sub(sp[0]).Hours()
		}
		d.Sessions = len(span)
		s := claude.PriceSpend(sel, from, to, cfg.Metrics)
		d.USD, d.Unpriced = s.USD, s.Unpriced
		r.Kinds = append(r.Kinds, d)
	}
	var rest []claude.SpendTranscript
	for i, t := range ts {
		if !attributed[i] {
			rest = append(rest, t)
		}
	}
	r.Unattributed = claude.PriceSpend(rest, from, to, cfg.Metrics).USD
	return r
}

// spendTranscripts reads every harness's transcripts over [from, to).
func (a *app) spendTranscripts(from, to time.Time) ([]claude.SpendTranscript, error) {
	ctx := a.context()
	var ts []claude.SpendTranscript
	for _, h := range harnesses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hs, err := h.SpendTranscripts(a.cfg, from, to)
		if err != nil {
			return nil, err
		}
		ts = append(ts, hs...)
	}
	return ts, nil
}

// kindDayWindow is the day of t in zone: its 00:00 to the next day's.
func kindDayWindow(t time.Time, zone *time.Location) (time.Time, time.Time) {
	y, m, d := t.In(zone).Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, zone)
	return from, from.AddDate(0, 0, 1)
}

// kindReportText renders a kind report: one line per kind, then the spend
// of no kind.
func kindReportText(r kindReport, zone *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s to %s, per task kind (agents.kinds):\n", r.From.In(zone).Format(time.RFC3339), r.To.In(zone).Format(time.RFC3339))
	if len(r.Kinds) == 0 {
		b.WriteString("  no agents.kinds configured\n")
	}
	for _, d := range r.Kinds {
		fmt.Fprintf(&b, "  %s: %d starts, %d sessions, %.1fh, $%.2f", d.Kind, d.Starts, d.Sessions, d.Hours, d.USD)
		if d.DailySpend > 0 {
			fmt.Fprintf(&b, " of dailySpend $%.2f", d.DailySpend)
		}
		if len(d.Unpriced) > 0 {
			fmt.Fprintf(&b, " (unpriced, left out: %s)", strings.Join(d.Unpriced, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  of no kind or no recorded session: $%.2f\n", r.Unattributed)
	return b.String()
}

// kindSpendEvery is how often the watch sums the day's spend per kind.
const kindSpendEvery = 10 * time.Minute

// kindSpendKey is the watch's one-time mark of a kind past its daily
// spend on a day.
const kindSpendPrefix = "kind-spend "

func kindSpendKey(kind string, day time.Time) string {
	return kindSpendPrefix + kind + " " + day.Format(time.DateOnly)
}

// kindSpending says SPEND <kind> once a day for each kind past its
// dailySpend: the day's spend so far in the machine's zone. The mark
// outlives a restarted watch.
func (w *watcher) kindSpending(now time.Time) {
	if !slices.ContainsFunc(w.cfg.Agents.Kinds, func(k config.AgentKind) bool { return k.DailySpend > 0 }) {
		return
	}
	zone, err := reportZone("", w.zone)
	if err != nil {
		zone = time.Local
	}
	from, _ := kindDayWindow(now, zone)
	st, err := w.store.Peek()
	if err != nil {
		return
	}
	ts, err := w.spendTranscripts(from, now)
	if w.context().Err() != nil {
		return
	}
	if err != nil {
		w.fail("kind-spend", "cannot sum the spend per task kind: %v", err)
		return
	}
	w.clear("kind-spend")
	today := from.Format(time.DateOnly)
	w.mu.Lock()
	for k := range w.reported {
		if strings.HasPrefix(k, kindSpendPrefix) && !strings.HasSuffix(k, " "+today) {
			delete(w.reported, k)
			w.dirty = true
		}
	}
	w.mu.Unlock()
	for _, d := range kindSpend(w.cfg, st, ts, from, now).Kinds {
		if d.DailySpend <= 0 || d.USD < d.DailySpend {
			continue
		}
		key := kindSpendKey(d.Kind, from)
		w.mu.Lock()
		said := w.reported[key]
		if !said {
			w.reported[key], w.dirty = true, true
		}
		w.mu.Unlock()
		if !said {
			w.emitNow("spend", "SPEND %s: $%.2f today in %d sessions of %d starts, over agents.kinds dailySpend $%.2f",
				d.Kind, d.USD, d.Sessions, d.Starts, d.DailySpend)
		}
	}
}

// sessionsByKind is sessions --by kind: day's figures per task kind.
func (a *app) sessionsByKind(day, tz string) error {
	zone, err := reportZone(tz, a.zone)
	if err != nil {
		return usageErr("--tz: %v", err)
	}
	ref := a.now
	if day != "" {
		if ref, err = time.ParseInLocation(time.DateOnly, day, zone); err != nil {
			return usageErr("--day %q: want 2006-01-02", day)
		}
	}
	from, to := kindDayWindow(ref, zone)
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	ts, err := a.spendTranscripts(from, to)
	if err != nil {
		return err
	}
	r := kindSpend(a.cfg, st, ts, from, to)
	if a.json {
		return a.printJSON(r)
	}
	_, err = fmt.Fprint(a.out, kindReportText(r, zone))
	return err
}

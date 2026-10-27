package cmd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// usageFile is the state's side file holding the latest subscription
	// usage reading, shared by watch, capacity, snapshot and agents start/resume.
	usageFile = "usage.json"
	// usageHighFile records the busy workers already told to park in the
	// current USAGE HIGH episode, so watch restarts never tell a worker twice.
	usageHighFile = "usage-high.json"
	// verbUsageHigh logs each busy worker told to park on USAGE HIGH.
	verbUsageHigh = "usage.high"
	// episodeGap separates consecutive 5-hour windows if watch slept across
	// a reset boundary while tolerating small resetsAt jitter within one window.
	episodeGap = 15 * time.Minute
)

// usageWindow is one subscription usage window (5-hour or 7-day).
type usageWindow struct {
	UsedPercent float64   `json:"usedPercent"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
}

// usageReading is the timestamped output of capacity.usage.command stored in
// usageFile.
type usageReading struct {
	At       time.Time    `json:"at"`
	FiveHour *usageWindow `json:"fiveHour,omitempty"`
	SevenDay *usageWindow `json:"sevenDay,omitempty"`
}

// usageView is how capacity, watch and snapshot present subscription usage and
// the target derived from it.
type usageView struct {
	At          time.Time    `json:"at,omitzero"`
	FiveHour    *usageWindow `json:"fiveHour,omitempty"`
	SevenDay    *usageWindow `json:"sevenDay,omitempty"`
	Floor       int          `json:"floor"`
	Ceiling     int          `json:"ceiling"`
	High        bool         `json:"high,omitempty"`
	HighPercent float64      `json:"highPercent,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// usageHighLedger tracks the current USAGE HIGH episode and who was told.
type usageHighLedger struct {
	Episode  string            `json:"episode,omitempty"`
	ResetsAt time.Time         `json:"resetsAt,omitzero"`
	Told     map[string]string `json:"told,omitempty"`
}

func runUsageCommand(ctx context.Context, command []string, timeout time.Duration) (usageReading, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // the configured capacity.usage.command
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return usageReading{}, fmt.Errorf("%s timed out after %s", filepath.Base(command[0]), timeout)
		}
		if msg := oneLine(stderr.String()); msg != "" {
			return usageReading{}, fmt.Errorf("%s: %s", err, truncate(msg, 120))
		}
		return usageReading{}, err
	}
	return parseUsageJSON(stdout.Bytes())
}

func parseUsageJSON(raw []byte) (usageReading, error) {
	type rawWindow struct {
		UsedPercent *float64 `json:"usedPercent"`
		ResetsAt    string   `json:"resetsAt"`
	}
	var doc struct {
		FiveHour *rawWindow `json:"fiveHour"`
		SevenDay *rawWindow `json:"sevenDay"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return usageReading{}, fmt.Errorf("invalid JSON: %w", err)
	}
	conv := func(name string, w *rawWindow) (*usageWindow, error) {
		if w == nil {
			return nil, nil
		}
		if w.UsedPercent == nil {
			return nil, fmt.Errorf("%s.usedPercent is missing", name)
		}
		v := *w.UsedPercent
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("%s.usedPercent %g is invalid", name, v)
		}
		var reset time.Time
		if s := strings.TrimSpace(w.ResetsAt); s != "" {
			var err error
			reset, err = time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, fmt.Errorf("%s.resetsAt: %w", name, err)
			}
			reset = reset.UTC()
		}
		return &usageWindow{UsedPercent: v, ResetsAt: reset}, nil
	}
	var r usageReading
	var err error
	if r.FiveHour, err = conv("fiveHour", doc.FiveHour); err != nil {
		return usageReading{}, err
	}
	if r.SevenDay, err = conv("sevenDay", doc.SevenDay); err != nil {
		return usageReading{}, err
	}
	if r.FiveHour == nil && r.SevenDay == nil {
		return usageReading{}, errors.New("usage output has neither fiveHour nor sevenDay")
	}
	return r, nil
}

func validateReading(r usageReading, u config.Usage) error {
	switch {
	case r.FiveHour == nil && r.SevenDay == nil:
		return errors.New("usage output has neither fiveHour nor sevenDay")
	case len(u.FiveHour) > 0 && r.FiveHour == nil:
		return errors.New("usage output omits fiveHour (capacity.usage.fiveHour is configured)")
	case u.HighPercentSet() && r.FiveHour == nil:
		return errors.New("usage output omits fiveHour (capacity.usage.highPercent is configured)")
	case len(u.SevenDay) > 0 && r.SevenDay == nil:
		return errors.New("usage output omits sevenDay (capacity.usage.sevenDay is configured)")
	}
	return nil
}

// usageFresh reports whether r is fresh at now: within u.EffectiveEvery(), and
// neither window crossed its ResetsAt since r.At.
func usageFresh(r usageReading, u config.Usage, now time.Time) bool {
	if r.At.IsZero() || now.Sub(r.At) > u.EffectiveEvery() || r.At.After(now.Add(time.Minute)) {
		return false
	}
	for _, w := range []*usageWindow{r.FiveHour, r.SevenDay} {
		if w != nil && !w.ResetsAt.IsZero() && r.At.Before(w.ResetsAt) && !now.Before(w.ResetsAt) {
			return false
		}
	}
	return true
}

// effectiveWindow returns w at now: a window whose ResetsAt has passed has
// reset to 0% in the new window.
func effectiveWindow(w *usageWindow, now time.Time) *usageWindow {
	if w == nil {
		return nil
	}
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
		return &usageWindow{UsedPercent: 0}
	}
	return &usageWindow{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt}
}

func pctPtr(w *usageWindow) *float64 {
	if w == nil {
		return nil
	}
	v := w.UsedPercent
	return &v
}

func deriveUsageView(r usageReading, k config.Capacity, now time.Time) (config.Capacity, *usageView) {
	fh := effectiveWindow(r.FiveHour, now)
	sd := effectiveWindow(r.SevenDay, now)
	derived, high := k.Derive(pctPtr(fh), pctPtr(sd))
	return derived, &usageView{
		At:          r.At,
		FiveHour:    fh,
		SevenDay:    sd,
		Floor:       derived.Floor,
		Ceiling:     derived.Ceiling,
		High:        high,
		HighPercent: k.Usage.EffectiveHighPercent(),
	}
}

func errorUsageView(k config.Capacity, err error) *usageView {
	return &usageView{
		Floor:       k.Floor,
		Ceiling:     k.Ceiling,
		HighPercent: k.Usage.EffectiveHighPercent(),
		Error:       err.Error(),
	}
}

func (a *app) execUsage(ctx context.Context, u config.Usage) (usageReading, error) {
	if a.readUsage != nil {
		return a.readUsage(ctx, u.Command, u.EffectiveTimeout())
	}
	return runUsageCommand(ctx, u.Command, u.EffectiveTimeout())
}

// cachedUsageCapacity derives Capacity from a fresh usage.json if one exists,
// without running capacity.usage.command.
func (a *app) cachedUsageCapacity(k config.Capacity) (config.Capacity, *usageView) {
	if !k.Usage.Enabled() || a.store == nil {
		return k, nil
	}
	var r usageReading
	if found, err := a.store.ReadFile(usageFile, &r); err == nil && found &&
		usageFresh(r, k.Usage, a.now) && validateReading(r, k.Usage) == nil {
		return deriveUsageView(r, k, a.now)
	}
	return k, nil
}

// resolveUsage returns the derived Capacity and usageView for k, reusing a
// fresh usage.json or running capacity.usage.command and saving usage.json.
// In the agent sandbox (where home credentials and external domains are
// closed), a recent usage.json written by the host's watch within 2 × every
// (before resetsAt) is kept when the command cannot run inside the sandbox.
func (a *app) resolveUsage(ctx context.Context, k config.Capacity) (config.Capacity, *usageView) {
	if !k.Usage.Enabled() {
		return k, nil
	}
	var cached usageReading
	var haveCached bool
	if a.store != nil {
		if found, err := a.store.ReadFile(usageFile, &cached); err == nil && found && validateReading(cached, k.Usage) == nil {
			if usageFresh(cached, k.Usage, a.now) {
				return deriveUsageView(cached, k, a.now)
			}
			haveCached = true
		}
	}
	r, err := a.execUsage(ctx, k.Usage)
	if err == nil {
		err = validateReading(r, k.Usage)
	}
	if err != nil {
		if haveCached && inSandbox() {
			grace := k.Usage
			grace.Every = config.Duration{Duration: 2 * k.Usage.EffectiveEvery()}
			if usageFresh(cached, grace, a.now) {
				return deriveUsageView(cached, k, a.now)
			}
		}
		return k, errorUsageView(k, err)
	}
	r.At = a.now.UTC()
	if a.store != nil {
		_ = a.store.WriteFile(usageFile, r)
	}
	return deriveUsageView(r, k, a.now)
}

// usageHighBlock returns the USAGE HIGH block reason if USAGE HIGH holds now,
// "" otherwise.
func (a *app) usageHighBlock(ctx context.Context) string {
	k := a.liveCapacity()
	if !k.Usage.Enabled() {
		return ""
	}
	_, u := a.resolveUsage(ctx, k)
	if u == nil {
		return ""
	}
	if u.High {
		return u.highBlock(a.now)
	}
	if u.Error != "" && a.store != nil {
		var l usageHighLedger
		if found, err := a.store.ReadFile(usageHighFile, &l); err == nil && found &&
			l.Episode != "" && !l.ResetsAt.IsZero() && a.now.Before(l.ResetsAt) {
			return fmt.Sprintf("usage high until %s (in %s); usage unreadable (%s)",
				clock(a.now.Local(), l.ResetsAt), dur(l.ResetsAt.Sub(a.now)), truncate(u.Error, 80))
		}
	}
	return ""
}

func windowFigure(label string, w *usageWindow, now time.Time) string {
	s := fmt.Sprintf("%s %s", label, pct(w.UsedPercent))
	if !w.ResetsAt.IsZero() && now.Before(w.ResetsAt) {
		s += fmt.Sprintf(" (resets %s, in %s)", clock(now.Local(), w.ResetsAt), dur(w.ResetsAt.Sub(now)))
	}
	return s
}

func (u *usageView) windowsLine(now time.Time) string {
	var parts []string
	if u.FiveHour != nil {
		parts = append(parts, windowFigure("5h", u.FiveHour, now))
	}
	if u.SevenDay != nil {
		parts = append(parts, windowFigure("7d", u.SevenDay, now))
	}
	return strings.Join(parts, ", ")
}

func (u *usageView) capacitySuffix(now time.Time) string {
	if u.Error != "" {
		return "usage unknown (" + truncate(u.Error, 120) + ")"
	}
	return "usage " + u.windowsLine(now)
}

func (u *usageView) watchLine(now time.Time) string {
	s := fmt.Sprintf("%s; target floor %d, ceiling %d", u.windowsLine(now), u.Floor, u.Ceiling)
	if u.High {
		s += " (USAGE HIGH)"
	}
	return s
}

func (u *usageView) snapshotLine(now time.Time) string {
	if u.Error != "" {
		return "usage: unknown (" + truncate(u.Error, 120) + ")"
	}
	return "usage: " + u.watchLine(now)
}

func (u *usageView) highFigure(now time.Time) string {
	used := u.HighPercent
	var reset time.Time
	if u.FiveHour != nil {
		used, reset = u.FiveHour.UsedPercent, u.FiveHour.ResetsAt
	}
	s := fmt.Sprintf("5h %s at or past %s", pct(used), pct(u.HighPercent))
	if !reset.IsZero() && now.Before(reset) {
		s += fmt.Sprintf(" (resets %s, in %s)", clock(now.Local(), reset), dur(reset.Sub(now)))
	}
	return s
}

func (u *usageView) highBlock(now time.Time) string {
	return "usage high: " + u.highFigure(now)
}

func (u *usageView) highWatchLine(now time.Time) string {
	return "USAGE HIGH: " + u.highFigure(now) + "; new starts and resumes wait until reset, telling busy workers to finish their step and park"
}

func (w *watcher) usageNeedsRefresh(k config.Capacity, now time.Time) bool {
	var cached usageReading
	if found, err := w.store.ReadFile(usageFile, &cached); err == nil && found &&
		usageFresh(cached, k.Usage, now) && validateReading(cached, k.Usage) == nil {
		return false
	}
	w.mu.Lock()
	last := w.lastUsage
	w.mu.Unlock()
	retry := min(k.Usage.EffectiveEvery(), max(w.cfg.Watch.Interval.Duration, time.Second))
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) refreshUsage(ctx context.Context, k config.Capacity, now time.Time) {
	if now.IsZero() {
		now = w.clk().Now()
	}
	w.mu.Lock()
	w.lastUsage = now
	w.mu.Unlock()
	r, err := w.execUsage(ctx, k.Usage)
	if err == nil {
		err = validateReading(r, k.Usage)
	}
	if err != nil {
		w.mu.Lock()
		w.usageErr = err.Error()
		w.mu.Unlock()
		return
	}
	r.At = now.UTC()
	_ = w.store.WriteFile(usageFile, r)
	w.mu.Lock()
	w.usageErr = ""
	w.mu.Unlock()
}

func (w *watcher) currentUsage(k config.Capacity, now time.Time) (config.Capacity, *usageView) {
	if !k.Usage.Enabled() {
		return k, nil
	}
	w.mu.Lock()
	uerr := w.usageErr
	w.mu.Unlock()
	if uerr != "" {
		return k, errorUsageView(k, errors.New(uerr))
	}
	var r usageReading
	if found, err := w.store.ReadFile(usageFile, &r); err == nil && found && validateReading(r, k.Usage) == nil {
		return deriveUsageView(r, k, now)
	}
	return k, nil
}

// watchUsage refreshes subscription usage when due, prints periodic USAGE
// lines, tracks USAGE HIGH / ENDED USAGE HIGH and tells live busy workers once
// per USAGE HIGH episode to finish their step and park.
func (w *watcher) watchUsage(ctx context.Context, st *state.State, sessions []*claude.Session) *usageView {
	k := w.liveCapacity()
	if !k.Usage.Enabled() {
		w.clear("usage-error")
		w.clear("usage-high")
		w.clearUsageHighEpisode()
		return nil
	}
	if !w.chores {
		_, u := w.resolveUsage(ctx, k)
		if u == nil {
			return nil
		}
		if u.Error != "" {
			w.fail("usage-error", "USAGE unreadable: %s", u.Error)
			return u
		}
		w.clear("usage-error")
		w.emitNow("usage", "USAGE: %s", u.watchLine(w.now))
		w.check("usage-high", u.High, "%s", u.highWatchLine(w.now))
		return u
	}
	now := w.now
	if w.usageNeedsRefresh(k, now) {
		w.inFlight(ctx, w.cfg.Watch.Interval.Duration, &w.usageBusy, func(ctx context.Context) {
			w.refreshUsage(ctx, k, now)
		})
	}
	_, u := w.currentUsage(k, w.now)
	if u == nil {
		return nil
	}
	if u.Error != "" {
		w.fail("usage-error", "USAGE unreadable: %s", u.Error)
		return u
	}
	w.clear("usage-error")
	if rep := k.Usage.ReportEvery.Duration; rep > 0 {
		w.mu.Lock()
		due := w.usageReported.IsZero() || w.now.Sub(w.usageReported) >= rep
		if due {
			w.usageReported = w.now
			w.dirty = true
		}
		w.mu.Unlock()
		if due {
			w.emitNow("usage", "USAGE: %s", u.watchLine(w.now))
		}
	}
	w.check("usage-high", u.High, "%s", u.highWatchLine(w.now))
	if !u.High {
		w.clearUsageHighEpisode()
		return u
	}
	if st.Stop == nil {
		w.tellBusyWorkersUsageHigh(ctx, st, sessions, u)
	}
	return u
}

func (w *watcher) clearUsageHighEpisode() {
	if w.store == nil {
		return
	}
	var l usageHighLedger
	if found, err := w.store.ReadFile(usageHighFile, &l); err == nil && found && l.Episode != "" {
		_ = w.store.WriteFile(usageHighFile, usageHighLedger{})
	}
}

func (w *watcher) tellBusyWorkersUsageHigh(ctx context.Context, st *state.State, sessions []*claude.Session, u *usageView) {
	var l usageHighLedger
	if w.store != nil {
		_, _ = w.store.ReadFile(usageHighFile, &l)
	}
	var reset time.Time
	if u.FiveHour != nil {
		reset = u.FiveHour.ResetsAt
	}
	newEpisode := l.Episode == "" ||
		(!l.ResetsAt.IsZero() && !reset.IsZero() && reset.Sub(l.ResetsAt) > episodeGap)
	changed := false
	if newEpisode {
		l = usageHighLedger{
			Episode:  w.now.UTC().Format(time.RFC3339),
			ResetsAt: reset,
			Told:     map[string]string{},
		}
		changed = true
	} else if !reset.IsZero() && !reset.Equal(l.ResetsAt) {
		l.ResetsAt = reset
		changed = true
	}
	if l.Told == nil {
		l.Told = map[string]string{}
	}
	msg := fmt.Sprintf("USAGE HIGH (%s): finish your current step, commit or save any work in flight, and park with `beekeeper agents park \"5-hour usage window reset\"` (or `--on` your open PR or note) so the 5-hour window can reset.",
		u.highFigure(w.now))
	for _, ag := range liveBusyWorkers(st, sessions, w.now) {
		key := cmp.Or(ag.Session, ag.Name)
		if l.Told[key] != "" {
			continue
		}
		quiet := *w.app
		quiet.out = io.Discard
		if err := wakeOwner(&quiet, ctx, watchParty, ag.Name, msg, ""); err != nil {
			w.emit("usage-wake-"+key, "USAGE HIGH WAKE FAILED %q: %v", ag.Name, err)
			continue
		}
		w.clear("usage-wake-" + key)
		l.Told[key] = w.now.UTC().Format(time.RFC3339)
		changed = true
		_ = w.store.Log(event(watchParty, verbUsageHigh, "%s: told to finish its step and park (%s)", ag.Name, u.highFigure(w.now)))
	}
	if changed && w.store != nil {
		_ = w.store.WriteFile(usageHighFile, l)
	}
}

// liveBusyWorkers are the busy workers whose CLI is running now: role holders,
// open relay successors, the one-off reporter, parked/kept/waiting agents and
// stopped CLIs are skipped so USAGE HIGH never launches a headless turn.
func liveBusyWorkers(st *state.State, sessions []*claude.Session, now time.Time) []state.Agent {
	var out []state.Agent
	sup, guide := st.SupervisorRole(), st.GuideRole()
	for _, ag := range st.Agents {
		p := ag.Party
		if roleRun(st, p) || sup.Relay.Open(now) && sup.Relay.To.Is(p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
			continue
		}
		if st.Report.Running() && ag.Is(st.Report.Party) {
			continue
		}
		if ag.Task == "" || ag.Done || keptBy(st, ag, now) != "" {
			continue
		}
		s, live := claude.Live(sessions, p)
		if !live || s.Waiting != nil || s.OwnWait(now, waitQuiet) != "" {
			continue
		}
		out = append(out, ag)
	}
	return out
}

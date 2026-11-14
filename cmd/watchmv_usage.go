package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// usageUnderusedFile records whether a P1 finding was already filed for
// the current 7-day subscription window's heavy underuse.
const usageUnderusedFile = "usage-underused.json"

func usageUnderusedTargetFile(target string) string {
	if strings.TrimSpace(target) == "" {
		return usageUnderusedFile
	}
	return "usage-underused-" + unsafeKey.ReplaceAllString(target, "_") + ".json"
}

// usageHighLedger tracks the current USAGE HIGH episode and who was told.
type usageHighLedger struct {
	Episode  string            `json:"episode,omitempty"`
	ResetsAt time.Time         `json:"resetsAt,omitzero"`
	Told     map[string]string `json:"told,omitempty"`
}

// usageUnderusedLedger tracks whether a P1 finding was already filed for the
// current 7-day subscription window.
type usageUnderusedLedger struct {
	ResetsAt   time.Time `json:"resetsAt,omitzero"`
	ReportedAt time.Time `json:"reportedAt,omitzero"`
}

func sameUnderuseWindow(prev, cur time.Time) bool {
	if prev.IsZero() && cur.IsZero() {
		return true
	}
	if prev.IsZero() || cur.IsZero() {
		return false
	}
	diff := cur.Sub(prev)
	if diff < 0 {
		diff = -diff
	}
	return diff <= episodeGap
}

func (w *watcher) checkUsageUnderused(condKey, file, target string, sd *capacity.UsageWindow, u config.Usage) {
	underused := capacity.WindowUnderused(sd, u, w.a.now)
	if !underused {
		w.check(condKey, false, "")
		if w.chores && w.a.store != nil && sd != nil && !sd.ResetsAt.IsZero() {
			var l usageUnderusedLedger
			if found, err := w.a.store.ReadFile(file, &l); err == nil && found && !l.ReportedAt.IsZero() && !sameUnderuseWindow(l.ResetsAt, sd.ResetsAt) {
				_ = w.a.store.WriteFile(file, usageUnderusedLedger{})
			}
		}
		return
	}
	fig := capacity.UnderusedFigureText(sd, u, w.a.now)
	line := "USAGE UNDERUSED: " + fig + "; P1 finding filed"
	if target != "" {
		line = fmt.Sprintf("USAGE UNDERUSED (%s): %s; P1 finding filed", target, fig)
	}
	w.check(condKey, true, "%s", line)
	if !w.chores || w.a.store == nil {
		return
	}
	var l usageUnderusedLedger
	_, _ = w.a.store.ReadFile(file, &l)
	if !l.ReportedAt.IsZero() && sameUnderuseWindow(l.ResetsAt, sd.ResetsAt) {
		return
	}
	l = usageUnderusedLedger{ResetsAt: sd.ResetsAt, ReportedAt: w.a.now.UTC()}
	_ = w.a.store.WriteFile(file, l)
	subject := cmp.Or(target, "subscription")
	winKey := w.a.now.UTC().Format(time.RFC3339)
	if !sd.ResetsAt.IsZero() {
		winKey = sd.ResetsAt.UTC().Format(time.RFC3339)
	}
	report := fmt.Sprintf("P1: subscription budget for %s is heavily underused: %s. File a P1 issue and ensure starts use this subscription budget before reset.",
		subject, fig)
	detail := fmt.Sprintf("[%s %s] P1 subscription budget underused (%s)%s%s",
		condKey, winKey, subject, loop.FileReportSeparator, report)
	if err := w.a.store.Record(state.Event{At: w.a.now, By: watchParty, Verb: loop.LoopFileVerb, Detail: detail}); err != nil {
		w.fail(condKey+"-guide", "cannot retain usage underuse finding for guide: %v", err)
	}
}

func (w *watcher) usageNeedsRefresh(k config.Capacity, now time.Time) bool {
	if len(k.Providers) > 0 {
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if p.Enabled() && w.providerUsageNeedsRefresh(name, p.Usage, now) {
				return true
			}
		}
		return false
	}
	var cached capacity.UsageReading
	if found, err := w.a.store.ReadFile(usageFile, &cached); err == nil && found &&
		capacity.UsageFresh(cached, k.Usage, now) && capacity.ValidateReadingAt(cached, k.Usage, now) == nil {
		w.mu.Lock()
		w.usageErr = ""
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	last := w.lastUsage
	w.mu.Unlock()
	retry := min(k.Usage.EffectiveEvery(), w.usageRetryFloor())
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) providerUsageNeedsRefresh(name string, u config.Usage, now time.Time) bool {
	var cached capacity.UsageReading
	if found, err := w.a.store.ReadFile(usageProviderFile(name), &cached); err == nil && found &&
		!w.a.useCodexRolloutUsage(u, cached) &&
		capacity.UsageFresh(cached, u, now) && capacity.ValidateReadingAt(cached, u, now) == nil {
		w.mu.Lock()
		delete(w.providerUsageErr, name)
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	last := w.providerLastUsage[name]
	w.mu.Unlock()
	retry := min(u.EffectiveEvery(), w.usageRetryFloor())
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) usageRetryFloor() time.Duration {
	if w.a.cfg != nil {
		return max(w.a.cfg.Watch.Interval.Duration, time.Second)
	}
	return time.Second
}

// Providers are read sequentially; allow each its configured command deadline.
func usageRefreshTimeout(k config.Capacity) time.Duration {
	var timeout time.Duration
	for _, p := range k.Providers {
		if p.Enabled() {
			timeout += p.EffectiveTimeout()
		}
	}
	return max(timeout, k.Usage.EffectiveTimeout())
}

func (w *watcher) usageNow() time.Time {
	if w.clock != nil || w.polls.Load() > 0 || w.a.now.IsZero() {
		return w.clk().Now()
	}
	return w.a.now
}

// execUsageNow runs in the command, which holds what it needs (watchExecUsageNow).
func (w *watcher) execUsageNow(ctx context.Context, file string, u config.Usage) (capacity.UsageReading, error) {
	return watchExecUsageNow(w, ctx, file, u)
}

// refreshUsage stamps every provider read with its own observation time: a
// slow earlier provider must not age the schedule of the next one.
func (w *watcher) refreshUsage(ctx context.Context, k config.Capacity) {
	if len(k.Providers) > 0 {
		for _, name := range k.ProviderNames() {
			now := w.usageNow()
			p := k.Providers[name]
			if !p.Enabled() || !w.providerUsageNeedsRefresh(name, p.Usage, now) {
				continue
			}
			r, err := w.execUsageNow(ctx, usageProviderFile(name), p.Usage)
			if err != nil && isProbeInProgress(err.Error()) || isProbeInProgress(r.Fallback) {
				continue
			}
			w.mu.Lock()
			if w.providerLastUsage == nil {
				w.providerLastUsage = map[string]time.Time{}
			}
			w.providerLastUsage[name] = now
			if w.providerUsageErr == nil {
				w.providerUsageErr = map[string]string{}
			}
			if err != nil {
				w.providerUsageErr[name] = err.Error()
				w.mu.Unlock()
				w.a.retainUsageStop(usageProviderFile(name), err)
				continue
			}
			w.providerUsageErr[name] = r.Fallback
			w.mu.Unlock()
		}
		return
	}
	now := w.usageNow()
	r, err := w.execUsageNow(ctx, usageFile, k.Usage)
	if err != nil && isProbeInProgress(err.Error()) || isProbeInProgress(r.Fallback) {
		return
	}
	w.mu.Lock()
	w.lastUsage = now
	if err != nil {
		w.usageErr = err.Error()
		w.mu.Unlock()
		w.a.retainUsageStop(usageFile, err)
		return
	}
	w.usageErr = r.Fallback
	w.mu.Unlock()
}

func (w *watcher) currentUsage(k config.Capacity, now time.Time) (config.Capacity, *capacity.UsageView) {
	if len(k.Providers) > 0 {
		w.mu.Lock()
		errs := maps.Clone(w.providerUsageErr)
		w.mu.Unlock()
		return w.a.cachedProvidersCapacityAt(k, now, errs)
	}
	if !k.Usage.Enabled() {
		return k, nil
	}
	w.mu.Lock()
	uerr := w.usageErr
	w.mu.Unlock()
	var cause error
	if uerr != "" {
		cause = errors.New(uerr)
	}
	r, err := w.a.cachedUsageReading(usageFile, k.Usage, now, cause)
	if err != nil {
		if uerr == "" && !slices.Equal(k.Usage.Command, config.DefaultCodexUsageCommand) {
			return k, nil
		}
		return capacity.ErrorUsageView(k, err)
	}
	return capacity.DeriveUsageView(r, k, now)
}

// reportUsageFallback keeps unreadability visible even when starts can continue.
// The keyed report feeds the guide's existing durable problem queue.
func (w *watcher) reportUsageFallback(key, provider, failure string) {
	if failure == "" || isProbeInProgress(failure) {
		w.clear(key)
		return
	}
	label := "USAGE"
	if provider != "" {
		label += " " + provider
	}
	line := w.fail(key, "%s unreadable: %s", label, failure)
	if line != "" && w.chores && w.a.store != nil {
		detail := fmt.Sprintf("[%s %s] usage reader failure%s%s", key, w.a.now.UTC().Format(time.RFC3339Nano), loop.FileReportSeparator, line)
		if err := w.a.store.Record(state.Event{At: w.a.now, By: watchParty, Verb: loop.LoopFileVerb, Detail: detail}); err != nil {
			w.fail(key+"-guide", "cannot retain usage failure for guide: %v", err)
		}
	}
}

// watchUsage refreshes subscription usage when due, prints USAGE lines, tracks
// USAGE HIGH / ENDED USAGE HIGH and tells live busy workers once per USAGE HIGH
// episode to finish their step and park.
func (w *watcher) watchUsage(ctx context.Context, st *state.State, sessions []*claude.Session) *capacity.UsageView {
	k := w.a.liveCapacity()
	if len(k.Providers) > 0 {
		return w.watchProvidersUsage(ctx, k, st, sessions)
	}
	if !k.Usage.Enabled() {
		w.clear("usage-error")
		w.clear("usage-high")
		w.clear("usage-underused")
		w.clearUsageHighEpisode(usageHighFile)
		return nil
	}
	if !w.chores {
		_, u := w.a.resolveUsage(ctx, k)
		if u == nil {
			return nil
		}
		if u.Error != "" {
			w.reportUsageFallback("usage-error", "", u.Error)
			return u
		}
		w.reportUsageFallback("usage-error", "", u.Fallback)
		w.emitNow("usage", "USAGE: %s", u.WatchLine(w.a.now))
		w.check("usage-high", u.High, "%s", u.HighWatchLine(w.a.now))
		w.checkUsageUnderused("usage-underused", usageUnderusedFile, "", u.SevenDay, k.Usage)
		return u
	}
	now := w.usageNow()
	w.a.now = now
	if w.usageNeedsRefresh(k, now) {
		w.inFlight(ctx, k.Usage.EffectiveTimeout(), &w.usageBusy, func(ctx context.Context) {
			w.refreshUsage(ctx, k)
		})
		now = w.usageNow()
		w.a.now = now
	}
	_, u := w.currentUsage(k, w.a.now)
	if u == nil {
		return nil
	}
	if u.Error != "" {
		w.reportUsageFallback("usage-error", "", u.Error)
		return u
	}
	w.reportUsageFallback("usage-error", "", u.Fallback)
	if rep := k.Usage.ReportEvery.Duration; rep > 0 {
		w.mu.Lock()
		due := w.usageReported.IsZero() || w.a.now.Sub(w.usageReported) >= rep
		if due {
			w.usageReported = w.a.now
			w.dirty = true
		}
		w.mu.Unlock()
		if due {
			w.emitNow("usage", "USAGE: %s", u.WatchLine(w.a.now))
		}
	}
	w.check("usage-high", u.High, "%s", u.HighWatchLine(w.a.now))
	w.checkUsageUnderused("usage-underused", usageUnderusedFile, "", u.SevenDay, k.Usage)
	if !u.High {
		w.clearUsageHighEpisode(usageHighFile)
		return u
	}
	if st.Stop == nil {
		w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighFile, nil)
	}
	return u
}

func (w *watcher) watchProvidersUsage(ctx context.Context, k config.Capacity, st *state.State, sessions []*claude.Session) *capacity.UsageView {
	w.a.syncAgyQuotaFromState(st, w.a.now)
	readings := map[string]capacity.ProviderReadingResult{}
	if !w.chores {
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			r, err := w.a.resolveReading(ctx, usageProviderFile(name), p.Usage)
			if err != nil {
				w.mu.Lock()
				if w.providerUsageErr == nil {
					w.providerUsageErr = map[string]string{}
				}
				w.providerUsageErr[name] = err.Error()
				w.mu.Unlock()
				readings[name] = capacity.ProviderReadingResult{Err: err}
				continue
			}
			readings[name] = capacity.ProviderReadingResult{Reading: r, OK: true}
		}
	} else {
		now := w.usageNow()
		w.a.now = now
		if w.usageNeedsRefresh(k, now) {
			w.inFlight(ctx, usageRefreshTimeout(k), &w.usageBusy, func(ctx context.Context) {
				w.refreshUsage(ctx, k)
			})
			now = w.usageNow()
			w.a.now = now
		}
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			w.mu.Lock()
			uerr := w.providerUsageErr[name]
			w.mu.Unlock()
			var cause error
			if uerr != "" {
				cause = errors.New(uerr)
			}
			r, err := w.a.cachedUsageReading(usageProviderFile(name), p.Usage, w.a.now, cause)
			readings[name] = capacity.ProviderReadingResult{Reading: r, Err: err, OK: err == nil}
		}
	}

	c := countAgents(st, sessions, w.a.now, w.a.cfg)
	pa := countProviderAgents(st, sessions, k, w.a.now, w.a.cfg)
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.SelfHosted() {
			continue
		}
		// A self-hosted provider reads no subscription usage; its ceiling may
		// follow the serving mode of the model it serves, and the modes a
		// node constraint holds back lower it before any switch ran.
		readings[name] = w.a.serveModeReadings(ctx, name, p)
	}
	w.mu.Lock()
	swap := w.swap
	w.mu.Unlock()
	var h *capacity.Headroom
	if w.readHeadroom != nil {
		h = w.readHeadroom(ctx, swap)
	} else {
		h = w.a.readHeadroom(ctx, k, swap)
	}
	withPaceOffs(readings, st, w.a.now)
	v := capacityStopped(capacity.BuildProvidersCapacity(c, pa, k, readings, h, false, nil, capacity.ForWorker, w.a.now), w.a, st)

	var parts []string
	allHigh := len(k.Providers) > 0
	anyUnderused := false
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.Enabled() {
			allHigh = false
		}
		pv := v.Providers[name]
		if pv == nil {
			continue
		}
		anyUnderused = anyUnderused || pv.Underused
		if pv.Kind == config.KindSelfHosted {
			// No subscription behind it: no USAGE HIGH, nothing to tell its
			// workers to park for; its own ceiling is the only guard.
			allHigh = false
			if pv.ServingModeError != "" {
				w.fail("serve-mode-error-"+name, "SERVE MODE %s unreadable: %s", name, pv.ServingModeError)
			} else {
				w.clear("serve-mode-error-" + name)
			}
			parts = append(parts, name+": "+pv.WatchSummary(w.a.now))
			continue
		}
		if pv.Error != "" {
			w.reportUsageFallback("usage-error-"+name, name, pv.Error)
		} else {
			w.reportUsageFallback("usage-error-"+name, name, pv.Fallback)
		}
		parts = append(parts, name+": "+pv.WatchSummary(w.a.now))
		if len(pv.Buckets) > 0 && !pv.SupplementalBuckets {
			for _, bName := range pv.BucketOrder {
				bv := pv.Buckets[bName]
				if bv == nil {
					continue
				}
				target := name + "/" + bName
				highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
					target, capacity.HighFigureText(bv.FiveHour, pv.HighPercent, w.a.now), target)
				w.check("usage-high-"+name+"-"+bName, bv.High, "%s", highLine)
				w.checkUsageUnderused("usage-underused-"+name+"-"+bName, usageUnderusedTargetFile(target), target, bv.SevenDay, p.Usage)
				ledgerFile := usageHighBucketFile(name, bName)
				if !bv.High {
					allHigh = false
					w.clearUsageHighEpisode(ledgerFile)
				} else if st.Stop == nil {
					u := &capacity.UsageView{FiveHour: bv.FiveHour, High: true, HighPercent: pv.HighPercent}
					w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, ledgerFile, func(ag state.Agent) bool {
						return slices.Contains(bv.Busy, ag.Name)
					})
				}
			}
			continue
		}
		highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
			name, capacity.HighFigureText(pv.FiveHour, pv.HighPercent, w.a.now), name)
		w.check("usage-high-"+name, pv.High, "%s", highLine)
		w.checkUsageUnderused("usage-underused-"+name, usageUnderusedTargetFile(name), name, pv.SevenDay, p.Usage)
		if !pv.High {
			allHigh = false
			w.clearUsageHighEpisode(usageHighProviderFile(name))
		} else if st.Stop == nil {
			u := &capacity.UsageView{FiveHour: pv.FiveHour, High: true, HighPercent: pv.HighPercent}
			w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighProviderFile(name), func(ag state.Agent) bool {
				return slices.Contains(pv.Busy, ag.Name)
			})
		}
	}
	if v.Choice != nil {
		parts = append(parts, "choice: "+v.Choice.Summary())
	}
	shouldEmit := !w.chores
	if w.chores {
		switch {
		case !k.Usage.ReportEverySet():
			shouldEmit = true
		case k.Usage.ReportEvery.Duration > 0:
			w.mu.Lock()
			due := w.usageReported.IsZero() || w.a.now.Sub(w.usageReported) >= k.Usage.ReportEvery.Duration
			if due {
				w.usageReported = w.a.now
				w.dirty = true
			}
			w.mu.Unlock()
			shouldEmit = due
		}
	}
	if shouldEmit && len(parts) > 0 {
		w.emitNow("usage", "USAGE: %s", strings.Join(parts, "; "))
	}
	return &capacity.UsageView{Ceiling: v.Ceiling, High: allHigh, Underused: anyUnderused}
}

func (w *watcher) clearUsageHighEpisode(file string) {
	if w.a.store == nil {
		return
	}
	var l usageHighLedger
	if found, err := w.a.store.ReadFile(file, &l); err == nil && found && l.Episode != "" {
		_ = w.a.store.WriteFile(file, usageHighLedger{})
	}
}

// tellBusyWorkersUsageHigh runs in the command, which holds what it needs (watchTellBusyWorkersUsageHigh).
func (w *watcher) tellBusyWorkersUsageHigh(ctx context.Context, st *state.State, sessions []*claude.Session, u *capacity.UsageView, ledgerFile string, match func(state.Agent) bool) {
	watchTellBusyWorkersUsageHigh(w, ctx, st, sessions, u, ledgerFile, match)
}

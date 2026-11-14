package watch

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
	return "usage-underused-" + Cmd.UnsafeKey().ReplaceAllString(target, "_") + ".json"
}

// UsageHighLedger tracks the current USAGE HIGH episode and who was told.
type UsageHighLedger struct {
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

func (w *Watcher) checkUsageUnderused(condKey, file, target string, sd *capacity.UsageWindow, u config.Usage) {
	underused := capacity.WindowUnderused(sd, u, w.Host.Now())
	if !underused {
		w.Check(condKey, false, "")
		if w.Chores && w.Host.Store() != nil && sd != nil && !sd.ResetsAt.IsZero() {
			var l usageUnderusedLedger
			if found, err := w.Host.Store().ReadFile(file, &l); err == nil && found && !l.ReportedAt.IsZero() && !sameUnderuseWindow(l.ResetsAt, sd.ResetsAt) {
				_ = w.Host.Store().WriteFile(file, usageUnderusedLedger{})
			}
		}
		return
	}
	fig := capacity.UnderusedFigureText(sd, u, w.Host.Now())
	line := "USAGE UNDERUSED: " + fig + "; P1 finding filed"
	if target != "" {
		line = fmt.Sprintf("USAGE UNDERUSED (%s): %s; P1 finding filed", target, fig)
	}
	w.Check(condKey, true, "%s", line)
	if !w.Chores || w.Host.Store() == nil {
		return
	}
	var l usageUnderusedLedger
	_, _ = w.Host.Store().ReadFile(file, &l)
	if !l.ReportedAt.IsZero() && sameUnderuseWindow(l.ResetsAt, sd.ResetsAt) {
		return
	}
	l = usageUnderusedLedger{ResetsAt: sd.ResetsAt, ReportedAt: w.Host.Now().UTC()}
	_ = w.Host.Store().WriteFile(file, l)
	subject := cmp.Or(target, "subscription")
	winKey := w.Host.Now().UTC().Format(time.RFC3339)
	if !sd.ResetsAt.IsZero() {
		winKey = sd.ResetsAt.UTC().Format(time.RFC3339)
	}
	report := fmt.Sprintf("P1: subscription budget for %s is heavily underused: %s. File a P1 issue and ensure starts use this subscription budget before reset.",
		subject, fig)
	detail := fmt.Sprintf("[%s %s] P1 subscription budget underused (%s)%s%s",
		condKey, winKey, subject, loop.FileReportSeparator, report)
	if err := w.Host.Store().Record(state.Event{At: w.Host.Now(), By: WatchParty, Verb: loop.LoopFileVerb, Detail: detail}); err != nil {
		w.Fail(condKey+"-guide", "cannot retain usage underuse finding for guide: %v", err)
	}
}

func (w *Watcher) usageNeedsRefresh(k config.Capacity, now time.Time) bool {
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
	if found, err := w.Host.Store().ReadFile(usageFile, &cached); err == nil && found &&
		capacity.UsageFresh(cached, k.Usage, now) && capacity.ValidateReadingAt(cached, k.Usage, now) == nil {
		w.Mu.Lock()
		w.UsageErr = ""
		w.Mu.Unlock()
		return false
	}
	w.Mu.Lock()
	last := w.lastUsage
	w.Mu.Unlock()
	retry := min(k.Usage.EffectiveEvery(), w.usageRetryFloor())
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *Watcher) providerUsageNeedsRefresh(name string, u config.Usage, now time.Time) bool {
	var cached capacity.UsageReading
	if found, err := w.Host.Store().ReadFile(Cmd.UsageProviderFile(name), &cached); err == nil && found &&
		!w.Host.UseCodexRolloutUsage(u, cached) &&
		capacity.UsageFresh(cached, u, now) && capacity.ValidateReadingAt(cached, u, now) == nil {
		w.Mu.Lock()
		delete(w.ProviderUsageErr, name)
		w.Mu.Unlock()
		return false
	}
	w.Mu.Lock()
	last := w.providerLastUsage[name]
	w.Mu.Unlock()
	retry := min(u.EffectiveEvery(), w.usageRetryFloor())
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *Watcher) usageRetryFloor() time.Duration {
	if w.Host.Cfg() != nil {
		return max(w.Host.Cfg().Watch.Interval.Duration, time.Second)
	}
	return time.Second
}

// Providers are read sequentially; allow each its configured command deadline.
func UsageRefreshTimeout(k config.Capacity) time.Duration {
	var timeout time.Duration
	for _, p := range k.Providers {
		if p.Enabled() {
			timeout += p.EffectiveTimeout()
		}
	}
	return max(timeout, k.Usage.EffectiveTimeout())
}

func (w *Watcher) UsageNow() time.Time {
	if w.Clock != nil || w.Polls.Load() > 0 || w.Host.Now().IsZero() {
		return w.Clk().Now()
	}
	return w.Host.Now()
}

// ExecUsageNow runs in the command, which holds what it needs (watchExecUsageNow).
func (w *Watcher) ExecUsageNow(ctx context.Context, file string, u config.Usage) (capacity.UsageReading, error) {
	return Cmd.WatchExecUsageNow(w, ctx, file, u)
}

// RefreshUsage stamps every provider read with its own observation time: a
// slow earlier provider must not age the schedule of the next one.
func (w *Watcher) RefreshUsage(ctx context.Context, k config.Capacity) {
	if len(k.Providers) > 0 {
		for _, name := range k.ProviderNames() {
			now := w.UsageNow()
			p := k.Providers[name]
			if !p.Enabled() || !w.providerUsageNeedsRefresh(name, p.Usage, now) {
				continue
			}
			r, err := w.ExecUsageNow(ctx, Cmd.UsageProviderFile(name), p.Usage)
			if err != nil && isProbeInProgress(err.Error()) || isProbeInProgress(r.Fallback) {
				continue
			}
			w.Mu.Lock()
			if w.providerLastUsage == nil {
				w.providerLastUsage = map[string]time.Time{}
			}
			w.providerLastUsage[name] = now
			if w.ProviderUsageErr == nil {
				w.ProviderUsageErr = map[string]string{}
			}
			if err != nil {
				w.ProviderUsageErr[name] = err.Error()
				w.Mu.Unlock()
				w.Host.RetainUsageStop(Cmd.UsageProviderFile(name), err)
				continue
			}
			w.ProviderUsageErr[name] = r.Fallback
			w.Mu.Unlock()
		}
		return
	}
	now := w.UsageNow()
	r, err := w.ExecUsageNow(ctx, usageFile, k.Usage)
	if err != nil && isProbeInProgress(err.Error()) || isProbeInProgress(r.Fallback) {
		return
	}
	w.Mu.Lock()
	w.lastUsage = now
	if err != nil {
		w.UsageErr = err.Error()
		w.Mu.Unlock()
		w.Host.RetainUsageStop(usageFile, err)
		return
	}
	w.UsageErr = r.Fallback
	w.Mu.Unlock()
}

func (w *Watcher) CurrentUsage(k config.Capacity, now time.Time) (config.Capacity, *capacity.UsageView) {
	if len(k.Providers) > 0 {
		w.Mu.Lock()
		errs := maps.Clone(w.ProviderUsageErr)
		w.Mu.Unlock()
		return w.Host.CachedProvidersCapacityAt(k, now, errs)
	}
	if !k.Usage.Enabled() {
		return k, nil
	}
	w.Mu.Lock()
	uerr := w.UsageErr
	w.Mu.Unlock()
	var cause error
	if uerr != "" {
		cause = errors.New(uerr)
	}
	r, err := w.Host.CachedUsageReading(usageFile, k.Usage, now, cause)
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
func (w *Watcher) reportUsageFallback(key, provider, failure string) {
	if failure == "" || isProbeInProgress(failure) {
		w.Clear(key)
		return
	}
	label := "USAGE"
	if provider != "" {
		label += " " + provider
	}
	line := w.Fail(key, "%s unreadable: %s", label, failure)
	if line != "" && w.Chores && w.Host.Store() != nil {
		detail := fmt.Sprintf("[%s %s] usage reader failure%s%s", key, w.Host.Now().UTC().Format(time.RFC3339Nano), loop.FileReportSeparator, line)
		if err := w.Host.Store().Record(state.Event{At: w.Host.Now(), By: WatchParty, Verb: loop.LoopFileVerb, Detail: detail}); err != nil {
			w.Fail(key+"-guide", "cannot retain usage failure for guide: %v", err)
		}
	}
}

// WatchUsage refreshes subscription usage when due, prints USAGE lines, tracks
// USAGE HIGH / ENDED USAGE HIGH and tells live busy workers once per USAGE HIGH
// episode to finish their step and park.
func (w *Watcher) WatchUsage(ctx context.Context, st *state.State, sessions []*claude.Session) *capacity.UsageView {
	k := w.Host.LiveCapacity()
	if len(k.Providers) > 0 {
		return w.WatchProvidersUsage(ctx, k, st, sessions)
	}
	if !k.Usage.Enabled() {
		w.Clear("usage-error")
		w.Clear("usage-high")
		w.Clear("usage-underused")
		w.clearUsageHighEpisode(usageHighFile)
		return nil
	}
	if !w.Chores {
		_, u := w.Host.ResolveUsage(ctx, k)
		if u == nil {
			return nil
		}
		if u.Error != "" {
			w.reportUsageFallback("usage-error", "", u.Error)
			return u
		}
		w.reportUsageFallback("usage-error", "", u.Fallback)
		w.EmitNow("usage", "USAGE: %s", u.WatchLine(w.Host.Now()))
		w.Check("usage-high", u.High, "%s", u.HighWatchLine(w.Host.Now()))
		w.checkUsageUnderused("usage-underused", usageUnderusedFile, "", u.SevenDay, k.Usage)
		return u
	}
	now := w.UsageNow()
	w.Host.SetNow(now)
	if w.usageNeedsRefresh(k, now) {
		w.InFlight(ctx, k.Usage.EffectiveTimeout(), &w.usageBusy, func(ctx context.Context) {
			w.RefreshUsage(ctx, k)
		})
		now = w.UsageNow()
		w.Host.SetNow(now)
	}
	_, u := w.CurrentUsage(k, w.Host.Now())
	if u == nil {
		return nil
	}
	if u.Error != "" {
		w.reportUsageFallback("usage-error", "", u.Error)
		return u
	}
	w.reportUsageFallback("usage-error", "", u.Fallback)
	if rep := k.Usage.ReportEvery.Duration; rep > 0 {
		w.Mu.Lock()
		due := w.UsageReported.IsZero() || w.Host.Now().Sub(w.UsageReported) >= rep
		if due {
			w.UsageReported = w.Host.Now()
			w.Dirty = true
		}
		w.Mu.Unlock()
		if due {
			w.EmitNow("usage", "USAGE: %s", u.WatchLine(w.Host.Now()))
		}
	}
	w.Check("usage-high", u.High, "%s", u.HighWatchLine(w.Host.Now()))
	w.checkUsageUnderused("usage-underused", usageUnderusedFile, "", u.SevenDay, k.Usage)
	if !u.High {
		w.clearUsageHighEpisode(usageHighFile)
		return u
	}
	if st.Stop == nil {
		w.TellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighFile, nil)
	}
	return u
}

func (w *Watcher) WatchProvidersUsage(ctx context.Context, k config.Capacity, st *state.State, sessions []*claude.Session) *capacity.UsageView {
	w.Host.SyncAgyQuotaFromState(st, w.Host.Now())
	readings := map[string]capacity.ProviderReadingResult{}
	if !w.Chores {
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			r, err := w.Host.ResolveReading(ctx, Cmd.UsageProviderFile(name), p.Usage)
			if err != nil {
				w.Mu.Lock()
				if w.ProviderUsageErr == nil {
					w.ProviderUsageErr = map[string]string{}
				}
				w.ProviderUsageErr[name] = err.Error()
				w.Mu.Unlock()
				readings[name] = capacity.ProviderReadingResult{Err: err}
				continue
			}
			readings[name] = capacity.ProviderReadingResult{Reading: r, OK: true}
		}
	} else {
		now := w.UsageNow()
		w.Host.SetNow(now)
		if w.usageNeedsRefresh(k, now) {
			w.InFlight(ctx, UsageRefreshTimeout(k), &w.usageBusy, func(ctx context.Context) {
				w.RefreshUsage(ctx, k)
			})
			now = w.UsageNow()
			w.Host.SetNow(now)
		}
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			w.Mu.Lock()
			uerr := w.ProviderUsageErr[name]
			w.Mu.Unlock()
			var cause error
			if uerr != "" {
				cause = errors.New(uerr)
			}
			r, err := w.Host.CachedUsageReading(Cmd.UsageProviderFile(name), p.Usage, w.Host.Now(), cause)
			readings[name] = capacity.ProviderReadingResult{Reading: r, Err: err, OK: err == nil}
		}
	}

	c := Cmd.CountAgents(st, sessions, w.Host.Now(), w.Host.Cfg())
	pa := Cmd.CountProviderAgents(st, sessions, k, w.Host.Now(), w.Host.Cfg())
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.SelfHosted() {
			continue
		}
		// A self-hosted provider reads no subscription usage; its ceiling may
		// follow the serving mode of the model it serves, and the modes a
		// node constraint holds back lower it before any switch ran.
		readings[name] = w.Host.ServeModeReadings(ctx, name, p)
	}
	w.Mu.Lock()
	swap := w.swap
	w.Mu.Unlock()
	var h *capacity.Headroom
	if w.ReadHeadroom != nil {
		h = w.ReadHeadroom(ctx, swap)
	} else {
		h = w.Host.ReadHeadroom(ctx, k, swap)
	}
	Cmd.WithPaceOffs(readings, st, w.Host.Now())
	v := Cmd.CapacityStopped(capacity.BuildProvidersCapacity(c, pa, k, readings, h, false, nil, capacity.ForWorker, w.Host.Now()), w.Host, st)

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
				w.Fail("serve-mode-error-"+name, "SERVE MODE %s unreadable: %s", name, pv.ServingModeError)
			} else {
				w.Clear("serve-mode-error-" + name)
			}
			parts = append(parts, name+": "+pv.WatchSummary(w.Host.Now()))
			continue
		}
		if pv.Error != "" {
			w.reportUsageFallback("usage-error-"+name, name, pv.Error)
		} else {
			w.reportUsageFallback("usage-error-"+name, name, pv.Fallback)
		}
		parts = append(parts, name+": "+pv.WatchSummary(w.Host.Now()))
		if len(pv.Buckets) > 0 && !pv.SupplementalBuckets {
			for _, bName := range pv.BucketOrder {
				bv := pv.Buckets[bName]
				if bv == nil {
					continue
				}
				target := name + "/" + bName
				highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
					target, capacity.HighFigureText(bv.FiveHour, pv.HighPercent, w.Host.Now()), target)
				w.Check("usage-high-"+name+"-"+bName, bv.High, "%s", highLine)
				w.checkUsageUnderused("usage-underused-"+name+"-"+bName, usageUnderusedTargetFile(target), target, bv.SevenDay, p.Usage)
				ledgerFile := Cmd.UsageHighBucketFile(name, bName)
				if !bv.High {
					allHigh = false
					w.clearUsageHighEpisode(ledgerFile)
				} else if st.Stop == nil {
					u := &capacity.UsageView{FiveHour: bv.FiveHour, High: true, HighPercent: pv.HighPercent}
					w.TellBusyWorkersUsageHigh(ctx, st, sessions, u, ledgerFile, func(ag state.Agent) bool {
						return slices.Contains(bv.Busy, ag.Name)
					})
				}
			}
			continue
		}
		highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
			name, capacity.HighFigureText(pv.FiveHour, pv.HighPercent, w.Host.Now()), name)
		w.Check("usage-high-"+name, pv.High, "%s", highLine)
		w.checkUsageUnderused("usage-underused-"+name, usageUnderusedTargetFile(name), name, pv.SevenDay, p.Usage)
		if !pv.High {
			allHigh = false
			w.clearUsageHighEpisode(Cmd.UsageHighProviderFile(name))
		} else if st.Stop == nil {
			u := &capacity.UsageView{FiveHour: pv.FiveHour, High: true, HighPercent: pv.HighPercent}
			w.TellBusyWorkersUsageHigh(ctx, st, sessions, u, Cmd.UsageHighProviderFile(name), func(ag state.Agent) bool {
				return slices.Contains(pv.Busy, ag.Name)
			})
		}
	}
	if v.Choice != nil {
		parts = append(parts, "choice: "+v.Choice.Summary())
	}
	shouldEmit := !w.Chores
	if w.Chores {
		switch {
		case !k.Usage.ReportEverySet():
			shouldEmit = true
		case k.Usage.ReportEvery.Duration > 0:
			w.Mu.Lock()
			due := w.UsageReported.IsZero() || w.Host.Now().Sub(w.UsageReported) >= k.Usage.ReportEvery.Duration
			if due {
				w.UsageReported = w.Host.Now()
				w.Dirty = true
			}
			w.Mu.Unlock()
			shouldEmit = due
		}
	}
	if shouldEmit && len(parts) > 0 {
		w.EmitNow("usage", "USAGE: %s", strings.Join(parts, "; "))
	}
	return &capacity.UsageView{Ceiling: v.Ceiling, High: allHigh, Underused: anyUnderused}
}

func (w *Watcher) clearUsageHighEpisode(file string) {
	if w.Host.Store() == nil {
		return
	}
	var l UsageHighLedger
	if found, err := w.Host.Store().ReadFile(file, &l); err == nil && found && l.Episode != "" {
		_ = w.Host.Store().WriteFile(file, UsageHighLedger{})
	}
}

// TellBusyWorkersUsageHigh runs in the command, which holds what it needs (watchTellBusyWorkersUsageHigh).
func (w *Watcher) TellBusyWorkersUsageHigh(ctx context.Context, st *state.State, sessions []*claude.Session, u *capacity.UsageView, ledgerFile string, match func(state.Agent) bool) {
	Cmd.WatchTellBusyWorkersUsageHigh(w, ctx, st, sessions, u, ledgerFile, match)
}

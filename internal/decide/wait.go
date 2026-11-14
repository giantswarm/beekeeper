package decide

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
)

// Overdue re-observes start one minute after the deadline's re-observe and double from there.
const overdueInitial = time.Minute

// want is the wait an object should hold now.
type want struct {
	On, Ref    string
	Deadline   time.Duration
	AtDeadline string
}

// waitCtx is what the wait lifecycle needs from the view.
type waitCtx struct {
	Now       time.Time
	Trigger   Trigger
	Source    string
	Reobserve *Reobserve
	Spec      *v1alpha1.InstallationSpec
	DropWaits bool
	// SubjectChanged is true when what the wait waits on changed although the wait goes on (e.g. one
	// more approval of two).
	SubjectChanged bool
}

type waitResult struct {
	Wait   *v1alpha1.Wait
	Events []history.Event
	// NextCheck is when the object must be reconciled for its wait; nil without a wait.
	NextCheck *time.Time
}

// wakeTrigger reports whether a trigger can wake a wait (the wake sources of engine_wait_wakes_total).
func wakeTrigger(t Trigger) bool {
	switch t {
	case TriggerWebhook, TriggerResync, TriggerDeadline, TriggerPoll, TriggerRetry:
		return true
	}
	return false
}

// obsSource is the source of the rows a reconcile records (wait.deadline aside).
func obsSource(t Trigger, source string) string {
	switch t {
	case TriggerDeadline, TriggerPoll, TriggerRetry:
		return v1alpha1.SourceReobserve
	}
	if source != "" {
		return source
	}
	switch t {
	case TriggerWebhook:
		return v1alpha1.SourceWebhook
	case TriggerResync:
		return v1alpha1.SourceResync
	case TriggerStartup:
		return v1alpha1.SourceStartup
	}
	return v1alpha1.SourceEngine
}

func waitData(w *v1alpha1.Wait) map[string]any {
	d := map[string]any{
		"on":       w.On,
		"ref":      w.Ref,
		"deadline": w.Deadline.UTC().Format(time.RFC3339),
	}
	if w.Reason != "" {
		d["reason"] = w.Reason
	}
	return d
}

// unobservedReason names why a re-observe failed: budget (rate limit or reserve), http-<code>, or
// source-degraded when no read could be made.
func unobservedReason(re *Reobserve) string {
	if re.Err != nil {
		switch re.Err.Kind {
		case forge.Deferred, forge.RateLimited, forge.SecondaryRateLimited:
			return v1alpha1.WaitReasonBudget
		}
		if re.Err.Code != 0 {
			return fmt.Sprintf("%s%d", v1alpha1.WaitReasonHTTPPrefix, re.Err.Code)
		}
		return v1alpha1.WaitReasonHTTPPrefix + "timeout"
	}
	if re.Reason != "" {
		return re.Reason
	}
	return v1alpha1.WaitReasonSourceDegraded
}

func overdueCap(spec *v1alpha1.InstallationSpec) time.Duration {
	if spec != nil && spec.Waits.OverdueBackoffCap.Duration > 0 {
		return spec.Waits.OverdueBackoffCap.Duration
	}
	return 10 * time.Minute
}

func retryBounds(spec *v1alpha1.InstallationSpec) (initial, maxBackoff time.Duration) {
	initial, maxBackoff = 10*time.Second, 5*time.Minute
	if spec != nil {
		if spec.Retry.Initial.Duration > 0 {
			initial = spec.Retry.Initial.Duration
		}
		if spec.Retry.Cap.Duration > 0 {
			maxBackoff = spec.Retry.Cap.Duration
		}
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}
	return initial, maxBackoff
}

func double(d *v1alpha1.Wait, initial, maxBackoff time.Duration) time.Duration {
	b := initial
	if d.Backoff != nil && d.Backoff.Duration > 0 {
		b = 2 * d.Backoff.Duration
	}
	if b > maxBackoff {
		b = maxBackoff
	}
	return b
}

func due(w *v1alpha1.Wait, now time.Time) bool {
	return w.NextCheck == nil || !now.Before(w.NextCheck.Time)
}

// lifecycle moves an object's wait from prev to what it should be now (w, nil for none) and returns
// the rows that record it. A wait is identified by on and ref: the same expectation keeps its since and
// deadline across reconciles and restarts.
func lifecycle(prev *v1alpha1.Wait, w *want, c waitCtx) waitResult {
	var res waitResult
	src := obsSource(c.Trigger, c.Source)
	add := func(typ, source string, wt *v1alpha1.Wait) {
		res.Events = append(res.Events, event(c.Now, typ, source, waitData(wt)))
	}

	re := c.Reobserve
	if re == nil && (c.Trigger == TriggerDeadline || c.Trigger == TriggerPoll || c.Trigger == TriggerRetry) {
		// A timer fired without reads of its own: what was read for the reconcile is the re-observe.
		re = &Reobserve{Made: true, Changed: c.SubjectChanged}
	}
	reOK := re != nil && re.Made && re.Err == nil
	reFailed := re != nil && !reOK
	same := prev != nil && w != nil && prev.On == w.On && prev.Ref == w.Ref

	if prev != nil {
		cur := prev.DeepCopy()
		deadlineDue := !cur.DeadlineHandled && !c.Now.Before(cur.Deadline.Time)
		handledNow := false
		switch {
		case reOK && deadlineDue:
			// The deadline's single re-observe (also when it was passed while unobserved).
			add(EventWaitDeadline, v1alpha1.SourceDeadline, cur)
			add(EventWaitReobserved, v1alpha1.SourceReobserve, cur)
			cur.DeadlineHandled = true
			handledNow = true
		case reOK && (cur.State == v1alpha1.WaitUnobserved ||
			(cur.State == v1alpha1.WaitOverdue && due(cur, c.Now)) || c.Trigger == TriggerPoll):
			add(EventWaitReobserved, v1alpha1.SourceReobserve, cur)
		}

		woken := !reFailed && wakeTrigger(c.Trigger) &&
			((re != nil && re.Changed) || !same || c.SubjectChanged)
		if woken {
			add(EventWaitWoken, src, cur)
		}

		if !same {
			cur.Reason = ""
			add(EventWaitEnded, src, cur)
		} else {
			switch {
			case reFailed:
				reason := unobservedReason(re)
				initial, retryCap := retryBounds(c.Spec)
				if cur.State != v1alpha1.WaitUnobserved || cur.Reason != reason {
					cur.Reason = reason
					add(EventWaitUnobserved, src, cur)
				}
				if cur.State != v1alpha1.WaitUnobserved || cur.Backoff == nil {
					cur.Backoff = md(initial)
					cur.NextCheck = mtp(c.Now.Add(initial))
				} else if due(cur, c.Now) {
					b := double(cur, initial, retryCap)
					cur.Backoff = md(b)
					cur.NextCheck = mtp(c.Now.Add(b))
				}
				if re.Err != nil {
					if re.Err.ResetAt.After(cur.NextCheck.Time) {
						cur.NextCheck = mtp(re.Err.ResetAt)
					}
					if after := c.Now.Add(re.Err.RetryAfter); re.Err.RetryAfter > 0 && after.After(cur.NextCheck.Time) {
						cur.NextCheck = mtp(after)
					}
				}
				cur.State = v1alpha1.WaitUnobserved
			case handledNow:
				// The situation is still what the wait expects: it's overdue, once.
				cur.State = v1alpha1.WaitOverdue
				cur.Reason = ""
				cur.Backoff = md(overdueInitial)
				cur.NextCheck = mtp(c.Now.Add(overdueInitial))
				add(EventWaitOverdue, src, cur)
			case cur.State == v1alpha1.WaitUnobserved && reOK:
				cur.Reason = ""
				if cur.DeadlineHandled {
					// It was overdue before it went unobserved.
					cur.State = v1alpha1.WaitOverdue
					cur.Backoff = md(overdueInitial)
					cur.NextCheck = mtp(c.Now.Add(overdueInitial))
				} else {
					cur.State = v1alpha1.WaitWatching
					cur.Backoff = nil
					cur.NextCheck = mtp(cur.Deadline.Time)
				}
			case cur.State == v1alpha1.WaitOverdue && reOK && due(cur, c.Now):
				b := double(cur, overdueInitial, overdueCap(c.Spec))
				cur.Backoff = md(b)
				cur.NextCheck = mtp(c.Now.Add(b))
			case cur.State == v1alpha1.WaitWatching || cur.State == "":
				cur.State = v1alpha1.WaitWatching
				cur.NextCheck = mtp(cur.Deadline.Time)
			}
			if cur.NextCheck == nil {
				cur.NextCheck = mtp(cur.Deadline.Time)
			}
			res.Wait = cur
			res.NextCheck = timep(cur.NextCheck.Time)
		}
	}

	if w != nil && !same && !c.DropWaits {
		at := w.AtDeadline
		if at == "" {
			at = v1alpha1.AtDeadlineReobserve
		}
		nw := &v1alpha1.Wait{
			On:         w.On,
			Ref:        w.Ref,
			Since:      mt(c.Now),
			Deadline:   mt(c.Now.Add(w.Deadline)),
			AtDeadline: at,
			State:      v1alpha1.WaitWatching,
			NextCheck:  mtp(c.Now.Add(w.Deadline)),
		}
		add(EventWaitStarted, src, nw)
		res.Wait = nw
		res.NextCheck = timep(nw.NextCheck.Time)
	}
	return res
}

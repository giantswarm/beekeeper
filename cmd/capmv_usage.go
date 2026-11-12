package cmd

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
)

// sevenDaySpan is the duration of a 7-day subscription usage window.
const sevenDaySpan = 7 * 24 * time.Hour

// usageWindow is one subscription usage window (5-hour or 7-day).
type usageWindow struct {
	UsedPercent float64   `json:"usedPercent"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
	Pace        *float64  `json:"pace,omitempty"`
}

// usageBucket is one named bucket (such as "Gemini Models" or
// "Claude and GPT models") in a multi-bucket usage reading.
type usageBucket struct {
	FiveHour *usageWindow `json:"fiveHour,omitempty"`
	SevenDay *usageWindow `json:"sevenDay,omitempty"`
}

// usageReading is the timestamped output of a usage command stored in
// usageFile or usageProviderFile.
type usageReading struct {
	At            time.Time              `json:"at"`
	Fallback      string                 `json:"fallback,omitempty"`
	Stop          string                 `json:"stop,omitempty"`
	SourceAt      time.Time              `json:"sourceAt,omitzero"`
	AgeMinutes    *float64               `json:"ageMinutes,omitempty"`
	Stale         bool                   `json:"stale,omitempty"`
	NoPlanWindows bool                   `json:"noPlanWindows,omitempty"`
	SingleWindow  bool                   `json:"singleWindow,omitempty"`
	FiveHour      *usageWindow           `json:"fiveHour,omitempty"`
	SevenDay      *usageWindow           `json:"sevenDay,omitempty"`
	Buckets       map[string]usageBucket `json:"buckets,omitempty"`
	BucketOrder   []string               `json:"bucketOrder,omitempty"`
}

// usageView is how capacity, watch and snapshot present subscription usage and
// the target derived from it.
type usageView struct {
	Fallback    string       `json:"fallback,omitempty"`
	At          time.Time    `json:"at,omitzero"`
	FiveHour    *usageWindow `json:"fiveHour,omitempty"`
	SevenDay    *usageWindow `json:"sevenDay,omitempty"`
	Ceiling     int          `json:"ceiling"`
	High        bool         `json:"high,omitempty"`
	HighPercent float64      `json:"highPercent,omitempty"`
	Underused   bool         `json:"underused,omitempty"`
	Error       string       `json:"error,omitempty"`
}

func validateReadingAt(r usageReading, u config.Usage, now time.Time) error {
	if r.Stop != "" {
		return codex.UsageStoppedError(r.Stop)
	}
	maxAge := u.EffectiveStaleAfter()
	if r.Stale {
		return errors.New("usage reading is stale")
	}
	if r.AgeMinutes != nil {
		age := time.Duration(*r.AgeMinutes * float64(time.Minute))
		// AgeMinutes describes the source at fetch time. Cached reads must
		// include the time since that fetch, even without an absolute SourceAt.
		if !r.At.IsZero() && !now.IsZero() {
			age += max(0, now.Sub(r.At))
		}
		if age > maxAge {
			return fmt.Errorf("usage reading is stale (%s old, max %s)", dur(age), dur(maxAge))
		}
	}
	if !r.SourceAt.IsZero() && !now.IsZero() {
		if r.SourceAt.After(now.Add(time.Minute)) {
			return fmt.Errorf("usage reading timestamp %s is in the future", r.SourceAt.Format(time.RFC3339))
		}
		// Codex rollout events advance only while a model runs. Their age
		// alone must not deadlock starts; the built-in reader probes the
		// account and labels its last-reading fallback when unavailable.
		if now.Sub(r.SourceAt) > maxAge && !slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
			return fmt.Errorf("usage reading is stale (%s old, max %s)", dur(now.Sub(r.SourceAt)), dur(maxAge))
		}
	}
	if !r.At.IsZero() && !now.IsZero() {
		if r.At.After(now.Add(time.Minute)) {
			return fmt.Errorf("usage reading timestamp %s is in the future", r.At.Format(time.RFC3339))
		}
		if now.Sub(r.At) > maxAge {
			return fmt.Errorf("usage reading is stale (%s old, max %s)", dur(now.Sub(r.At)), dur(maxAge))
		}
	}
	if r.NoPlanWindows {
		return nil
	}
	if len(r.Buckets) > 0 && r.FiveHour == nil && r.SevenDay == nil {
		for _, bName := range bucketNames(r) {
			b := r.Buckets[bName]
			switch {
			case b.FiveHour == nil && b.SevenDay == nil:
				return fmt.Errorf("bucket %q has neither fiveHour nor sevenDay", bName)
			case len(u.FiveHour) > 0 && b.FiveHour == nil:
				return fmt.Errorf("bucket %q omits fiveHour (fiveHour is configured)", bName)
			case u.HighPercentSet() && b.FiveHour == nil:
				return fmt.Errorf("bucket %q omits fiveHour (highPercent is configured)", bName)
			case len(u.SevenDay) > 0 && b.SevenDay == nil:
				return fmt.Errorf("bucket %q omits sevenDay (sevenDay is configured)", bName)
			}
		}
		return nil
	}
	switch {
	case r.FiveHour == nil && r.SevenDay == nil:
		return errors.New("usage output has neither fiveHour nor sevenDay")
	case len(u.FiveHour) > 0 && r.FiveHour == nil && !r.SingleWindow:
		return errors.New("usage output omits fiveHour (capacity.usage.fiveHour is configured)")
	case u.HighPercentSet() && r.FiveHour == nil && !r.SingleWindow:
		return errors.New("usage output omits fiveHour (capacity.usage.highPercent is configured)")
	case len(u.SevenDay) > 0 && r.SevenDay == nil:
		return errors.New("usage output omits sevenDay (capacity.usage.sevenDay is configured)")
	}
	return nil
}

func bucketNames(r usageReading) []string {
	if len(r.Buckets) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(r.Buckets))
	var out []string
	for _, b := range r.BucketOrder {
		if _, ok := r.Buckets[b]; ok && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, b := range slices.Sorted(maps.Keys(r.Buckets)) {
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// usageFresh reports whether r is fresh at now: within u.EffectiveEvery(), not
// stale, and no window crossed its ResetsAt since r.At.
func usageFresh(r usageReading, u config.Usage, now time.Time) bool {
	if r.At.IsZero() || now.Sub(r.At) > u.EffectiveEvery() || r.At.After(now.Add(time.Minute)) {
		return false
	}
	if validateReadingAt(r, u, now) != nil {
		return false
	}
	crossed := func(w *usageWindow) bool {
		return w != nil && !w.ResetsAt.IsZero() && r.At.Before(w.ResetsAt) && !now.Before(w.ResetsAt)
	}
	if crossed(r.FiveHour) || crossed(r.SevenDay) {
		return false
	}
	for _, b := range r.Buckets {
		if crossed(b.FiveHour) || crossed(b.SevenDay) {
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
	return &usageWindow{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt, Pace: w.Pace}
}

// effectiveSevenDayWindow returns the 7-day window at now with its elapsed
// weekly pace share in percent attached.
func effectiveSevenDayWindow(w *usageWindow, now time.Time) *usageWindow {
	ew := effectiveWindow(w, now)
	if ew == nil {
		return nil
	}
	ew.Pace = windowPace(ew, now)
	return ew
}

// windowPace returns the 7-day window's elapsed share in percent [0, 100] at now,
// or w.Pace when explicitly supplied in JSON, or nil when resetsAt is unset.
func windowPace(w *usageWindow, now time.Time) *float64 {
	if w == nil {
		return nil
	}
	if w.Pace != nil {
		v := math.Round(max(0, min(100, *w.Pace))*10) / 10
		return &v
	}
	if w.ResetsAt.IsZero() || !now.Before(w.ResetsAt) {
		return nil
	}
	rem := w.ResetsAt.Sub(now)
	elapsed := sevenDaySpan - rem
	v := max(0, min(100, float64(elapsed)/float64(sevenDaySpan)*100))
	v = math.Round(v*10) / 10
	return &v
}

func sdPace(w *usageWindow) *float64 {
	if w == nil {
		return nil
	}
	return w.Pace
}

func pctPtr(w *usageWindow) *float64 {
	if w == nil {
		return nil
	}
	v := w.UsedPercent
	return &v
}

func deriveUsageView(r usageReading, k config.Capacity, now time.Time) (config.Capacity, *usageView) {
	return deriveUsageViewForStart(r, k, forWorker, now)
}

func deriveUsageViewForStart(r usageReading, k config.Capacity, kind startFor, now time.Time) (config.Capacity, *usageView) {
	fh := effectiveWindow(r.FiveHour, now)
	sd := effectiveSevenDayWindow(r.SevenDay, now)
	var pace *float64
	if kind == forWorker {
		pace = sdPace(sd)
	}
	derived, high := k.DeriveWithPace(pctPtr(fh), pctPtr(sd), pace)
	return derived, &usageView{
		At:          r.At,
		Fallback:    r.Fallback,
		FiveHour:    fh,
		SevenDay:    sd,
		Ceiling:     derived.Ceiling,
		High:        high,
		HighPercent: k.Usage.EffectiveHighPercent(),
		Underused:   windowUnderused(sd, k.Usage, now),
	}
}

func errorUsageView(k config.Capacity, err error) (config.Capacity, *usageView) {
	derived := k
	derived.Ceiling = 0
	return derived, &usageView{
		Ceiling:     0,
		HighPercent: k.Usage.EffectiveHighPercent(),
		Error:       err.Error(),
	}
}

// usageUnreadable labels Codex failures that retain windows or the ceiling.
const usageUnreadable = "usage unreadable; "

const claudeNoPlanWindows = "no plan windows"

func windowFigure(label string, w *usageWindow, now time.Time) string {
	s := fmt.Sprintf("%s %s", label, pct(w.UsedPercent))
	if !w.ResetsAt.IsZero() && now.Before(w.ResetsAt) {
		if w.Pace != nil {
			s += fmt.Sprintf(" (resets %s, in %s, pace %s)", clock(now.Local(), w.ResetsAt), dur(w.ResetsAt.Sub(now)), pct(*w.Pace))
		} else {
			s += fmt.Sprintf(" (resets %s, in %s)", clock(now.Local(), w.ResetsAt), dur(w.ResetsAt.Sub(now)))
		}
	} else if w.Pace != nil {
		s += fmt.Sprintf(" (pace %s)", pct(*w.Pace))
	}
	return s
}

func windowsPairLine(fh, sd *usageWindow, now time.Time) string {
	var parts []string
	if fh != nil {
		parts = append(parts, windowFigure("5h", fh, now))
	}
	if sd != nil {
		parts = append(parts, windowFigure("7d", sd, now))
	}
	return strings.Join(parts, ", ")
}

func (u *usageView) windowsLine(now time.Time) string {
	return withFallback(windowsPairLine(u.FiveHour, u.SevenDay, now), u.Fallback, u.At, now)
}

func withFallback(s, fallback string, at, now time.Time) string {
	switch {
	case fallback == "":
		return s
	case at.IsZero() && s == "":
		return truncate(fallback, 120)
	case at.IsZero():
		return s + "; " + truncate(fallback, 120)
	}
	return s + fmt.Sprintf("; last good read %s ago (%s)", dur(now.Sub(at)), truncate(fallback, 120))
}

func (u *usageView) capacitySuffix(now time.Time) string {
	if u.Error != "" {
		return "usage unknown (" + truncate(u.Error, 120) + ")"
	}
	line := u.windowsLine(now)
	if strings.HasPrefix(line, usageUnreadable) {
		return line
	}
	return "usage " + line
}

func (u *usageView) watchLine(now time.Time) string {
	s := fmt.Sprintf("%s; target ceiling %d", u.windowsLine(now), u.Ceiling)
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

func highFigureText(fh *usageWindow, highPct float64, now time.Time) string {
	used := highPct
	var reset time.Time
	if fh != nil {
		used, reset = fh.UsedPercent, fh.ResetsAt
	}
	s := fmt.Sprintf("5h %s at or past %s", pct(used), pct(highPct))
	if !reset.IsZero() && now.Before(reset) {
		s += fmt.Sprintf(" (resets %s, in %s)", clock(now.Local(), reset), dur(reset.Sub(now)))
	}
	return s
}

func highBlockText(fh *usageWindow, highPct float64, now time.Time) string {
	return "usage high: " + highFigureText(fh, highPct, now)
}

func (u *usageView) highFigure(now time.Time) string {
	return highFigureText(u.FiveHour, u.HighPercent, now)
}

func (u *usageView) highBlock(now time.Time) string {
	return "usage high: " + u.highFigure(now)
}

func (u *usageView) highWatchLine(now time.Time) string {
	return "USAGE HIGH: " + u.highFigure(now) + "; new starts and resumes wait until reset, telling busy workers to finish their step and park"
}

func underusedFigureText(sd *usageWindow, u config.Usage, now time.Time) string {
	if sd == nil || sd.Pace == nil {
		return ""
	}
	behind := math.Round(max(0, *sd.Pace-sd.UsedPercent)*10) / 10
	s := fmt.Sprintf("7d %s is %s behind pace %s (below %s of pace",
		pct(sd.UsedPercent), pct(behind), pct(*sd.Pace), pct(math.Round(u.EffectiveUnderuseRatio()*1000)/10))
	if !sd.ResetsAt.IsZero() && now.Before(sd.ResetsAt) {
		s += fmt.Sprintf(", resets %s, in %s", clock(now.Local(), sd.ResetsAt), dur(sd.ResetsAt.Sub(now)))
	}
	return s + ")"
}

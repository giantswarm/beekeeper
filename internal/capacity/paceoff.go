package capacity

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// paceOffSlack is how far beyond a week from a pace override's set a 7-day
// window may reset and still be the window that was current at the set: the
// usage readers report a reset to the minute or the hour.
const paceOffSlack = time.Hour

// PaceOffView is a provider's pace override (capacity pace off) as capacity
// weighs it: a worker start on the provider is neither refused for running
// ahead of the even pace of its 7-day window nor capped by its paceCaps, until
// Until and within the 7-day window that was current when it was set. The
// weekly caps (sevenDayMax, a choice's workerSevenDayMax, 100%) and the 5-hour
// window still bound it.
type PaceOffView struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
	// Ended says why the override no longer lifts the pace: the provider's
	// 7-day window started anew since it was set, a usage reset used or the
	// week over.
	Ended string `json:"ended,omitempty"`
}

// PaceOffEnded is a pace override's end by a new 7-day window.
const PaceOffEnded = "the 7-day window started anew"

// NewPaceOffView is p as capacity weighs it, nil for none.
func NewPaceOffView(p *state.PaceOff) *PaceOffView {
	if p == nil {
		return nil
	}
	return &PaceOffView{Until: p.Until, Reason: p.Reason, By: p.By.Name, At: p.At}
}

// waives reports whether p lifts the pace of sd, a 7-day window of its
// provider: p is set and sd is the window that was current at its set.
func (p *PaceOffView) waives(sd *UsageWindow) bool {
	return p != nil && !windowStartedSince(sd, p.At)
}

// windowStartedSince reports whether the 7-day window sd began after at: it
// resets more than a week after at.
func windowStartedSince(sd *UsageWindow, at time.Time) bool {
	return sd != nil && !sd.ResetsAt.IsZero() && sd.ResetsAt.Sub(at) > sevenDaySpan+paceOffSlack
}

// settle marks p ended once every 7-day window of its provider (windows)
// started anew since its set; a provider without a window keeps it.
func (p *PaceOffView) settle(windows ...*UsageWindow) *PaceOffView {
	if p == nil {
		return nil
	}
	seen := false
	for _, w := range windows {
		if w == nil {
			continue
		}
		if !windowStartedSince(w, p.At) {
			return p
		}
		seen = true
	}
	if seen {
		p.Ended = PaceOffEnded
	}
	return p
}

// Line says the override on its provider's line: until when and why, or
// that it ended.
func (p *PaceOffView) Line(now time.Time) string {
	if p.Ended != "" {
		return "pace off ended: " + p.Ended
	}
	return fmt.Sprintf("pace off until %s: %s", Cmd.Clock(now.Local(), p.Until), p.Reason)
}

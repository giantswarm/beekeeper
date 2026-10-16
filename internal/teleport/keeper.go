package teleport

import (
	"errors"
	"fmt"
	"time"
)

// Record is what the keeper last did, kept in the state directory between
// its runs.
type Record struct {
	// RenewedAt is the last renewal's time, ValidUntil the expiry it got.
	RenewedAt  time.Time `json:"renewedAt,omitzero"`
	ValidUntil time.Time `json:"validUntil,omitzero"`
	// FailedAt is the last failed renewal, FailedFor the expiry of the
	// profile it was to replace (zero: none was active), Reason why.
	FailedAt  time.Time `json:"failedAt,omitzero"`
	FailedFor time.Time `json:"failedFor,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	// AfterMiss marks a failed renewal of a missed tick's run: the keeper
	// tries that login once more at its next run, since the network may
	// not have been back from the suspend yet.
	AfterMiss bool `json:"afterMiss,omitempty"`
	// Waiting is why the keeper's last run that was due did not renew (the
	// browser lease is held), WaitingAt when.
	Waiting   string    `json:"waiting,omitempty"`
	WaitingAt time.Time `json:"waitingAt,omitzero"`
	// CheckedAt is the keeper's last run.
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// Missed reports whether the keeper's run at now follows a missed tick:
// more than two intervals every passed since its last run, as after a
// suspend or a powered-off machine.
func (r Record) Missed(now time.Time, every time.Duration) bool {
	return !r.CheckedAt.IsZero() && now.Sub(r.CheckedAt) > 2*every
}

// Failed reports whether the record's failed renewal was for the profile p
// (read with perr): the keeper does not try that login again, a person
// does.
func (r Record) Failed(p Profile, perr error) bool {
	if r.FailedAt.IsZero() {
		return false
	}
	if errors.Is(perr, ErrNotLoggedIn) {
		return r.FailedFor.IsZero()
	}
	return perr == nil && r.FailedFor.Equal(p.ValidUntil)
}

// Due decides whether the keeper renews now: once less than before is
// left on the active profile, or none is active, unless a renewal of that
// very profile failed already. A run after a missed tick (missed) and the
// run after a missed tick's failed renewal try that profile again. The
// reason says why not.
func Due(p Profile, perr error, r Record, now time.Time, before time.Duration, missed bool) (bool, string) {
	switch {
	case perr != nil && !errors.Is(perr, ErrNotLoggedIn):
		return false, perr.Error()
	case r.Failed(p, perr) && !missed && !r.AfterMiss:
		return false, fmt.Sprintf("the renewal at %s failed (%s): a person renews it (beekeeper teleport renew)", r.FailedAt.Local().Format(time.TimeOnly), r.Reason)
	case perr != nil:
		return true, ""
	case p.Left(now) >= before:
		return false, fmt.Sprintf("valid until %s, renewed from %s", p.ValidUntil.Local().Format(time.TimeOnly), p.ValidUntil.Add(-before).Local().Format(time.TimeOnly))
	}
	return true, ""
}

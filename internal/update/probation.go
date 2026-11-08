package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ProbationWindow is how long a watched candidate is observed. A failure
	// restores the recovery target at once, so a broken release is gone within it.
	ProbationWindow = 30 * time.Minute
	// HeartbeatGrace is how long a candidate's loop may stay silent before the
	// silence counts as a failure. It counts from the candidate loop's start: a
	// replaced loop re-executes only at a quiet pass, which can come late.
	HeartbeatGrace = 10 * time.Minute
	// ProbeFailures is how many consecutive failed probes fail a candidate.
	ProbeFailures = 3
)

// ErrProbationWatched is an update asked for while a recovery controller
// supervises the installed candidate: its verdict comes first.
var ErrProbationWatched = errors.New("a recovery controller supervises the installed candidate's probation")

// ErrUpdateLocked is an installation another updater holds.
var ErrUpdateLocked = errors.New("update lock unavailable")

// errModifiedRecovery is a recovery binary that no longer matches its digest.
var errModifiedRecovery = errors.New("modified recovery binary")

// Observed is a controller's durable record of one probation. Elapsed counts
// observed time, tick by tick: a machine that slept proves nothing and fails
// nothing while it slept.
type Observed struct {
	Elapsed       time.Duration `json:"elapsed"`
	ProbePassed   bool          `json:"probePassed,omitempty"`
	ProbeFailures int           `json:"probeFailures,omitempty"`
	// ProbeFailure is the bounded cause of the latest counted candidate failure.
	ProbeFailure string `json:"probeFailure,omitempty"`
	// ProbedAt is the Elapsed of the last probe; LastPassAt is the Elapsed of
	// the last passed probe, kept as history when a later probe is inconclusive.
	ProbedAt   time.Duration `json:"probedAt,omitempty"`
	LastPassAt time.Duration `json:"lastPassAt,omitempty"`
	// Held is why recovery itself failed: updates and starts stay held.
	Held string `json:"held,omitempty"`
	// Reason is why the candidate was rolled back.
	Reason string `json:"reason,omitempty"`
	// Notified says the person's one line about the rollback is out.
	Notified bool `json:"notified,omitempty"`
	// Restarts counts the ticks spent moving processes off the failed candidate;
	// Restarted says none is left on it.
	Restarts  int  `json:"restarts,omitempty"`
	Restarted bool `json:"restarted,omitempty"`
	// RollbackFailures counts the failed attempts at a rollback.
	RollbackFailures int `json:"rollbackFailures,omitempty"`
	// LoopStarted says the candidate's own loop was seen running; LoopStartedAt
	// is the Elapsed at which it started, the start of its heartbeat grace.
	LoopStarted   bool          `json:"loopStarted,omitempty"`
	LoopStartedAt time.Duration `json:"loopStartedAt,omitempty"`
}

// NoteLoop records when the candidate's loop started, once, in observed time.
// The age is capped by the time observed so far; the controller caps it at one
// step, so a sleep or downtime before the first sighting shortens no grace.
func (o *Observed) NoteLoop(s Signals) {
	if o.LoopStarted || !s.LoopStarted {
		return
	}
	ago := min(max(s.LoopStartedAgo, 0), o.Elapsed)
	o.LoopStarted, o.LoopStartedAt = true, o.Elapsed-ago
}

// Signals are what one evaluation sees of the installed candidate.
type Signals struct {
	// Failure is a tripped failure streak of the candidate's merges or starts.
	Failure string
	// Intentional says a stop or hold explains the silence: nothing counts.
	Intentional bool
	// Unproven is why the evidence cannot be trusted (GitHub unavailable,
	// unreadable health). It keeps the candidate pending, never rolls it back.
	Unproven string
	// HeartbeatRequired says this machine runs a loop that must show up.
	HeartbeatRequired, Heartbeat bool
	// LoopStarted says the candidate's own loop started after the install,
	// LoopStartedAgo how long ago. LoopPending says it has not, while a loop of
	// another version still runs and re-executes at its next quiet pass.
	LoopStarted, LoopPending bool
	LoopStartedAgo           time.Duration
}

// Verdicts of Decide.
const (
	Wait     = "wait"
	Promote  = "promote"
	RollBack = "rollback"
)

// Decide is a probation's verdict after o, given s.
func Decide(o Observed, s Signals) (verdict, reason string) {
	if s.Intentional {
		return Wait, "held on purpose"
	}
	if s.Unproven != "" {
		return Wait, s.Unproven
	}
	switch {
	case s.Failure != "":
		return RollBack, s.Failure
	case o.ProbeFailures >= ProbeFailures:
		reason := fmt.Sprintf("isolated workflow probe failed %d times", o.ProbeFailures)
		if o.ProbeFailure != "" {
			reason += ": " + o.ProbeFailure
		}
		return RollBack, reason
	case s.HeartbeatRequired && !s.Heartbeat && heartbeatOverdue(o, s):
		return RollBack, "no health heartbeat from the candidate's loop"
	case o.Elapsed < ProbationWindow:
		return Wait, "in probation"
	case !o.ProbePassed:
		return Wait, "no passed probe yet"
	case s.HeartbeatRequired && !s.Heartbeat:
		return Wait, "no heartbeat yet"
	}
	return Promote, ""
}

// heartbeatOverdue says a silent candidate loop has used up its grace. A loop
// that started has HeartbeatGrace from its start; one an older loop has yet to
// hand over to has the probation window; one that never started, the grace.
func heartbeatOverdue(o Observed, s Signals) bool {
	switch {
	case o.LoopStarted:
		return o.Elapsed-o.LoopStartedAt >= HeartbeatGrace
	case s.LoopPending:
		return o.Elapsed >= ProbationWindow
	}
	return o.Elapsed >= HeartbeatGrace
}

// Inspect reads the journal of the installation at path, nil for none, without
// the update lock: a journal is replaced in one rename, and an updater holding
// the lock through a download must not stall a controller's evaluation.
func Inspect(path string) (*Journal, error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if target, err = filepath.Abs(target); err != nil {
		return nil, err
	}
	// Asking creates nothing: an installation without a directory has no journal.
	dir := target + ".update"
	if _, err := os.Lstat(dir); err != nil {
		if dir, err = externalRecoveryDir(target); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(dir); err != nil {
			return nil, nil //nolint:nilnil // no journal is an answer
		}
	}
	i := &installation{target: target, dir: dir}
	return i.read()
}

// RecoveryArtifact is the verified recovery binary of the installation at path.
func RecoveryArtifact(path string) (string, error) {
	j, err := Inspect(path)
	if err != nil {
		return "", err
	}
	if j == nil || j.Previous == nil {
		return "", errors.New("no verified recovery target")
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	dir := target + ".update"
	if _, err := os.Lstat(dir); err != nil {
		if dir, err = externalRecoveryDir(target); err != nil {
			return "", err
		}
	}
	return (&installation{target: target, dir: dir}).verify(*j.Previous)
}

// Digest is the SHA-256 of the file at path, as a journal records it.
func Digest(path string) (string, error) { return digest(path) }

// Observe lets change update the durable journal under the update lock, after
// reconciling an interrupted transaction. A nil journal is an error.
func Observe(ctx context.Context, path string, change func(*Journal) error) error {
	return withInstallation(ctx, path, func(i *installation) error {
		j, err := i.read()
		if err != nil {
			return err
		}
		if j == nil {
			return errors.New("no installation journal")
		}
		// A bad recovery binary is what a controller has to report: it must
		// still be able to say so here.
		if err := i.recover(j); err != nil && !errors.Is(err, errModifiedRecovery) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := change(j); err != nil {
			return err
		}
		return i.write(j)
	})
}

func withInstallation(ctx context.Context, path string, do func(*installation) error) error {
	i, err := openInstallation(ctx, path)
	if err != nil {
		return err
	}
	defer i.close()
	return do(i)
}

// Pending says the installed candidate of a journal awaits its verdict.
func (j *Journal) Pending() bool {
	return j != nil && (j.Probation == probationPending || j.Probation == probationRollingBack)
}

// RollingBack says a rollback began and did not end: it is finished, not judged.
func (j *Journal) RollingBack() bool { return j != nil && j.Probation == probationRollingBack }

// RolledBack says the journal's candidate was restored away from.
func (j *Journal) RolledBack() bool { return j != nil && j.Probation == probationRolledBack }

// Settled says no controller has anything left to do for this journal.
func (j *Journal) Settled() bool {
	switch {
	case j == nil, j.Probation == probationPassed, j.Held() != "":
		return true
	case j.Probation == probationRolledBack:
		return j.Observed == nil || j.Observed.Notified && j.Observed.Restarted
	}
	return false
}

// Held is why recovery of this journal failed, "" when it did not.
func (j *Journal) Held() string {
	if j == nil || j.Observed == nil {
		return ""
	}
	return j.Observed.Held
}

// WatchedProbation says the installation at path has a supervised candidate
// awaiting its verdict: an update would only be refused, so it waits. A
// journal that cannot be read answers false: the update itself refuses it.
func WatchedProbation(path string) bool {
	j, err := Inspect(path)
	if err != nil || j == nil || !j.Watched {
		return false
	}
	if j.Pending() {
		return true
	}
	if j.Probation == probationPrepared {
		d, err := Digest(path)
		return err == nil && d == j.Candidate.Digest
	}
	return false
}

// Quarantined is why the version is quarantined on the installation at path,
// "" when it is not.
func Quarantined(path, version string) string {
	j, err := Inspect(path)
	if err != nil || j == nil {
		return ""
	}
	if strings.TrimPrefix(j.Candidate.Version, "v") == strings.TrimPrefix(version, "v") {
		return j.Quarantine[j.Candidate.Digest]
	}
	return ""
}

// ClearHold lifts a failed recovery's hold once an operator has dealt with its
// cause. The quarantine stays: the failed candidate is never promoted. A
// recovery target that no longer verifies is dropped, or it would refuse every
// update after this one: the next install has none, as a first install has.
func ClearHold(ctx context.Context, path string) error {
	return withInstallation(ctx, path, func(i *installation) error {
		j, err := i.read()
		if err != nil {
			return err
		}
		if j == nil {
			return errors.New("no installation journal")
		}
		if j.Observed != nil {
			j.Observed.Held = ""
		}
		j.Watched = false
		if j.Previous != nil {
			if _, err := i.verify(*j.Previous); err != nil {
				j.Previous = nil
			}
		}
		return i.write(j)
	})
}

// launchController starts the controller for the candidate about to replace
// the executable and says whether it runs. Nothing can recover a first install.
func (i *installation) launchController(previous *Binary) bool {
	launch := optionsFrom(i.ctx).Recovery
	if launch == nil || previous == nil {
		return false
	}
	artifact, err := i.verify(*previous)
	if err != nil {
		return false
	}
	return launch(i.ctx, *previous, artifact, i.target) == nil
}

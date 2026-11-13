// Package fatal owns the persistent, machine-local outage queue. It deliberately
// does not depend on the supervisor, hooks, board or main configuration loader.
package fatal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const File = "fatal.json"

// MaxAttempts bounds the repair starts of one outage episode.
const MaxAttempts = 3

// repairCooldown spaces the repair attempts of an outage while a started
// repair has had no time to work. A start that failed is no such repair.
const repairCooldown = 10 * time.Minute

// Observation distinguishes a failed probe from one that could not run, and
// from a component deliberately disabled by its owner.
type Observation struct {
	ID, Evidence              string
	Failed, Unknown, Disabled bool
}

// Incident survives restarts. Only a successful probe resolves an outage;
// finishing a repair session never does. Attempts are reserved before launch.
type Incident struct {
	ID          string    `json:"id"`
	Evidence    string    `json:"evidence"`
	Owner       string    `json:"owner,omitempty"`
	Repair      string    `json:"repair,omitempty"`
	Opened      time.Time `json:"opened"`
	Resolved    time.Time `json:"resolved,omitzero"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	Attempts    int       `json:"attempts"`
	Outcome     string    `json:"outcome,omitempty"`
	// StartError is why the last attempt's repair start failed (its exit
	// and error); empty while that start succeeded or is not known yet.
	StartError string `json:"startError,omitempty"`
}

type Queue struct {
	Incidents []Incident `json:"incidents"`
}

func Read(dir string) (Queue, error) {
	b, err := os.ReadFile(filepath.Join(dir, File)) //nolint:gosec // fixed journal basename in the configured private state directory
	if errors.Is(err, os.ErrNotExist) {
		return Queue{}, nil
	}
	if err != nil {
		return Queue{}, err
	}
	var q Queue
	err = json.Unmarshal(b, &q)
	return q, err
}

// Save atomically publishes the admission barrier before any repair starts.
// The caller must hold fatal.lock for the entire reconciliation.
func (q Queue) Save(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".fatal-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, File)); err != nil {
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // configured private state directory; sync the rename before launching a repair
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Observe returns a transition message, empty for a repeated observation.
// Unknown never clears a known outage. Disabled explicitly withdraws it.
func (q *Queue) Observe(o Observation, now time.Time) string {
	for i := range q.Incidents {
		x := &q.Incidents[i]
		if x.ID != o.ID {
			continue
		}
		if o.Unknown {
			return ""
		}
		if !o.Failed || o.Disabled {
			if !x.Resolved.IsZero() {
				return ""
			}
			x.Resolved = now
			x.Outcome = "verified healthy"
			if o.Disabled {
				x.Outcome = "deliberately disabled"
			}
			return fmt.Sprintf("%s: %s", x.ID, x.Outcome)
		}
		if x.Resolved.IsZero() {
			x.Evidence = o.Evidence
			return ""
		}
		// A flapping probe must not orphan a repair still in review/proof.
		// New episodes get a new budget, retaining the owner and rate limit.
		*x = Incident{ID: o.ID, Evidence: o.Evidence, Opened: now, Owner: x.Owner, Repair: x.Repair, LastAttempt: x.LastAttempt}
		return "fatal detected: " + o.ID
	}
	if o.Unknown || o.Disabled || !o.Failed {
		return ""
	}
	q.Incidents = append(q.Incidents, Incident{ID: o.ID, Evidence: o.Evidence, Opened: now})
	return "fatal detected: " + o.ID
}

// Repository identifies a default-branch outage. Other incidents concern the
// execution infrastructure and retain the global ordinary-start barrier.
func (x Incident) Repository() string {
	repo, ok := strings.CutPrefix(x.ID, "main:")
	if !ok {
		return ""
	}
	return repo
}

// Scope is derived from the incident ID, including for journals from older
// versions. A stored display field can never weaken admission.
func (x Incident) Scope() string {
	if repo := x.Repository(); repo != "" {
		return "default-branch merges and releases in " + repo
	}
	return "all ordinary worker starts"
}

func (x Incident) MarshalJSON() ([]byte, error) {
	type incident Incident
	return json.Marshal(struct {
		incident
		Scope string `json:"scope"`
	}{incident: incident(x), Scope: x.Scope()})
}

func (x Incident) Refusal() error {
	return fmt.Errorf("fatal outage %s takes priority; scope: %s (repair owner %q, pull request %q): beekeeper fatal status", x.ID, x.Scope(), x.Owner, x.Repair)
}

// Barrier leaves existing work running. Repository CI outages never prevent
// workers from starting, even in that repository or after failed repairs.
// A reserved infrastructure repair may start despite other global outages.
func (q Queue) Barrier(name string) error {
	if name != "" {
		for _, x := range q.Incidents {
			if x.Owner == name && x.Resolved.IsZero() && x.Repository() == "" {
				return nil
			}
		}
	}
	for _, x := range q.Incidents {
		if x.Resolved.IsZero() && x.Repository() == "" {
			return x.Refusal()
		}
	}
	return nil
}

// MainOutage returns the unresolved publication barrier for exactly repo.
func (q Queue) MainOutage(repo string) (Incident, bool) {
	for _, x := range q.Incidents {
		if x.Resolved.IsZero() && x.Repository() != "" && strings.EqualFold(x.Repository(), repo) {
			return x, true
		}
	}
	return Incident{}, false
}

// Reserve chooses at most one repair, with a global owner and a bounded retry
// budget. A live owner blocks every other repair, even after its cooldown.
// The cooldown spaces repairs that started; after a failed start the next
// attempt is due at the next pass.
func (q *Queue) Reserve(now time.Time, live func(string) bool) *Incident {
	for _, x := range q.Incidents {
		if x.Owner != "" && live(x.Owner) {
			return nil
		}
	}
	for i := range q.Incidents {
		x := &q.Incidents[i]
		if x.Resolved.IsZero() && x.Attempts >= MaxAttempts {
			x.Outcome = "repair budget exhausted; outage unresolved"
			if x.StartError != "" {
				x.Outcome += "; last repair start failed: " + x.StartError
			}
		}
		cooling := !x.LastAttempt.IsZero() && now.Sub(x.LastAttempt) < repairCooldown && x.StartError == ""
		if !x.Resolved.IsZero() || x.Attempts >= MaxAttempts || cooling {
			continue
		}
		x.Attempts++
		x.LastAttempt = now
		x.Owner = fmt.Sprintf("fatal repair %d %d", x.Opened.UnixNano(), x.Attempts)
		x.Outcome = "reserved; launch outcome not yet known"
		x.StartError = ""
		return x
	}
	return nil
}

// StartFailed records that the reserved repair's start failed with why: the
// attempt stays spent and the next pass retries without the cooldown. The
// owner's name stays, so a session that start did register is still judged
// live or dead before any retry. It reports whether the budget is spent.
func (x *Incident) StartFailed(why string) (exhausted bool) {
	x.StartError = why
	x.Outcome = fmt.Sprintf("repair start failed: %s; retry at the next pass", why)
	if x.Attempts >= MaxAttempts {
		x.Outcome = "repair budget exhausted; outage unresolved; last repair start failed: " + why
		return true
	}
	return false
}

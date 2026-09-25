// Package lease holds the machine's shared resources, one holder at a time:
// kind labs, shared installations, the browser. A lease is a directory
// (mkdir is atomic) holding the holder's record; the record's fields are the
// ones the lab's earlier shell lock wrote, so both read the same directory.
//
// While a supervisor is recorded, a free lease is not permission: a session
// claims only what the supervisor granted it, in the order the grants were
// given, from the supervisor's start to a deliberate stop, whether its
// session runs or has crashed. Grants live in the state document.
package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
)

// Holder is the record in <leaseDir>/<resource>/holder.json.
type Holder struct {
	// Env is the resource; the field name is the shell lock's.
	Env         string `json:"env"`
	Holder      string `json:"holder"`
	Session     string `json:"session"`
	HostSession string `json:"hostSession,omitempty"`
	Name        string `json:"name,omitempty"`
	Purpose     string `json:"purpose"`
	Since       string `json:"since"`
	// UpgradeUnblock is the reason of the upgrade-unblock grant the claim
	// was admitted by during an upgrade, empty for any other claim.
	UpgradeUnblock string `json:"upgradeUnblock,omitempty"`
}

// Party is the holder as the state names it.
func (h Holder) Party() state.Party {
	return state.Party{Session: h.Session, HostSession: h.HostSession, Name: h.Name}
}

// SinceTime parses Since.
func (h Holder) SinceTime() time.Time {
	t, _ := time.Parse(time.RFC3339, h.Since)
	return t
}

// Dir is the lease directory.
type Dir string

func (d Dir) path(res string) string { return filepath.Join(string(d), res) }

// Get returns the holder of res, or nil when it is free.
func (d Dir) Get(res string) (*Holder, error) {
	raw, err := os.ReadFile(filepath.Join(d.path(res), "holder.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, err := os.Stat(d.path(res)); err == nil {
			// Taken a moment ago, the record not written yet.
			return &Holder{Env: res, Holder: "?", Purpose: "(being claimed)"}, nil
		}
		return nil, nil
	case err != nil:
		return nil, err
	}
	h := &Holder{}
	if err := json.Unmarshal(raw, h); err != nil {
		return nil, fmt.Errorf("%s: %w", res, err)
	}
	return h, nil
}

// Claim takes res for h. It returns the current holder instead when res is
// held already.
func (d Dir) Claim(res string, h Holder) (*Holder, error) {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(d.path(res), 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			cur, gerr := d.Get(res)
			if gerr != nil {
				return nil, gerr
			}
			return cur, nil
		}
		return nil, err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(d.path(res), "holder.json"), append(raw, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(d.path(res))
		return nil, err
	}
	return nil, nil
}

// Release frees res.
func (d Dir) Release(res string) error {
	return os.RemoveAll(d.path(res))
}

// List returns every held lease, oldest first.
func (d Dir) List() ([]Holder, error) {
	entries, err := os.ReadDir(string(d))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Holder
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		h, err := d.Get(e.Name())
		if err != nil || h == nil {
			continue
		}
		if h.Env == "" {
			h.Env = e.Name()
		}
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b Holder) int { return strings.Compare(a.Since, b.Since) })
	return out, nil
}

// Refusal is why a claim is not allowed.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Gate is what a claim is checked against.
type Gate struct {
	Resource string
	Caller   state.Party
	// Supervisor is the recorded supervisor, nil when none is.
	Supervisor *state.Supervisor
	// RestartUntil is set while the supervisor's CLI may still come back
	// as a restart; Gone once its CLI stayed away longer: claims wait for
	// its successor.
	RestartUntil time.Time
	Gone         bool
	// Held is whether the resource is held right now (by anyone).
	Held bool
	Now  time.Time
	TTL  time.Duration
}

// Pending returns the grants of res still claimable, in order.
func Pending(st *state.State, res string, held bool, now time.Time, ttl time.Duration) []state.Grant {
	var out []state.Grant
	for _, g := range st.Grants {
		if g.Resource == res && !expired(st, g, held, now, ttl) {
			out = append(out, g)
		}
	}
	return out
}

// Prune drops the grants whose time ran out; held says which resources are
// held right now (their grants wait without expiring).
func Prune(st *state.State, held map[string]bool, now time.Time, ttl time.Duration) {
	st.Grants = slices.DeleteFunc(st.Grants, func(g state.Grant) bool {
		return expired(st, g, held[g.Resource], now, ttl)
	})
}

// expired: a grant waits while its resource is held; once the resource is
// free its TTL runs from the later of the grant and the last release.
func expired(st *state.State, g state.Grant, held bool, now time.Time, ttl time.Duration) bool {
	if held {
		return false
	}
	from := g.At
	if r := st.Released[g.Resource]; r.After(from) {
		from = r
	}
	return now.Sub(from) > ttl
}

// Check decides whether the caller may claim and, when a grant carries the
// claim, returns its index in st.Grants (-1 when none is needed). While an
// upgrade runs on the installation of the resource's name only an
// upgrade-unblock grant admits a claim, in grant order.
func Check(st *state.State, g Gate) (int, error) {
	if h, ok := upgrade.Held(st, g.Resource, g.Now); ok {
		return checkUnblock(st, g, h)
	}
	if g.Supervisor == nil || g.Caller.Is(g.Supervisor.Party) || g.Caller.Session == "" {
		// No supervisor is recorded, the supervisor itself, or a person.
		return -1, nil
	}
	pending := Pending(st, g.Resource, g.Held, g.Now, g.TTL)
	for pos, p := range pending {
		if !p.To.Is(g.Caller) {
			continue
		}
		if pos > 0 {
			return -1, &Refusal{fmt.Sprintf("%s is granted to %q first; you are number %d in its queue", g.Resource, pending[0].To.Name, pos+1)}
		}
		return slices.IndexFunc(st.Grants, func(x state.Grant) bool {
			return x.Resource == p.Resource && x.To.Is(p.To) && x.At.Equal(p.At)
		}), nil
	}
	switch {
	case g.Gone:
		return -1, &Refusal{fmt.Sprintf("supervisor %q is gone: a free lease is not a grant until its successor's `beekeeper supervisor start`; send the successor `%s needed: <purpose>`",
			g.Supervisor.Name, g.Resource)}
	case !g.RestartUntil.IsZero():
		return -1, &Refusal{fmt.Sprintf("supervisor %q is restarting its CLI (grace until %s): a free lease is not a grant; send it `%s needed: <purpose>` once it is back",
			g.Supervisor.Name, g.RestartUntil.Local().Format(time.TimeOnly), g.Resource)}
	}
	return -1, &Refusal{fmt.Sprintf("supervisor %q runs: a free lease is not a grant; send it `%s needed: <purpose>` and claim after its `yours %s`",
		g.Supervisor.Name, g.Resource, g.Resource)}
}

// checkUnblock admits a claim during the upgrade h only by the caller's
// upgrade-unblock grant, the first such grant of the resource.
func checkUnblock(st *state.State, g Gate, h state.Hold) (int, error) {
	pending := slices.DeleteFunc(Pending(st, g.Resource, g.Held, g.Now, g.TTL), func(p state.Grant) bool { return p.UpgradeUnblock == "" })
	for pos, p := range pending {
		if !p.To.Is(g.Caller) {
			continue
		}
		if pos > 0 {
			return -1, &Refusal{fmt.Sprintf("%s runs: its upgrade unblock is granted to %q first; you are number %d", h.Reason, pending[0].To.Name, pos+1)}
		}
		return slices.IndexFunc(st.Grants, func(x state.Grant) bool {
			return x.Resource == p.Resource && x.To.Is(p.To) && x.At.Equal(p.At)
		}), nil
	}
	return -1, &Refusal{fmt.Sprintf("%s runs since %s: claim after it ends (beekeeper hold lists it); only work that unblocks the upgrade is claimed during it, with the supervisor's `lease grant %s <session> --upgrade-unblock <why>`",
		h.Reason, h.At.Local().Format(time.TimeOnly), g.Resource)}
}

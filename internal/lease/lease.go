// Package lease holds the machine's shared resources, one holder at a time:
// kind labs, shared installations, the browser. A lease is a directory
// (mkdir is atomic) holding the holder's record; the record's fields are the
// ones the lab's earlier shell lock wrote, so both read the same directory.
//
// Grants live in the state document and queue claims in grant order.
// A resource with no pending grants can be claimed directly.
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
	// Person, Team and Host are the holder party's.
	Person           string `json:"person,omitempty"`
	Team             string `json:"team,omitempty"`
	Host             string `json:"host,omitempty"`
	Purpose          string `json:"purpose"`
	DestructiveProof bool   `json:"destructiveProof,omitempty"`
	PreviousProof    *Proof `json:"previousProof,omitempty"`
	Since            string `json:"since"`
	// UpgradeUnblock is the reason of the upgrade-unblock grant the claim
	// was admitted by during an upgrade, empty for any other claim.
	UpgradeUnblock string `json:"upgradeUnblock,omitempty"`
	// BudgetGiB is what the holder's models may hold on the model server,
	// 0 on every other resource.
	BudgetGiB int `json:"budgetGiB,omitempty"`
	// LabConfig is a lab's agentlab.yaml at the claim, nil on every other
	// resource and on a lab whose configuration the claim could not read.
	LabConfig *LabConfig `json:"labConfig,omitempty"`
	// LabChange is the change the lab's previous release left, shown to
	// this holder at its claim.
	LabChange *LabChange `json:"labChange,omitempty"`
	// Damaged is why the resource's record could not be read, empty for a
	// readable one. A damaged lease stays held: only a forced release frees
	// it.
	Damaged string `json:"-"`
}

// Label is the resource with the budget its claim carries.
func (h Holder) Label() string {
	if h.BudgetGiB == 0 {
		return h.Env
	}
	return fmt.Sprintf("%s (%d GiB)", h.Env, h.BudgetGiB)
}

// Party is the holder as the state names it.
func (h Holder) Party() state.Party {
	return state.Party{Session: h.Session, HostSession: h.HostSession, Name: h.Name, Person: h.Person, Team: h.Team, Host: h.Host}
}

// SinceTime parses Since.
func (h Holder) SinceTime() time.Time {
	t, _ := time.Parse(time.RFC3339, h.Since)
	return t
}

// Dir is the lease directory.
type Dir string

func (d Dir) path(res string) string { return filepath.Join(string(d), res) }

// Get returns the holder of res, or nil when it is free. A record that
// cannot be read or decoded is a damaged holder: the resource stays held,
// named, and the error is the caller's only for the lease directory itself.
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
		return damaged(res, err), nil
	}
	h := &Holder{}
	if err := json.Unmarshal(raw, h); err != nil {
		return damaged(res, err), nil
	}
	return h, nil
}

// damaged is the holder of res whose record failed with err.
func damaged(res string, err error) *Holder {
	return &Holder{
		Env:     res,
		Holder:  "?",
		Purpose: fmt.Sprintf("(unreadable holder record: beekeeper lease release %s --force frees it)", res),
		Damaged: err.Error(),
	}
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
	if err := d.write(res, h); err != nil {
		_ = os.RemoveAll(d.path(res))
		return nil, err
	}
	return nil, nil
}

// Hand gives the lease h.Env, held, to h: its record is replaced in one
// rename, so a reader sees the old holder or the new one, never none.
func (d Dir) Hand(h Holder) error {
	return d.write(h.Env, h)
}

// write puts h as the record of res in one rename, so a reader or a crash
// never leaves a partly written record.
func (d Dir) write(res string, h Holder) error {
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(d.path(res), ".holder-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(raw, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(d.path(res), "holder.json"))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Release frees res.
func (d Dir) Release(res string) error {
	h, err := d.Get(res)
	if err != nil {
		return err
	}
	if h != nil {
		var proof *Proof
		if h.DestructiveProof || IsDestructiveProof(h.Purpose) {
			proof = &Proof{Purpose: h.Purpose, By: h.Name}
		}
		raw, err := json.Marshal(proof)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d.proofPath(res), raw, 0o600); err != nil {
			return err
		}
	}
	return os.RemoveAll(d.path(res))
}

// List returns every held lease, oldest first; a damaged record is listed as
// its damaged holder, so a listing never calls an occupied resource free.
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
		if err != nil {
			return nil, err
		}
		if h == nil {
			continue
		}
		if h.Env == "" {
			h.Env = e.Name()
		}
		out = append(out, *h)
	}
	slices.SortStableFunc(out, func(a, b Holder) int { return strings.Compare(a.Since, b.Since) })
	return out, nil
}

// Refusal is why a claim is not allowed. A claim queued behind earlier
// grants carries its place: Position (from 1) and Ahead, the grantee who
// claims first.
type Refusal struct {
	Reason   string
	Position int
	Ahead    string
}

func (r *Refusal) Error() string { return r.Reason }

// Gate is what a claim is checked against.
type Gate struct {
	Resource string
	Caller   state.Party
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

// ToHead moves st.Grants[i] before every other grant of its resource, as
// given at now: the grants it passes keep their order behind it. It returns
// the grant's new index.
func ToHead(st *state.State, i int, now time.Time) int {
	g := st.Grants[i]
	g.At = now.UTC()
	st.Grants = slices.Delete(st.Grants, i, i+1)
	j := slices.IndexFunc(st.Grants, func(x state.Grant) bool { return x.Resource == g.Resource })
	if j < 0 {
		j = len(st.Grants)
	}
	st.Grants = slices.Insert(st.Grants, j, g)
	return j
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

// Check admits the head grant only; an empty queue permits a direct claim.
// Upgrade holds require an upgrade-unblock grant.
func Check(st *state.State, g Gate) (int, error) {
	if h, ok := upgrade.Held(st, g.Resource, g.Now); ok {
		return checkUnblock(st, g, h)
	}
	pending := Pending(st, g.Resource, g.Held, g.Now, g.TTL)
	for pos, p := range pending {
		if p.To.Is(g.Caller) {
			if pos > 0 {
				return -1, &Refusal{Reason: fmt.Sprintf("%s is granted to %q first; you are number %d", g.Resource, pending[0].To.Name, pos+1), Position: pos + 1, Ahead: pending[0].To.Name}
			}
			return slices.IndexFunc(st.Grants, func(x state.Grant) bool {
				return x.Resource == p.Resource && x.To.Is(p.To) && x.At.Equal(p.At)
			}), nil
		}
	}
	if len(pending) > 0 {
		return -1, &Refusal{Reason: fmt.Sprintf("%s is granted to %q first; request a grant to join its queue", g.Resource, pending[0].To.Name), Ahead: pending[0].To.Name}
	}
	return -1, nil
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
			return -1, &Refusal{Reason: fmt.Sprintf("%s runs: its upgrade unblock is granted to %q first; you are number %d", h.Reason, pending[0].To.Name, pos+1), Position: pos + 1, Ahead: pending[0].To.Name}
		}
		return slices.IndexFunc(st.Grants, func(x state.Grant) bool {
			return x.Resource == p.Resource && x.To.Is(p.To) && x.At.Equal(p.At)
		}), nil
	}
	return -1, &Refusal{Reason: fmt.Sprintf("%s runs since %s: claim after it ends (beekeeper hold lists it); only work that unblocks the upgrade is claimed during it, with the supervisor's `lease grant %s <session> --upgrade-unblock <why>`",
		h.Reason, h.At.Local().Format(time.TimeOnly), g.Resource)}
}

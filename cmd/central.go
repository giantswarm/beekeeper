package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// ExitCentral is a central verb refused because the central instance is
// unreachable: there is no local copy to act on instead.
const ExitCentral = 69

// hub is the client of the central instance; nil when none is configured.
func (a *app) hub() *central.Client {
	if a.centralClient == nil && a.cfg.Central.Enabled() {
		a.centralClient = central.New(a.cfg.Central, nil)
	}
	return a.centralClient
}

// callCentral calls one of the central instance's tools as me: the person
// is the token's, the agent and its host are me's.
func (a *app) callCentral(ctx context.Context, me state.Party, tool string, args map[string]any, v any) (string, error) {
	text, err := a.callCentralRaw(ctx, me, tool, args, v)
	return text, centralErr(err)
}

// callCentralRaw is callCentral with the client's error: *central.Refused,
// *central.Unreachable or another.
func (a *app) callCentralRaw(ctx context.Context, me state.Party, tool string, args map[string]any, v any) (string, error) {
	args[paramAgent], args[paramHost] = me.Name, me.Host
	return a.hub().Call(ctx, tool, args, v)
}

// centralErr carries a central call's failure as the exit code it asks for:
// a refusal 3, an unreachable instance ExitCentral.
func centralErr(err error) error {
	var r *central.Refused
	var u *central.Unreachable
	switch {
	case errors.As(err, &r) && r.Usage:
		return usageErr("%s", r.Message)
	case errors.As(err, &r):
		return refused("%s", r.Message)
	case errors.As(err, &u):
		return &exitError{code: ExitCentral, msg: u.Error()}
	}
	return err
}

// centralLease is a held lease of the central instance (lease_list).
type centralLease struct {
	Environment string      `json:"environment"`
	Holder      state.Party `json:"holder"`
	Purpose     string      `json:"purpose"`
	Since       time.Time   `json:"since"`
}

// centralLeases is the central instance's lease_list: its Environments,
// held and free.
type centralLeases struct {
	Context string         `json:"context"`
	Held    []centralLease `json:"held"`
	Free    []string       `json:"free"`
}

func (a *app) centralLeases(me state.Party) (*centralLeases, error) {
	l := &centralLeases{Context: a.cfg.Central.Context}
	if _, err := a.callCentral(context.Background(), me, "lease_list", map[string]any{}, l); err != nil {
		return nil, err
	}
	return l, nil
}

// holder is the central holder of env, nil when it is free; an env the
// central instance does not know is a usage error.
func (l *centralLeases) holder(env string) (*centralLease, error) {
	for i, h := range l.Held {
		if h.Environment == env {
			return &l.Held[i], nil
		}
	}
	if slices.Contains(l.Free, env) {
		return nil, nil
	}
	return nil, usageErr("%s is neither one of this machine's resources (%s) nor an Environment of the central instance (%s)",
		env, strings.Join(slices.Concat(l.Free, heldNames(l.Held)), ", "), l.Context)
}

func heldNames(held []centralLease) []string {
	out := make([]string, len(held))
	for i, h := range held {
		out[i] = h.Environment
	}
	return out
}

// centralHeldBy is the refusal naming a central lease's holder.
func (a *app) centralHeldBy(h centralLease) error {
	return refused("%s is held by %q since %s: %s", h.Environment, partyName(h.Holder), clock(a.now, h.Since), h.Purpose)
}

// claimCentral claims a central resource: the machine's supervisor grants
// it as a local one, the central instance holds it.
func (a *app) claimCentral(res, purpose string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	dir := lease.Dir(a.cfg.LeaseDir)
	check := func(st *state.State) (int, error) {
		holders, err := dir.List()
		if err != nil {
			return -1, err
		}
		lease.Prune(st, heldMap(holders), a.now, a.cfg.GrantTTL.Duration)
		sv := a.supervision(st, sessions)
		idx, err := lease.Check(st, lease.Gate{Resource: res, Caller: me, Supervisor: sv.sup, RestartUntil: sv.until, Gone: sv.down(), Now: a.now, TTL: a.cfg.GrantTTL.Duration})
		if r := (*lease.Refusal)(nil); errors.As(err, &r) {
			return -1, refused("%s: %s", res, r.Reason)
		}
		return idx, err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if _, err := check(st); err != nil {
		return err
	}
	text, err := a.callCentral(context.Background(), me, "lease_claim", map[string]any{paramEnvironment: res, paramPurpose: purpose}, nil)
	if err != nil {
		return err
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if idx, err := check(st); err == nil && idx >= 0 {
			st.Grants = slices.Delete(st.Grants, idx, idx+1)
		}
		return []state.Event{event(me, verbLeaseClaim, "%s (central %s): %s", res, a.cfg.Central.Context, purpose)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, text)
	return err
}

// releaseCentral releases a central resource: the caller's own, or with
// force whoever's the central instance lets the caller free.
func (a *app) releaseCentral(res string, force bool) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	l, err := a.centralLeases(me)
	if err != nil {
		return err
	}
	h, err := l.holder(res)
	if err != nil {
		return err
	}
	if h == nil {
		_, err := fmt.Fprintln(a.out, res+" was free")
		return err
	}
	if !h.Holder.Is(me) && !force {
		return refused("%s is held by %q, not by you: only --force frees another holder's lease", res, partyName(h.Holder))
	}
	text, err := a.callCentral(context.Background(), me, "lease_release", map[string]any{paramEnvironment: res}, nil)
	if err != nil {
		return err
	}
	_ = a.store.Log(event(me, "lease.release", "%s (central %s)", res, a.cfg.Central.Context))
	_, err = fmt.Fprintln(a.out, text)
	return err
}

// statusCentral says whether a central resource is free, or refuses with
// its holder.
func (a *app) statusCentral(res string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	l, err := a.centralLeases(me)
	if err != nil {
		return err
	}
	h, err := l.holder(res)
	if err != nil {
		return err
	}
	if h == nil {
		_, err := fmt.Fprintln(a.out, "free")
		return err
	}
	if a.json {
		_ = a.printJSON(h)
	}
	return a.centralHeldBy(*h)
}

// printCentralLeases prints the central instance's leases below the
// machine's.
func (a *app) printCentralLeases(l *centralLeases) {
	_, _ = fmt.Fprintf(a.out, "\ncentral (muster context %s):\n", l.Context)
	if len(l.Held) == 0 {
		_, _ = fmt.Fprintln(a.out, "no installation is held")
	} else {
		w := a.table()
		_, _ = fmt.Fprintln(w, "ENVIRONMENT\tHOLDER\tSINCE\tPURPOSE")
		for _, h := range l.Held {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", h.Environment, withOwner(truncate(partyName(h.Holder), 60), state.Party{Team: h.Holder.Team, Host: h.Holder.Host}),
				clock(a.now, h.Since), truncate(h.Purpose, 60))
		}
		_ = w.Flush()
	}
	if len(l.Free) > 0 {
		_, _ = fmt.Fprintln(a.out, "free:", strings.Join(l.Free, ", "))
	}
}

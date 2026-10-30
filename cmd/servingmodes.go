package cmd

// The serving modes of a self-hosted provider: the modes its model can be
// served in, the constraint a node lease is on them, and the machine's one
// switch at a time. A mode names the nodes it occupies and the sessions it
// sustains; a claim of a node a mode occupies is the generic constraint
// "needs node N exclusively", and the model drains to the mode the
// constraints allow before the platform's own switch command runs.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const (
	// verbServeDrain logs a model draining into the mode a constraint allows.
	verbServeDrain = "serve.drained"
	// verbServeSwitch logs a serving-mode switch, run or failed.
	verbServeSwitch = "serve.switch"
	// verbServeRefused logs a switch refused by a holding constraint.
	verbServeRefused = "serve.refused"
)

// serveModeStateFile is the state's side file holding a self-hosted
// provider's serving-mode machine: the mode the last read reported, the mode
// the desk is waiting to switch into and why the switch waits.
func serveModeStateFile(name string) string {
	return "serve-mode-state-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

// serveModePendingFile is the state's side file of the constraint request
// that drains a self-hosted provider: the node claimed, the mode the switch
// waits to run into, and why it waits.
func serveModePendingFile(name string) string {
	return "serve-mode-pending-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

// serveModeLockFile is the state's side file a running switch holds: one
// switch at a time, across the machine's processes.
func serveModeLockFile(name string) string {
	return "serve-mode-lock-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

// ServeModeState is a self-hosted provider's serving-mode machine: the mode
// the reader reported, the mode a switch waits to run into, the node whose
// constraint the drain waits for, and why the switch waits.
type ServeModeState struct {
	// Mode is the mode the reader last reported.
	Mode string `json:"mode,omitempty"`
	// Pending is the mode a switch waits to run into.
	Pending string `json:"pending,omitempty"`
	// Node is the constraint the pending switch waits for; empty once the
	// holder released and the request stands only for the watch to retry.
	Node string `json:"node,omitempty"`
	// Reason says why the switch waits.
	Reason string `json:"reason,omitempty"`
	// Since is when the desk asked for the pending switch.
	Since time.Time `json:"since,omitzero"`
	// At is when the machine was last reconciled.
	At time.Time `json:"at,omitzero"`
}

// serveModeState reads the provider's serving-mode machine; a missing file
// is the zero state.
func (a *app) serveModeState(name string) (ServeModeState, error) {
	var s ServeModeState
	if a.store == nil {
		return s, nil
	}
	found, err := a.store.ReadFile(serveModeStateFile(name), &s)
	if err != nil || !found {
		return ServeModeState{}, err
	}
	return s, nil
}

// pendingServeMode is the constraint request that drains a self-hosted
// provider: the node claimed and the mode the switch waits to run into.
func (a *app) pendingServeMode(name string) (ServeModeState, error) {
	var s ServeModeState
	if a.store == nil {
		return s, nil
	}
	found, err := a.store.ReadFile(serveModePendingFile(name), &s)
	if err != nil || !found {
		return ServeModeState{}, err
	}
	return s, nil
}

// writePendingServeMode records the constraint request of a drain.
func (a *app) writePendingServeMode(name string, s ServeModeState) {
	if a.store != nil {
		_ = a.store.WriteFile(serveModePendingFile(name), s)
	}
}

// clearPendingServeMode drops the constraint request once the switch ran or
// the constraint fell.
func (a *app) clearPendingServeMode(name string) {
	if a.store == nil {
		return
	}
	_ = os.Remove(filepath.Join(a.store.Dir(), serveModePendingFile(name)))
}

// writeServeModeState records the provider's serving-mode machine.
func (a *app) writeServeModeState(name string, s ServeModeState) {
	if a.store != nil {
		_ = a.store.WriteFile(serveModeStateFile(name), s)
	}
}

// nodeHeldByOthers says whether the lease record of the node stands held by
// a party other than this caller: a lease held of the node is the generic
// constraint "needs node N exclusively"; the node this caller holds itself
// is its own claim's or release's, never a constraint standing behind it.
// The party, not its name alone: a CLI run by hand and the machine's own
// actor can carry the same name and are not the same holder.
func (a *app) nodeHeldByOthers(dir lease.Dir, n string) bool {
	h, err := dir.Get(n)
	return err == nil && h != nil && !h.Party().Is(a.actor())
}

// serveModeNodesHeld are the machine's nodes a serving mode would occupy
// that a constraint holds: of the held leases, those the mode occupies.
// The lease's own name decides: a node claimed is the generic constraint
// "needs node N exclusively", whoever holds it.
func (a *app) serveModeNodesHeld(p config.Provider, mode string, dir lease.Dir) []string {
	nodes, ok := p.ServeModeNodes(mode)
	if !ok || len(nodes) == 0 {
		return nil
	}
	var held []string
	for _, n := range nodes {
		if a.nodeHeldByOthers(dir, n) {
			// A lease held of the node is the constraint; the node this
			// caller holds itself is its own claim's or release's, never
			// a constraint standing behind it — except the node whose
			// claim or release runs a switch right now: the caller asks
			// for that node as held itself, and its own switch never
			// stands behind its own node.
			held = append(held, n)
		} else if n == a.serveModeSwitching {
			held = append(held, n)
		}
	}
	return held
}

// drainCeiling is the ceiling the provider runs with while the constraints
// hold: the mode the constraints allow names it, so a node claimed lowers
// the model's ceiling to what the mode without the node sustains before the
// switch runs.
func drainCeiling(p config.Provider, heldFn func(mode string) []string) (int, bool) {
	want := serveModeWanted(p, heldFn)
	if want.Mode == "" {
		return 0, false
	}
	return p.ServeModeCeiling(want.Mode)
}

// modeIsHeld says whether a constraint holds a node the mode occupies: the
// nodes heldBy names for the mode, those of the mode itself among them.
func modeIsHeld(p config.Provider, mode string, heldBy func(string) []string) bool {
	return modeHeld(p, mode, heldBy(mode))
}

// serveModeReadings is the provider's serving-mode reading for a capacity
// view: the mode the reader reports, the mode a switch waits to run into,
// whose ceiling drains the provider while busy sessions finish or park, and
// the ceiling the constraints held right now stand under: a node claimed
// lowers the model's ceiling to the mode the machine runs with — a
// constraint lowers and never raises — and a drain holds the provider at
// the ceiling of the mode it drains to.
func (a *app) serveModeReadings(ctx context.Context, name string, p config.Provider) providerReadingResult {
	r := a.serveMode(ctx, name, p)
	res := providerReadingResult{serveMode: r.Mode, serveModeErr: r.Err}
	dir := lease.Dir(a.cfg.LeaseDir)
	heldBy := func(mode string) []string { return a.serveModeNodesHeld(p, mode, dir) }
	// The ceiling the constraints stand the provider under: a mode the
	// constraints hold — one of its nodes stands claimed — keeps the
	// provider at the mode the machine ran before it moved (the recorded
	// one, if a switch moved it, else what it runs now); the drain of a
	// mode the constraints allow narrows the provider to the mode it
	// drains into. The narrower of the readings holds.
	ceiling, hasCeiling := 0, false
	narrow := func(c int, ok bool) {
		if ok && (!hasCeiling || c < ceiling) {
			ceiling, hasCeiling = c, true
		}
	}
	if modeIsHeld(p, r.Mode, heldBy) {
		if st, err := a.serveModeState(name); err == nil && st.Mode != "" && !strings.EqualFold(st.Mode, r.Mode) {
			narrow(p.ServeModeCeiling(st.Mode))
		} else {
			narrow(p.ServeModeCeiling(r.Mode))
		}
	} else {
		narrow(drainCeiling(p, heldBy))
	}
	// The state and the request file are one request: the request the
	// constraint asked for is the newer one, and the machine's record
	// names the same drain. The pending mode is the one that is not the
	// mode the model already runs — a request settled by a switch that
	// ran is no drain.
	if sm, err := a.serveModeState(name); err == nil && sm.Pending != "" && !strings.EqualFold(sm.Pending, r.Mode) {
		res.pendingMode = sm.Pending
	}
	if pnd, err := a.pendingServeMode(name); err == nil && pnd.Pending != "" && !strings.EqualFold(pnd.Pending, r.Mode) {
		res.pendingMode = pnd.Pending
	}
	// The drain waiting narrows the provider to the ceiling of the mode
	// it drains into: no session starts on the model beyond what the
	// mode the machine moves to sustains.
	if res.pendingMode != "" {
		narrow(p.ServeModeCeiling(res.pendingMode))
	}
	res.hasConstraintCeiling, res.constraintCeiling = hasCeiling, ceiling
	return res
}

// busyOnProvider are the busy agents that run on the self-hosted provider.
func (a *app) busyOnProvider(name string) ([]string, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	sessions, _, err := a.sessions()
	if err != nil && !platformMissing(err) {
		return nil, err
	}
	k := a.liveCapacity()
	pa := countProviderAgents(st, sessions, k, a.nowOrNow())
	return pa[name].Busy, nil
}

// serveModeTarget is the mode the provider should run in right now and why:
// "" with the reason empty when it should stay as it is, and a drain to the
// mode the constraints allow while busy sessions still run on the model.
type serveModeTarget struct {
	Mode   string
	Reason string
	// Drain is set while the switch waits for the model's sessions to
	// finish or park; the ceiling of the target mode already holds then.
	Drain bool
}

// serveModeWanted is the mode a self-hosted provider should run in, read
// from its modes and the constraints held: the widest mode occupying no held
// node — the same selection serveModeAllowed makes on the held set, so
// claim, release, watch and capacity never disagree on the mode the machine
// runs. heldFn names which of the mode's nodes stand held.
func serveModeWanted(p config.Provider, heldFn func(mode string) []string) serveModeTarget {
	best, bestSessions := "", -1
	// The modes in name order: with two modes of equal sessions the
	// wanted one is the same in every process, not the map's order.
	for _, mode := range slices.Sorted(maps.Keys(p.ServeModeModes)) {
		if modeHeld(p, mode, heldFn(mode)) {
			continue
		}
		if s := p.ServeModeModes[mode].Sessions; s > bestSessions {
			best, bestSessions = mode, s
		}
	}
	return serveModeTarget{Mode: best}
}

// serveModeAllowed is the mode the machine runs while the held nodes stand:
// the mode sustaining the most sessions among those occupying none of them.
// It is the same selection in every place the machine decides a mode — the
// claim's drain, the release's switch back, the watch's reconcile and the
// capacity's constraint ceiling — so a drain never asks for a mode a later
// tick would want again: where one mode is free of a node and a wider one is
// held by another node, the model runs the widest mode the constraints allow
// and the claim of the held one waits behind its holder. "" when no mode
// runs without the held nodes: the constraint has nowhere to drain to.
func serveModeAllowed(p config.Provider, held []string) string {
	best, bestSessions := "", -1
	// The modes in name order: with two modes of equal sessions the
	// allowed one is the same in every process, not the map's order.
	modes := slices.Sorted(maps.Keys(p.ServeModeModes))
	for _, mode := range modes {
		if modeHeld(p, mode, held) {
			continue
		}
		if s := p.ServeModeModes[mode].Sessions; s > bestSessions {
			best, bestSessions = mode, s
		}
	}
	return best
}

// modeHeld says whether any held node is one the mode occupies.
func modeHeld(p config.Provider, mode string, held []string) bool {
	for _, n := range p.ServeModeModes[mode].Nodes {
		if slices.Contains(held, n) {
			return true
		}
	}
	return false
}

// reconcileServeModes brings every self-hosted provider's serving mode to
// what its constraints ask for: it lowers the ceiling of a model whose mode
// occupies a newly held node (drain), runs the platform's own switch once
// the model drained or a constraint fell (switch and raise), and re-bases
// the recorded mode on what the reader reports. A failure leaves the mode as
// reported and the request pending; the next read retries it.
func (a *app) reconcileServeModes(ctx context.Context) error {
	k := a.liveCapacity()
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.HasServeModeSwitch() || len(p.ServeModeModes) == 0 {
			continue
		}
		if err := a.reconcileServeMode(ctx, name, p); err != nil {
			return err
		}
	}
	return nil
}

// reconcileServeMode reconciles one provider's serving mode.
func (a *app) reconcileServeMode(ctx context.Context, name string, p config.Provider) error {
	if !p.HasServeModeSwitch() || len(p.ServeModeModes) == 0 {
		return nil
	}
	dir := lease.Dir(a.cfg.LeaseDir)
	r := a.serveMode(ctx, name, p)
	if r.Err != "" {
		// The mode is unknown: nothing moves on an unread mode; the line
		// says it, and the next read retries.
		return nil
	}
	st, err := a.serveModeState(name)
	if err != nil {
		return err
	}
	now := a.nowOrNow()
	// Every read rebases the recorded mode: the state follows what the
	// command reports, never what was recorded. A pending drain that
	// names the very mode the model runs in is settled: the request is
	// dropped with it, so no later claim of the node it names stands
	// behind a switch already done.
	if st.Pending != "" && strings.EqualFold(st.Pending, r.Mode) {
		st.Pending, st.Reason, st.Node = "", "", ""
		a.clearPendingServeMode(name)
	}
	st.Mode = r.Mode
	st.At = now
	want := serveModeWanted(p, func(mode string) []string { return a.serveModeNodesHeld(p, mode, dir) })
	if want.Mode == "" {
		a.writeServeModeState(name, st)
		return nil
	}
	// A constraint lowers the model, never raises it: a wanted mode wider
	// than the model runs is no drain while a node stands claimed — the
	// machine moved down for a reason and stays down; the wider mode comes
	// back with the release. With no node held, the wanted mode is the
	// widest of all and this never fires.
	if r.Mode != "" && !strings.EqualFold(want.Mode, r.Mode) &&
		ceilingOf(p, want.Mode) > ceilingOf(p, r.Mode) && len(a.serveModeHeldNodes()) > 0 {
		want.Mode = r.Mode
	}
	// A drain already asked for stands: the watch keeps working on the
	// request the constraints last allowed, and a lower one it recorded
	// than the constraints allow now is the constraint's own. A drain of
	// a node no constraint holds any more and no claim of it waits any
	// more is no drain: its request fell with the lease that asked for
	// it, and the mode it named would hold the machine narrower than the
	// constraints allow. The lease of a claim still waiting on the drain
	// stands only after the switch ran, so a claim whose node is free is
	// matched against its own record: a pending drain naming the node
	// keeps its request while the model still runs over the drain's
	// ceiling — the sessions have not left the model — and falls once
	// they have, when the switch is retried right here.
	if pnd, err := a.pendingServeMode(name); err == nil && pnd.Pending != "" && pnd.Node != "" {
		if held, gerr := dir.Get(pnd.Node); gerr == nil && held == nil {
			drop := true
			if _, perr := a.serveModeState(name); perr == nil {
				if busy, berr := a.busyOnProvider(name); berr == nil {
					if c, ok := p.ServeModeCeiling(pnd.Pending); ok && len(busy) > c {
						// The model still runs the sessions the drain
						// waits for: the claim it stands for has not
						// been granted, and its drain keeps its node.
						drop = false
					}
				}
			}
			if drop {
				pnd.Node = ""
				a.writePendingServeMode(name, pnd)
			}
		}
		if pnd.Node != "" {
			if c, ok := p.ServeModeCeiling(pnd.Pending); ok && c < p.ServeModeModes[want.Mode].Sessions {
				want.Mode = pnd.Pending
			}
		}
	}
	if strings.EqualFold(st.Mode, want.Mode) {
		st.Pending, st.Reason, st.Node = "", "", ""
		a.writeServeModeState(name, st)
		a.clearPendingServeMode(name)
		return nil
	}
	// A switch waits: the ceiling of the wanted mode holds while the model
	// drains or a switch is asked for, and the switch runs once nothing on
	// the model stands in the way. The node the drain waits for is the
	// constraint's own record; a drain the watch opens itself names none.
	if st.Pending == "" || !strings.EqualFold(st.Pending, want.Mode) {
		pnd, perr := a.pendingServeMode(name)
		node := ""
		if perr == nil && pnd.Pending != "" && strings.EqualFold(pnd.Pending, want.Mode) {
			node = pnd.Node
		}
		st.Pending, st.Since, st.Reason, st.Node = want.Mode, now, "the model's sessions drain", node
		a.writeServeModeState(name, st)
		a.writePendingServeMode(name, ServeModeState{Mode: st.Mode, Node: st.Node, Pending: st.Pending, Since: st.Since, Reason: st.Reason})
		if err := a.store.Record(event(a.actor(), verbServeDrain, "%s: %s -> %s: %s", name, st.Mode, want.Mode, st.Reason)); err != nil {
			return err
		}
	} else if pnd, err := a.pendingServeMode(name); err == nil && pnd.Pending == "" {
		a.writePendingServeMode(name, ServeModeState{Pending: st.Pending, Since: st.Since, Reason: st.Reason})
	}
	busy, err := a.busyOnProvider(name)
	if err != nil {
		return err
	}
	if limit, ok := p.ServeModeCeiling(want.Mode); ok && len(busy) > limit {
		st.Reason = fmt.Sprintf("%d sessions still run; %d drain", len(busy), limit)
		a.writeServeModeState(name, st)
		return nil
	}
	// The model drained: the switch runs. A request whose node stands free
	// again is a claim still waiting for its grant or a constraint that
	// fell: the node names only what a pending switch waits for, and it
	// clears here so an old one never holds a later switch refused.
	if pnd, err := a.pendingServeMode(name); err == nil && pnd.Node != "" {
		if held, gerr := dir.Get(pnd.Node); gerr == nil && held == nil {
			pnd.Node = ""
			a.writePendingServeMode(name, pnd)
		}
	}
	if err := a.switchServeMode(ctx, name, want.Mode, 0); err != nil {
		st.Reason = err.Error()
		a.writeServeModeState(name, st)
		return nil
	}
	_ = a.forgetCachedServeMode(name)
	if fresh := a.serveMode(ctx, name, p); fresh.Mode != "" {
		st.Mode = fresh.Mode
	}
	st.Pending, st.Reason = "", ""
	a.writeServeModeState(name, st)
	return nil
}

// platformMissing reports whether err is the platform refusing to list the
// sessions: busy agents are then counted from the roster alone.
func platformMissing(err error) bool { return err != nil && strings.Contains(err.Error(), "nknown") }

func (a *app) nowOrNow() time.Time {
	if a.now.IsZero() {
		return time.Now()
	}
	return a.now
}

// switchServeMode runs the provider's switch into mode through the
// platform's own command. It is refused — never run — while a constraint
// holds a node the mode occupies, while a session of this very caller runs
// on the model, and while another switch runs. A failure leaves the mode as
// the reader reports it and leaves a pending switch to retry.
func (a *app) switchServeMode(ctx context.Context, name, mode string, timeout time.Duration) error {
	k := a.liveCapacity()
	p, ok := k.Providers[name]
	if !ok || !p.HasServeModeSwitch() {
		return refused("%s has no serving-mode switch command", name)
	}
	if a.callerRunsOnProvider(name) {
		return refused("a serving-mode switch takes the model down: it runs never from a session that runs on it (%s); ask the supervisor to switch it", name)
	}

	// A node stands held against a switch by a constraint the caller does
	// not hold itself, and only where the mode the switch runs into
	// occupies the node. The node whose claim or release runs the switch
	// right now is claimed by that very claimer: its own switch is never
	// held back by its own node.
	held := []string{}
	for _, n := range a.serveModeHeldNodes() {
		if n == a.serveModeSwitching {
			continue
		}
		if !slices.Contains(p.ServeModeModes[mode].Nodes, n) {
			continue
		}
		held = append(held, n)
	}
	if len(held) > 0 {
		err := refused("%s: node %s is held exclusively: %s occupies it and never switches in while the constraint holds", name, strings.Join(held, ", "), mode)
		a.recordRefused(name, mode, err)
		return err
	}
	unlock, err := a.takeServeModeLock(name)
	if err != nil {
		return err
	}
	defer unlock()
	if a.serveModeSwitchHeld != nil {
		a.serveModeSwitchHeld(name)
	}
	if a.serveModeLockStandsHere == nil {
		a.serveModeLockStandsHere = map[string]bool{}
		a.serveModeSwitchingTo = map[string]string{}
	}
	if a.serveModeLockStandsHere[name] {
		return refused("another serving-mode switch of %s runs; only one runs at a time", name)
	}
	a.serveModeLockStandsHere[name] = true
	if a.serveModeSwitchingTo == nil {
		a.serveModeSwitchingTo = map[string]string{}
	}
	a.serveModeSwitchingTo[name] = mode
	defer func() {
		a.serveModeLockStandsHere[name] = false
		delete(a.serveModeSwitchingTo, name)
	}()

	if timeout <= 0 {
		timeout = p.EffectiveServeModeSwitchTimeout()
	}
	before, err := a.serveModeState(name)
	if err != nil {
		return err
	}
	from := before.Mode
	if from == "" {
		from = a.serveMode(ctx, name, p).Mode
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := a.runServeModeSwitch(runCtx, p, mode)
	if err != nil {
		if ctx.Err() == nil && runCtx.Err() != nil {
			// The switch outlived its own timeout: the command's own
			// context is what expired, and the mode is whatever the
			// switcher left on disk.
			err = fmt.Errorf("the switch timed out after %s: %s", timeout, strings.Join(p.ServeModeSwitch, " "))
		}
		// The mode stands as the command reports it; the failed switch is
		// logged and stays pending until the reason is gone.
		a.logSwitch(name, from, mode, err)
		return fmt.Errorf("switching %s to %s: %w", name, mode, err)
	}
	if err := a.forgetCachedServeMode(name); err != nil {
		return err
	}
	r := a.serveMode(ctx, name, p)
	if r.Err != "" {
		a.logSwitch(name, from, mode, errors.New("the mode after the switch reads unknown: "+r.Err))
		return fmt.Errorf("switching %s to %s: the mode reads unknown (%s)", name, mode, r.Err)
	}
	if !strings.EqualFold(r.Mode, mode) {
		a.logSwitch(name, from, mode, fmt.Errorf("the reader reports %s, not the mode asked for", r.Mode))
		return refused("switching %s to %s: the reader reports %s", name, mode, r.Mode)
	}
	a.logSwitch(name, from, mode, nil)
	// The switch ran: the machine's record is rebased on what the model
	// runs now, and the drain it ran into is settled — no request outlives
	// the switch that ran it.
	if stS, errS := a.serveModeState(name); errS == nil && (stS.Mode != r.Mode || stS.Pending != "") {
		stS.Mode, stS.Pending, stS.Reason, stS.Node = r.Mode, "", "", ""
		a.writeServeModeState(name, stS)
	}
	if pndS, errS := a.pendingServeMode(name); errS == nil && (strings.EqualFold(pndS.Pending, mode) || pndS.Node == "") {
		a.clearPendingServeMode(name)
	}
	_ = out
	return nil
}

// callerRunsOnProvider reports whether the calling session runs on the
// self-hosted provider: the switch that takes the model down runs never
// from one of its own sessions.
func (a *app) callerRunsOnProvider(name string) bool {
	st, err := a.store.Read()
	if err != nil {
		return false
	}
	me, err := a.caller()
	if err != nil || me.Name == "" {
		return false
	}
	k := a.liveCapacity()
	sessions, _, err := a.sessions()
	if err != nil {
		sessions = nil
	}
	pa := countProviderAgents(st, sessions, k, a.nowOrNow())
	return slices.Contains(pa[name].Busy, me.Name)
}

// takeServeModeLock takes the machine's one-switch-at-a-time lock for the
// provider; the unlock it returns frees it.
func (a *app) takeServeModeLock(name string) (func(), error) {
	if a.store == nil {
		return func() {}, nil
	}
	path := filepath.Join(a.store.Dir(), serveModeLockFile(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	unlock := func() { _ = os.Remove(path) }
	take := func() (bool, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // the lock file beside the state, its own name
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return false, nil
			}
			return false, err
		}
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Close()
		return true, nil
	}
	ok, err := take()
	if err != nil {
		return nil, err
	}
	if ok {
		return unlock, nil
	}
	// A switch whose process is gone left its lock: reap it, so a crashed
	// switch never holds the machine's switches forever. The retry runs at
	// most once: a switch that took the lock beside the reap keeps it, and
	// this caller stands refused like any second switch.
	if raw, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // the lock file this process just found
		if pid, cerr := strconv.Atoi(strings.TrimSpace(string(raw))); cerr == nil && !proc.Alive(pid) {
			_ = os.Remove(path)
			if ok, err := take(); err != nil {
				return nil, err
			} else if ok {
				return unlock, nil
			}
		}
	}
	return nil, refused("another serving-mode switch of %s runs; only one runs at a time", name)
}

// runServeModeSwitch runs the platform's own switch command with the wanted
// mode as its argument; the seam lets a test run its own switcher.
func (a *app) runServeModeSwitch(ctx context.Context, p config.Provider, mode string) (string, error) {
	if a.switchServeModeRun != nil {
		return a.switchServeModeRun(ctx, p.ServeModeSwitch, mode)
	}
	return runServeModeSwitchCommand(ctx, p, mode, "", nil)
}

// runServeModeSwitchCommand runs the configured switch command with the
// wanted mode as its argument, in dir (the working directory, empty for the
// caller's) with env added to the process environment.
func runServeModeSwitchCommand(ctx context.Context, p config.Provider, mode, dir string, env []string) (string, error) {
	c := exec.CommandContext(ctx, p.ServeModeSwitch[0], append(slices.Clone(p.ServeModeSwitch[1:]), mode)...) //nolint:gosec // the configured switch command
	// The switch's own timeout must end its process: a switcher that
	// leaves a child holding the mode would keep the machine's one-switch
	// lock past the timeout, and the retry would wait on the orphan.
	armServeModeSwitch(c)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if dir != "" || len(env) > 0 {
		c.Dir = dir
		c.Env = append(os.Environ(), env...)
	}
	if err := c.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("the switch timed out after %s: %s", p.EffectiveServeModeSwitchTimeout(), strings.Join(p.ServeModeSwitch, " "))
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", truncate(oneLine(msg), 200))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// logSwitch records a switch, run or failed, in the event log.
func (a *app) logSwitch(name, from, mode string, err error) {
	if a.store == nil {
		return
	}
	detail := fmt.Sprintf("%s: %s -> %s", name, from, mode)
	if err != nil {
		_ = a.store.Record(event(a.actor(), verbServeSwitch, "%s: %v", detail, err))
		return
	}
	_ = a.store.Record(event(a.actor(), verbServeSwitch, "%s: switched", detail))
}

// recordRefused says a switch refused by a holding constraint.
func (a *app) recordRefused(name, mode string, err error) {
	if a.store == nil {
		return
	}
	_ = a.store.Record(event(a.actor(), verbServeRefused, "%s: %s: %v", name, mode, err))
}

// actor is the party the serving-mode events carry.
func (a *app) actor() state.Party {
	if me, err := a.caller(); err == nil {
		return me
	}
	return state.Party{Name: project.Name}
}

// forgetCachedServeMode drops the cached reading so the next read sees the
// mode the switch left.
func (a *app) forgetCachedServeMode(name string) error {
	if a.store == nil {
		return nil
	}
	if err := os.Remove(filepath.Join(a.store.Dir(), serveModeFile(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

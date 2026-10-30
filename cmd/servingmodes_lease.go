package cmd

// The lease side of the serving modes: a node a self-hosted mode occupies is
// a lease resource, and claiming it is the generic constraint "needs node N
// exclusively". A claim asks for the switch the constraint needs; beekeeper
// drains the model (the ceiling of the mode without the node holds, the
// sessions that run finish or park), runs the platform's own switch, and
// only then lets the lease be taken. A release asks for the switch back.
// Every failure leaves the mode as the mode reader reports it: a claim or
// release whose switch fails says so, the mode stands, and the request stays
// pending for the watch to retry.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// serveModeHeldNodes are the machine's nodes a constraint holds: the lease
// resources named by any self-hosted provider's modes and held right now.
// A node may be leasable through the machine's configured resources and so
// not listed under the provider's modes: any lease held of a name a mode
// occupies is the generic constraint "needs node N exclusively".
func (a *app) serveModeHeldNodes() []string {
	var out []string
	if holders, err := lease.Dir(a.cfg.LeaseDir).List(); err == nil {
		for _, h := range holders {
			if h.Party().Is(a.actor()) {
				// The node this caller holds itself is no constraint
				// standing behind its own claim or release.
				continue
			}
			out = append(out, h.Env)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// prepareServeModeClaim runs the serving modes up to a claim of the node
// res: a model served in a mode that occupies res drains and switches to
// the mode that runs without it, and only then does the lease get taken. A
// model already in a mode that runs without res needs no switch. The line
// says the switch; the events carry it. A switch that fails refuses the
// claim: the mode stands as the command reports it, and the watch retries
// the pending request.
func (a *app) prepareServeModeClaim(res string) (string, []state.Event, error) {
	k := a.liveCapacity()
	ran := false
	for _, p := range k.Providers {
		for _, m := range p.ServeModeModes {
			if slices.Contains(m.Nodes, res) {
				ran = true
			}
		}
	}
	if !ran {
		return "", nil, nil
	}
	dir := lease.Dir(a.cfg.LeaseDir)
	// The constraint stands as if held while its own claim runs: a claim
	// never stands behind its own node's lease record, and one of a node
	// another holder took stands behind the constraint asked for. The
	// node of a switch running right now, or of the lease this very
	// caller holds, counts as not held: the record of the holder leaving
	// is no constraint against the switch the release asks for, and the
	// holder's own retry is no constraint against the switch it asks for.
	held := slices.DeleteFunc(a.serveModeHeldNodes(), func(n string) bool { return n == a.serveModeSwitching })
	var line string
	var evs []state.Event
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.HasServeModeSwitch() || len(p.ServeModeModes) == 0 {
			continue
		}
		heldFn := func(mode string) []string {
			// Only the provider's own modes decide which nodes the modes
			// occupy; the lease's directory says which of them stand held.
			// The serving-mode state's node field is not consulted: it
			// names what a drain waits for, not what a constraint holds.
			out := []string{}
			for _, n := range p.ServeModeModes[mode].Nodes {
				if n == res {
					// This claim itself is the constraint: its lease
					// record stands only after the switch it asks for
					// ran, so the mode wanted is the one that runs
					// without the node the claim wants.
					out = append(out, n)
					continue
				}
				if slices.Contains(held, n) || a.nodeHeldByOthers(dir, n) {
					// Another holder's record is the constraint; the node
					// this very caller holds is its own claim's, never a
					// constraint standing behind it.
					out = append(out, n)
				}
			}
			return out
		}
		// The claim stands as if held: the mode without the node is wanted
		// even though the lease's directory does not exist yet. The model
		// is read as it is now: a cached reading of before the constraint
		// before drained the model would hide the switch this claim asks
		// for — and its failure.
		_ = a.forgetCachedServeMode(name)
		r := a.serveMode(context.Background(), name, p)
		// The constraint holds from here on: the ceiling the provider runs
		// with while the claim stands is the one the mode the constraints
		// allow names. The desk starts no new session on a model wider
		// than the mode the node's constraint allows, before any switch
		// has run: a claim of an idle model switches it, and the ceiling
		// of the mode the constraints allow holds from the request on.
		// The drain runs into the widest mode the constraints allow — the
		// mode sustaining the most sessions among those occupying no held
		// node, this claim's node among them. It is the same selection the
		// watch's reconcile and the capacity's constraint ceiling make, so
		// no later tick wants the model wider than this drain left it: a
		// drain to a narrower one would be undone by the next read. Where
		// no mode runs free of the held nodes, the constraint has nowhere
		// to drain to.
		lowest := serveModeWanted(p, heldFn).Mode
		// A constraint lowers the model, never raises it: the machine
		// moves down to the widest mode that runs free of the held nodes
		// and stays below what it ran; the wider mode comes back with the
		// release. A model already running narrower than the constraints
		// allow stays where it is — no claim of a node it does not
		// occupy switches a model up.
		if lowest != "" && r.Mode != "" && ceilingOf(p, lowest) > ceilingOf(p, r.Mode) && !strings.EqualFold(lowest, r.Mode) {
			lowest = r.Mode
		}
		// The machine's record of a drain of this node stands: the drain
		// kept waiting by busy sessions or a failing switch is retried
		// into the very mode it asked for, never raised to a wider one
		// while the constraint stands, and never re-derived against a
		// mode the model has come to run by a hand other than its own.
		// The request file and the machine's record are one request: a
		// record naming the mode the model runs in is settled — that
		// switch ran — and falls with this read. A record naming no node
		// is no drain of this claim: its holder is gone, and the request
		// falls with this read — a claim of the node never waits behind a
		// switch back no constraint holds.
		stP, errP := a.serveModeState(name)
		if errP == nil && stP.Node == res && stP.Pending != "" {
			if _, ok := p.ServeModeCeiling(stP.Pending); ok {
				if strings.EqualFold(stP.Pending, r.Mode) {
					a.clearPendingServeMode(name)
					stP.Pending, stP.Reason, stP.Node = "", "", ""
					a.writeServeModeState(name, stP)
				} else {
					lowest = stP.Pending
				}
			}
		}
		want := serveModeTarget{Mode: lowest}
		pnd, pndErr := a.pendingServeMode(name)
		// The model already runs the mode a drain would run into and no
		// drain of this node waits: this claim's own drain is done, and
		// the node is granted with the model as it stands. No claim ever
		// asks for a wider mode than the constraints allow: the mode of
		// the model is the machine's, not this lease's to widen.
		if strings.EqualFold(r.Mode, want.Mode) && (pndErr != nil || pnd.Node != res) {
			want.Mode = r.Mode
		}

		// The machine's record of a drain of this node is retried as it
		// stands: a failed switch keeps its request, and the claim of the
		// same node asks for the very switch the watch would run.
		if stR, errR := a.serveModeState(name); errR == nil && stR.Node == res && stR.Pending != "" {
			if _, ok := p.ServeModeCeiling(stR.Pending); ok && !strings.EqualFold(stR.Pending, r.Mode) {
				want.Mode = stR.Pending
			}
		}
		// A drain of a mode that runs free of the claimed node is no drain
		// of this claim, and a drain of a mode still occupying it is
		// retried above as it stands: nothing here drops a request the
		// watch is working on.
		// A drain already asked for for another node's constraint stands
		// only while its constraint holds: the node of a request no lease
		// holds any more names a drain whose reason fell, and the mode it
		// named would hold the machine narrower than the constraints
		// allow. The model runs the widest mode the constraints allow —
		// the same selection everywhere.
		if pnd, err := a.pendingServeMode(name); err == nil && pnd.Node != "" && pnd.Pending != "" && pnd.Node != res {
			if held, gerr := dir.Get(pnd.Node); gerr == nil && held != nil {
				if c, ok := p.ServeModeCeiling(pnd.Pending); ok && want.Mode != "" && c < p.ServeModeModes[want.Mode].Sessions {
					want.Mode = pnd.Pending
				}
			}
		}
		if want.Mode == "" {
			return "", nil, refused("no serving mode of %s runs without node %s: the constraint has nowhere to drain to", name, res)
		}
		// A claim of the node a pending switch drains for stands behind
		// the constraint already asked for: the mode it drains into
		// would occupy the very node the new claim wants. A pending
		// switch the mode already runs is no drain, and one of a mode
		// that runs without the node is this claim's own kind of
		// request: neither blocks it.
		// Another holder's constraint on the node stands: this claim has
		// no switch of its own to make — the mode already runs without
		// the node, and the drain belongs to the holder's request. The
		// claim waits behind the constraint the holder asked for; the
		// request is recorded for the holder's claim to run.
		if strings.EqualFold(r.Mode, want.Mode) {
			// Another holder's constraint on the node stands and the model
			// already runs the mode the node is free of: this claim has
			// no switch of its own to make, and waits behind the
			// constraint the holder of the node asked for.
			if a.nodeHeldByOthers(dir, res) {
				return "", nil, refused("%s: node %s waits behind the constraint already asked for (the model runs %s)", name, res, r.Mode)
			}
			continue
		}
		// This claim's own request stands: a pending switch left by an
		// earlier holder must not hold the claim behind it, and the
		// switch below re-records what this constraint asks for. A request
		// naming a mode that occupies the node is the exception: it is a
		// drain still waiting, and the claim of the node it drains into
		// waits behind it — whoever asked for the drain, and with the
		// request kept for the watch to retry.
		pndOwn, errOwn := a.pendingServeMode(name)
		drainWaits := errOwn == nil && pndOwn.Node == res && pndOwn.Pending != "" &&
			slices.Contains(p.ServeModeModes[pndOwn.Pending].Nodes, res) && !strings.EqualFold(r.Mode, pndOwn.Pending)
		if a.nodeHeldByOthers(dir, res) {
			// Another holder's constraint on the node stands: its request
			// is the machine's, never this claim's to clear. A drain of
			// the node the constraints now ask for — the mode the model
			// runs is wider than the constraint of the holder allows —
			// is that request too: this claim stands behind it and asks
			// for it to be recorded.
			drainWaits = (pndOwn.Pending != "" && pndOwn.Node == res && !strings.EqualFold(r.Mode, pndOwn.Pending)) ||
				(want.Mode != "" && !strings.EqualFold(r.Mode, want.Mode) && len(heldFn(want.Mode)) == 0)
			if pndOwn.Pending == "" || pndOwn.Node != res {
				if w := want.Mode; w != "" && !strings.EqualFold(r.Mode, w) {
					why := "the constraint on " + res + " drains the model"
					a.markServeModeDrain(name, res, r.Mode, w, why)
					a.writePendingServeMode(name, ServeModeState{Mode: r.Mode, Node: res, Pending: w, Since: a.nowOrNow(), Reason: why})
					if err := a.store.Record(event(a.actor(), verbServeDrain, "%s: %s -> %s: %s", name, r.Mode, w, why)); err != nil {
						return "", nil, err
					}
					pndOwn.Pending, pndOwn.Node = w, res
				}
			}
		} else if !drainWaits {
			// No drain of this node waits: a stale request of a node now
			// free falls with this read, and the target is what the
			// constraints allow right now — the widest mode free of the
			// held nodes, never a wider one than the model runs.
			if stR, errR := a.serveModeState(name); (errR != nil || stR.Node != res || stR.Pending == "") &&
				want.Mode != "" && r.Mode != "" && !strings.EqualFold(want.Mode, r.Mode) &&
				ceilingOf(p, want.Mode) > ceilingOf(p, r.Mode) {
				want.Mode = r.Mode
			}
			if pndC, errC := a.pendingServeMode(name); errC != nil || pndC.Node != res ||
				strings.EqualFold(pndC.Pending, r.Mode) {
				a.clearPendingServeMode(name)
			}
		}
		if stR, errR := a.serveModeState(name); errR == nil && stR.Node == res && stR.Pending != "" && !strings.EqualFold(stR.Pending, r.Mode) {
			// A drain of this node already stands in the machine's
			// record — a failed switch the watch retries, or busy
			// sessions draining: the same drain is re-asked, never
			// re-derived against a mode the model has come to run.
			if _, ok := p.ServeModeCeiling(stR.Pending); ok {
				want.Mode = stR.Pending
			}
		}
		if drainWaits {
			// The drain of the node stands: its ceiling is never raised
			// above what it drains to, whoever holds the node. The claim
			// waits on the switch the watch retries; the request stays as
			// recorded.
			return "", nil, refused("%s: node %s waits behind the constraint already asked for (the model drains to %s)", name, res, pndOwn.Pending)
		}
		if busy, ok := a.busyOverCeiling(name, p, want.Mode); ok {
			why := fmt.Sprintf("%d sessions still run; %d drain to %s", len(busy), ceilingOf(p, want.Mode), want.Mode)
			// The drain event rides a drain newly opened: a claim that
			// finds the same drain already asked for waits behind the
			// record, and the machine drained once, not once per claim.
			fresh := true
			a.markServeModeDrain(name, res, r.Mode, want.Mode, why)
			a.writePendingServeMode(name, ServeModeState{Mode: r.Mode, Node: res, Pending: want.Mode, Since: a.nowOrNow(), Reason: why})
			if fresh {
				if err := a.store.Record(event(a.actor(), verbServeDrain, "%s: %s -> %s: %s", name, r.Mode, want.Mode, why)); err != nil {
					return "", nil, err
				}
			}
			// The drain stands: the sessions finish or park and the watch
			// runs the switch; the claim waits for it.
			return "", nil, refused("%s: the model drains to %s (%d sessions at once): %d still run; no session starts on it meanwhile; the constraint on %s waits for the switch", name, want.Mode, ceilingOf(p, want.Mode), len(busy), res)
		}
		// The model drained: the drain is told, then the platform's own
		// switch runs, then the constraint is granted. The node stands
		// claimed by this very claim while its switch runs: the guard of
		// the switch never weighs the node its own claim holds. The drain
		// was told when the claim opened it — the machine drains once and
		// says it once; a claim that finds the model already drained runs
		// the switch without repeating the line.
		if pndG, errG := a.pendingServeMode(name); errG != nil || pndG.Pending == "" ||
			!strings.EqualFold(pndG.Pending, want.Mode) {
			if err := a.store.Record(event(a.actor(), verbServeDrain, "%s: %s -> %s: the constraint on %s drains the model", name, r.Mode, want.Mode, res)); err != nil {
				return "", nil, err
			}
		}
		a.serveModeSwitching = res
		err := a.switchServeMode(context.Background(), name, want.Mode, 0)
		a.serveModeSwitching = ""
		if err != nil {
			// The request stands: the watch retries the switch while the
			// constraint waits; a failed switch keeps the mode as reported.
			// The reader's mode and the node this claim waits for stand in
			// both records, so the retry asks for the same switch.
			a.writeServeModeState(name, ServeModeState{Mode: r.Mode, Node: res, Pending: want.Mode, Reason: err.Error(), Since: a.nowOrNow()})
			a.writePendingServeMode(name, ServeModeState{Mode: r.Mode, Node: res, Pending: want.Mode, Reason: err.Error(), Since: a.nowOrNow()})
			return "", nil, err
		}
		a.clearPendingServeMode(name)
		// The node this claim holds no longer waits for a switch: the
		// drain it asked for ran. The state's node names only what a
		// pending switch still waits for.
		if st, _ := a.serveModeState(name); st.Node == res {
			st.Node = ""
			a.writeServeModeState(name, st)
		}
		line += fmt.Sprintf("; %s switched to %s (ceiling %d)", name, want.Mode, ceilingOf(p, want.Mode))
		// The switch itself is in the event log already: the switch
		// command recorded it there. A second event of the same switch
		// would say the machine switched twice.
	}
	return line, evs, nil
}

// finishServeModeRelease runs the serving modes after the lease of the node
// res released: where no constraint needs res any longer, the model switches
// back to the mode sustaining the most sessions and its ceiling rises again.
// A switch back that fails keeps the mode as it is; the pending request
// stays for the watch. The release of the lease itself never waits on it.
func (a *app) finishServeModeRelease(res string) (string, []state.Event) {
	k := a.liveCapacity()
	dir := lease.Dir(a.cfg.LeaseDir)
	var line string
	var evs []state.Event
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.HasServeModeSwitch() || len(p.ServeModeModes) == 0 {
			continue
		}
		// The lease of res is gone already: this release's own node is no
		// constraint against the switch back it asks for — unless the
		// node stands claimed again by a holder: its constraint holds the
		// model in the narrow mode, and the release keeps it there. A
		// waiting claim of the node is no such holder: its claim asked
		// for the drain and stands behind it, and while the node is free
		// the release switches the model back; the claim of the next
		// drain runs the switch down again.
		want := serveModeWanted(p, func(mode string) []string {
			// The lease of res is gone already: the release's own node is
			// no constraint against the switch back it asks for; any
			// other node a mode occupies and a party other than this
			// caller holds is.
			return slices.DeleteFunc(a.serveModeNodesHeld(p, mode, dir), func(n string) bool {
				return n == res
			})
		})
		if h, herr := dir.Get(res); herr == nil && h != nil {
			// The node stands claimed again by a holder — a session of
			// the same name took it again and was granted: its constraint
			// keeps the model in the mode the constraints allow without
			// the node. The same selection as everywhere: the widest mode
			// occupying no held node, this one held among them.
			if n := serveModeAllowed(p, append(a.serveModeHeldNodes(), res)); n != "" {
				want.Mode = n
			} else {
				want.Mode = ""
			}
		}

		// The model must be read as it is now: the release asks for a
		// switch the very claim before ran, and a cached reading from
		// before it would send the switch back the wrong way.
		_ = a.forgetCachedServeMode(name)
		r := a.serveMode(context.Background(), name, p)
		// A request of a drain still waiting stands: the mode it drains into is
		// what the constraint needs until the constraint falls, and the model
		// comes up only once the last one is gone. The release of the node a
		// drain drains for settles that drain: its constraint is gone, the
		// request falls with it, and the release's own switch back runs
		// here — a request naming the very mode the model runs in is no
		// drain either, and dropped.
		// The request file and the machine's record of a drain are one
		// request: both fall with the constraint that asked for it.
		// The node of a request stands only while a constraint holds it: a
		// request whose node stands free again is the record of a drain the
		// watch still works on, and the release never waits on the switch it
		// asks for — it runs that switch here.
		if pnd, err := a.pendingServeMode(name); err == nil && pnd.Pending != "" {
			heldNode := pnd.Node != ""
			if heldNode {
				if h, gerr := dir.Get(pnd.Node); gerr == nil && h == nil {
					heldNode = false
				}
			}
			// A drain of a node that still stands claimed is the machine's
			// request: its constraint is in want's own derivation — the
			// widest mode occupying no held node — and the release never
			// narrows the model below what the constraints allow.
			if heldNode && strings.EqualFold(pnd.Pending, r.Mode) {
				// The drain ran already: the request names the very mode the
				// model runs in, and falls with this read.
				a.clearPendingServeMode(name)
				if st0, err0 := a.serveModeState(name); err0 == nil && strings.EqualFold(st0.Pending, r.Mode) {
					st0.Pending, st0.Reason, st0.Node = "", "", ""
					a.writeServeModeState(name, st0)
				}
			}
		}
		if pnd, err := a.pendingServeMode(name); err == nil && pnd.Node == res && pnd.Pending != "" {
			// A second claim of the node stands behind this request: the
			// mode it drains into occupies the node the claim wants. The
			// drain of the first holder stands; with this lease gone the
			// switch is asked for right here, and the release of the
			// lease never waits on it.
			if busy, ok := a.busyOverCeiling(name, p, pnd.Pending); ok {
				line += fmt.Sprintf("; %s drains to %s (%d sessions at once): %d still run", name, pnd.Pending, ceilingOf(p, pnd.Pending), len(busy))
				continue
			}
			a.serveModeSwitching = res
			err := a.switchServeMode(context.Background(), name, pnd.Pending, 0)
			a.serveModeSwitching = ""
			if err != nil {
				// The mode stands as the command reports it; the request
				// stays pending, and both the watch and the next read
				// retry it.
				line += fmt.Sprintf("; %s does not switch to %s yet (%v): the mode stands as reported", name, pnd.Pending, err)
				continue
			}
			line += fmt.Sprintf("; %s switched to %s (ceiling %d)", name, pnd.Pending, ceilingOf(p, pnd.Pending))
			// The switch is in the event log already: the switch command
			// recorded it. One event per switch, wherever it ran.
			continue
		}
		// No constraint waits on the node: this is the switch back the
		// release asks for. The node of a request that is not this one
		// (the mode already stands, or nothing is pending) never holds a
		// later claim: it is dropped with the request.
		if pnd, err := a.pendingServeMode(name); err == nil && (pnd.Node == "" || pnd.Node == res || strings.EqualFold(r.Mode, pnd.Pending)) {
			a.clearPendingServeMode(name)
		}
		if want.Mode == "" {
			continue
		}
		if strings.EqualFold(r.Mode, want.Mode) {
			continue
		}

		a.serveModeSwitching = res
		err := a.switchServeMode(context.Background(), name, want.Mode, 0)
		a.serveModeSwitching = ""
		if err != nil {
			// A switch back of a release that failed stands as the
			// request of this node's constraint: the node names what the
			// retry waits for, and the watch runs it.
			st := ServeModeState{Mode: r.Mode, Node: res, Pending: want.Mode, Reason: err.Error(), Since: a.nowOrNow()}
			a.writeServeModeState(name, st)
			a.writePendingServeMode(name, st)
			line += fmt.Sprintf("; %s does not switch back to %s yet (%v): the mode stands as reported", name, want.Mode, err)
			continue
		}
		a.clearPendingServeMode(name)
		line += fmt.Sprintf("; %s switched back to %s (ceiling %d)", name, want.Mode, ceilingOf(p, want.Mode))
		// The switch back is in the event log already: the switch command
		// recorded it. One event per switch, wherever it ran.
	}
	return line, evs
}

// busyOverCeiling are the provider's busy sessions when they stand over the
// ceiling of the mode to switch into — the sessions a drain waits for.
func (a *app) busyOverCeiling(name string, p config.Provider, mode string) ([]string, bool) {
	busy, err := a.busyOnProvider(name)
	if err != nil {
		return nil, false
	}
	limit, ok := p.ServeModeCeiling(mode)
	return busy, ok && len(busy) > limit
}

func ceilingOf(p config.Provider, mode string) int {
	if c, ok := p.ServeModeCeiling(mode); ok {
		return c
	}
	return p.Ceiling
}

// markServeModeDrain records the pending switch and its reason beside the
// provider's mode; node is the constraint the drain waits for.
func (a *app) markServeModeDrain(name, node, from, to, why string) {
	s, err := a.serveModeState(name)
	if err != nil {
		return
	}
	if s.Mode == "" {
		s.Mode = from
	}
	// A drain without a node is no longer this claim's: the node names
	// only the constraint the pending switch waits for now.
	s.Node = node
	s.Pending, s.Reason, s.Since = to, why, a.nowOrNow()
	a.writeServeModeState(name, s)
}

// serveModeStatusLine is the serving modes for beekeeper status: the mode
// each self-hosted provider runs in with the sessions it sustains, each node
// constraint held, and the switch or drain waiting on them.
func (a *app) serveModeStatusLine() string {
	k := a.liveCapacity()
	dir := lease.Dir(a.cfg.LeaseDir)
	var parts []string
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.HasServeMode() || len(p.ServeModeModes) == 0 {
			continue
		}
		r := a.serveMode(context.Background(), name, p)
		// The nodes a constraint holds: every node of every mode, held by
		// another party. The names the machine's own modes occupy, and any
		// other lease held of them, are the constraint "needs node N
		// exclusively".
		var heldNodes []string
		seen := map[string]bool{}
		for _, m := range p.ServeModeModes {
			for _, n := range m.Nodes {
				if !seen[n] && a.nodeHeldByOthers(dir, n) {
					seen[n] = true
					heldNodes = append(heldNodes, n)
				}
			}
		}
		slices.Sort(heldNodes)
		if r.Mode == "" {
			// The mode could not be read: the line says so and the watch
			// holds the condition; still, what the machine holds and waits
			// for is said, never hidden.
			line := "serving mode unknown"
			if len(heldNodes) > 0 {
				line = strings.Join(heldNodes, ", ") + " held (" + line + ")"
			}
			if pnd, err := a.pendingServeMode(name); err == nil && pnd.Pending != "" {
				c := ceilingOf(p, pnd.Pending)
				line += fmt.Sprintf("; waits to switch to %s (%d session%s)", pnd.Pending, c, sessionS(c))
			}
			parts = append(parts, line)
			continue
		}
		limit := ceilingOf(p, r.Mode)
		line := fmt.Sprintf("serving mode %s (%d session%s)", r.Mode, limit, sessionS(limit))
		if len(heldNodes) > 0 {
			line = strings.Join(heldNodes, ", ") + " held (" + line + ")"
		}
		// The drain or switch that waits: the mode it runs into and why.
		if pnd, err := a.pendingServeMode(name); err == nil && pnd.Pending != "" && !strings.EqualFold(pnd.Pending, r.Mode) {
			c := ceilingOf(p, pnd.Pending)
			line += fmt.Sprintf("; drains to %s (%d session%s)", pnd.Pending, c, sessionS(c))
			if pnd.Reason != "" {
				line += ": " + pnd.Reason
			}
		}
		if sw := a.serveModeSwitchingName(name); sw != "" {
			line += "; switching to " + sw
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}

// serveModeSwitchingName names the mode a switch of this provider runs into
// right now in this process: the machine's lock is what it holds while the
// command runs. "" when no switch of it runs here.
func (a *app) serveModeSwitchingName(name string) string {
	if a.serveModeLockStandsHere != nil && a.serveModeLockStandsHere[name] {
		return a.serveModeSwitchingTo[name]
	}
	return ""
}

// sessionS is the plural s of a session count.
func sessionS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

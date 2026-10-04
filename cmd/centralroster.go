package cmd

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/state"
)

// centralRosterFile is the side file of what the central roster was last
// told of the machine's agents.
const centralRosterFile = "central-roster.json"

// centralKey is the watch's condition of an unreachable central instance.
const centralKey = "central"

// centralProbe is how often a watch with nothing to publish asks whether
// the central instance answers.
const centralProbe = 5 * time.Minute

// publishedAgent is what the central roster knows of one of the machine's
// agents.
type publishedAgent struct {
	Task string `json:"task,omitempty"`
	Done bool   `json:"done,omitempty"`
}

// syncRoster publishes the machine's agents to the central roster on their
// transitions only: registered, a task taken or ended, done, gone. A central
// instance that does not answer is one CENTRAL UNREACHABLE line, and one
// ENDED line once it answers again; what was not published is published
// then.
func (w *watcher) syncRoster(ctx context.Context) {
	st, err := w.store.Read()
	if err != nil {
		return
	}
	host := w.cfg.Identity.Host
	want := map[string]publishedAgent{}
	for _, ag := range st.Agents {
		if cmp.Or(ag.Host, host) == host {
			want[ag.Name] = publishedAgent{Task: ag.Task, Done: ag.Done}
		}
	}
	pub := map[string]publishedAgent{}
	if _, err := w.store.ReadFile(centralRosterFile, &pub); err != nil {
		pub = map[string]publishedAgent{}
	}
	var down *central.Unreachable
	call := func(name, tool string, args map[string]any) bool {
		_, err := w.callCentralRaw(ctx, state.Party{Name: name, Host: host}, tool, args, nil)
		if errors.As(err, &down) {
			return false
		}
		w.check(centralKey+" roster "+name, err != nil, "CENTRAL ROSTER %s: %v", name, err)
		return err == nil
	}
	calls := 0
	for _, name := range slices.Sorted(maps.Keys(want)) {
		p := want[name]
		if old, ok := pub[name]; ok && old == p || down != nil {
			continue
		}
		calls++
		if call(name, "agents_register", map[string]any{paramTask: p.Task, paramDone: p.Done}) {
			pub[name] = p
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pub)) {
		if _, ok := want[name]; ok || down != nil {
			continue
		}
		calls++
		if call(name, "agents_leave", map[string]any{}) {
			delete(pub, name)
		}
	}
	if calls == 0 && time.Since(w.centralChecked) >= centralProbe {
		calls++
		_, err := w.callCentralRaw(ctx, state.Party{Name: w.cfg.Identity.Person, Host: host}, "lease_list", map[string]any{}, nil)
		errors.As(err, &down)
	}
	if calls > 0 {
		w.centralChecked = time.Now()
	}
	_ = w.store.WriteFile(centralRosterFile, pub)
	reason := ""
	if down != nil {
		reason = down.Reason
	}
	w.check(centralKey, down != nil, "CENTRAL UNREACHABLE %s: %s", w.cfg.Central.Context, reason)
}

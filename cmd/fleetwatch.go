package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/lease"
	"golang.org/x/time/rate"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/upgrade"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// fleetWatch has one owner. A round finishes before the next starts; workers
// only perform reads. Applying results in installation order keeps the feed
// independent of network completion order.
type fleetWatch struct {
	limiter    *rate.Limiter
	alertState map[string]*alerts.Installation
	s          *server
	call       func(context.Context, string, map[string]any) (string, error)
	baseline   map[string]map[string]string
	agents     map[string]map[string]fleetAgent
	changed    map[string]time.Time
}

type fleetAgent struct {
	ID          string          `json:"id"`
	State       string          `json:"state"`
	CurrentTask json.RawMessage `json:"current_task"`
}

type fleetRead struct {
	key, raw string
	err      error
}
type fleetReading struct {
	env   v1alpha1.Environment
	reads []fleetRead
}

func (s *server) startFleetWatch(ctx context.Context) error {
	cfg := s.cfg.Serve.FleetWatch
	if cfg.Kubernetes == "" && cfg.Kagent == "" && cfg.Alertmanager == "" {
		return nil
	}
	if cfg.Kubernetes == "" || cfg.Kagent == "" || cfg.Alertmanager == "" || s.cfg.Serve.Muster == "" {
		return errors.New("serve.fleetWatch requires kubernetes, kagent, alertmanager and serve.muster")
	}
	if cfg.Concurrency < 0 || cfg.Concurrency > 32 || cfg.Timeout.Duration < 0 || cfg.MinCallInterval.Duration < 0 || s.cfg.Watch.Interval.Duration < 0 || s.cfg.Watch.StuckAfter.Duration < 0 {
		return errors.New("fleet watch requires positive durations and concurrency between 1 and 32 (zero uses defaults)")
	}
	ex, err := identity.NewExchanger(s.cfg.Serve.Issuer, s.cfg.Serve.Identity)
	if err != nil {
		return err
	}
	w := &fleetWatch{s: s, baseline: map[string]map[string]string{}, agents: map[string]map[string]fleetAgent{}, changed: map[string]time.Time{}}
	w.call = func(ctx context.Context, tool string, args map[string]any) (string, error) {
		token, err := ex.Token(ctx)
		if err != nil {
			return "", err
		}
		return callMuster(ctx, s.cfg.Serve.Muster, token, tool, args)
	}
	pace := cfg.MinCallInterval.Duration
	if pace == 0 {
		pace = 100 * time.Millisecond
	}
	w.limiter = rate.NewLimiter(rate.Every(pace), 1)
	w.alertState = map[string]*alerts.Installation{}
	go w.run(ctx)
	return nil
}

func (w *fleetWatch) run(ctx context.Context) {
	interval := w.s.cfg.Watch.Interval.Duration
	if interval == 0 {
		interval = 30 * time.Second
	}
	// No catch-up rounds after a slow backend: wait a full interval after each round.
	for {
		if err := w.round(ctx); err != nil && ctx.Err() == nil {
			w.s.log.Error("fleet watch", "detail", err.Error())
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *fleetWatch) read(ctx context.Context, env v1alpha1.Environment) fleetReading {
	cfg := w.s.cfg.Serve.FleetWatch
	timeout := cfg.Timeout.Duration
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	result := fleetReading{env: env}
	sources := []struct{ key, tool, resource string }{
		{"agents", cfg.Kagent, ""}, {"alerts", cfg.Alertmanager, ""},
		{"templates", cfg.Kubernetes, "agenttemplates"}, {"harnesses", cfg.Kubernetes, "harnesses"},
		{"releases", cfg.Kubernetes, "helmreleases.helm.toolkit.fluxcd.io"},
	}
	for _, kind := range upgrade.Kinds {
		sources = append(sources, struct{ key, tool, resource string }{kind, cfg.Kubernetes, kind})
	}
	for _, source := range sources {
		args := map[string]any{"cluster": cmp.Or(env.Spec.MusterCluster, env.Spec.Installation)}
		if source.resource != "" {
			args["resourceType"] = source.resource
			args["allNamespaces"] = true
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		var raw string
		var err error
		if w.limiter != nil {
			err = w.limiter.Wait(callCtx)
		}
		if err == nil {
			raw, err = w.call(callCtx, source.tool, args)
		}
		cancel()
		result.reads = append(result.reads, fleetRead{source.key, raw, err})
		if ctx.Err() != nil {
			break
		}
	}
	return result
}

func (w *fleetWatch) round(ctx context.Context) error {
	envs, err := w.s.store.Environments()
	if err != nil {
		return err
	}
	n := w.s.cfg.Serve.FleetWatch.Concurrency
	if n == 0 {
		n = 4
	}
	results := make([]fleetReading, len(envs))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(n, len(envs)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				results[i] = w.read(ctx, envs[i])
			}
		}()
	}
	for i := range envs {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	var errs []error
	for _, result := range results {
		if err := w.apply(result); err != nil {
			errs = append(errs, err)
		}
	}
	for _, env := range envs {
		if err := w.expireGrants(env); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (w *fleetWatch) publish(env v1alpha1.Environment, key string, levels map[string]string) error {
	old := w.baseline[key]
	if old == nil {
		old = map[string]string{}
		w.baseline[key] = old
	}
	keys := make([]string, 0, len(levels))
	for k := range levels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if old[k] == levels[k] {
			continue
		}
		ev := state.Event{At: w.s.now().UTC(), By: state.Party{Name: "fleet-watch", Team: env.Spec.Team}, Verb: "watch.fleet", Detail: levels[k]}
		if err := w.s.store.Audit(&env, env.Spec.Team, ev); err != nil {
			return err
		}
		old[k] = levels[k]
	}
	w.baseline[key] = levels
	return nil
}

// list requires an explicit complete list. A malformed or truncated response
// must never turn into AGENT GONE or ALERT RESOLVED.
func fleetList(raw, field string) ([]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return nil, err
	}
	if t := object["truncated"]; len(t) > 0 && string(t) != "null" && string(t) != "false" {
		return nil, errors.New("truncated fleet list")
	}
	value, ok := object[field]
	if !ok || string(value) == "null" {
		return nil, fmt.Errorf("fleet list missing %s", field)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(value, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (w *fleetWatch) apply(r fleetReading) error {
	installation := r.env.Spec.Installation
	var errs []error
	var objects []upgrade.Object
	upgradeOK := true
	upgradeSources := 0
	for _, read := range r.reads {
		if read.err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", installation, read.key, read.err))
			if slices.Contains(upgrade.Kinds, read.key) {
				upgradeOK = false
			}
			continue
		}
		key := installation + "/" + read.key
		var err error
		switch read.key {
		case "agents":
			err = w.applyAgents(r.env, key, read.raw)
		case "alerts":
			err = w.applyAlerts(r.env, key, read.raw)
		case "templates", "harnesses":
			var items []json.RawMessage
			items, err = fleetList(read.raw, "items")
			levels := map[string]string{}
			for _, raw := range items {
				var o struct {
					Metadata struct{ Name, Namespace string }
					Status   struct {
						Conditions []struct{ Type, Status, Reason, Message string }
					}
				}
				if e := json.Unmarshal(raw, &o); e != nil {
					err = e
					break
				}
				for _, c := range o.Status.Conditions {
					if (c.Type == "Accepted" || c.Type == "Admitted" || c.Type == "Ready") && c.Status == "False" {
						id := o.Metadata.Namespace + "/" + o.Metadata.Name
						levels[id] = "TEMPLATE NOT ADMITTED " + installation + " " + id + ": " + c.Reason
						break
					}
				}
			}
			if err == nil {
				err = w.publish(r.env, key, levels)
			}
		case "releases":
			err = w.applyLanes(r.env, read.raw)
		default:
			upgradeSources++
			var items []json.RawMessage
			items, err = fleetList(read.raw, "items")
			for _, raw := range items {
				var o upgrade.Object
				if e := json.Unmarshal(raw, &o); e != nil {
					err = e
					break
				}
				objects = append(objects, o)
			}
			if err != nil {
				upgradeOK = false
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	if upgradeOK && upgradeSources == len(upgrade.Kinds) {
		if err := w.applyUpgrades(r.env, objects); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (w *fleetWatch) applyAgents(env v1alpha1.Environment, key, raw string) error {
	items, err := fleetList(raw, "instances")
	if err != nil {
		return err
	}
	now := w.s.now()
	stuck := w.s.cfg.Watch.StuckAfter.Duration
	if stuck == 0 {
		stuck = 15 * time.Minute
	}
	current := map[string]fleetAgent{}
	levels := map[string]string{}
	for _, raw := range items {
		var a fleetAgent
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.ID == "" {
			var alternate struct {
				ID string `json:"agent_instance_id"`
			}
			if err := json.Unmarshal(raw, &alternate); err != nil {
				return err
			}
			a.ID = alternate.ID
			if a.ID == "" {
				return errors.New("agent instance missing id")
			}
		}
		var task any
		if len(a.CurrentTask) > 0 {
			if err := json.Unmarshal(a.CurrentTask, &task); err != nil {
				return err
			}
			a.CurrentTask, _ = json.Marshal(task)
		}
		current[a.ID] = a
		old, ok := w.agents[key][a.ID]
		clock := key + "/" + a.ID
		if !ok || old.State != a.State || string(old.CurrentTask) != string(a.CurrentTask) {
			w.changed[clock] = now
		}
		label := ""
		switch strings.ToUpper(a.State) {
		case "FAILED", "ERROR":
			label = "AGENT FAILED"
		case "RUNNING", "WORKING":
			if now.Sub(w.changed[clock]) >= stuck {
				label = "AGENT STUCK"
			}
		}
		if label != "" {
			levels[a.ID] = label + " " + env.Spec.Installation + " " + a.ID
		}
	}
	for id := range w.agents[key] {
		if _, ok := current[id]; !ok {
			levels[id] = "AGENT GONE " + env.Spec.Installation + " " + id
			if h := env.Status.Holder; h != nil && h.Party.Session == id {
				levels["lease/"+id] = "STALE LEASE " + env.Spec.Installation + ": agent " + id + " is gone"
			}
			delete(w.changed, key+"/"+id)
		}
	}
	if err := w.publish(env, key, levels); err != nil {
		return err
	}
	w.agents[key] = current
	return nil
}

func (w *fleetWatch) applyAlerts(env v1alpha1.Environment, key, raw string) error {
	items, err := fleetList(raw, "alerts")
	if err != nil {
		return err
	}
	var input []alerts.Raw
	for _, raw := range items {
		var a alerts.Raw
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.Fingerprint == "" {
			return errors.New("alert missing fingerprint")
		}
		var tenant struct{ Tenant string }
		if err := json.Unmarshal(raw, &tenant); err != nil {
			return err
		}
		a.Fingerprint = tenant.Tenant + "/" + a.Fingerprint
		input = append(input, a)
	}
	cfg := w.s.cfg.Alerts
	rules := alerts.Rules{Collapse: cfg.Collapse, Team: env.Spec.Team, Floors: map[string]string{env.Spec.Installation: env.Spec.AlertFloor}, Flap: alerts.Damper{Changes: cfg.Flap.Changes, Window: cfg.Flap.Window.Duration}}
	previous := w.alertState[key]
	if previous == nil {
		previous = &alerts.Installation{Reachable: true, Alerts: alerts.Set{}}
	}
	lines, next := rules.Step(env.Spec.Installation, previous, alerts.Answer{OK: true, Alerts: input}, w.s.now())
	if len(lines) > 0 {
		recent, err := w.s.store.Events(0, func(e state.Event) bool {
			return !e.At.Before(w.s.now().Add(-30*time.Minute)) && (strings.HasPrefix(e.Verb, "merge.") || e.Verb == "lease.claim")
		})
		if err != nil {
			return err
		}
		for _, line := range lines {
			context := []string{}
			for _, e := range recent {
				context = append(context, e.Verb+" "+e.Detail)
			}
			if len(context) > 20 {
				context = append(context[:20], fmt.Sprintf("%d further changes", len(context)-20))
			}
			if len(context) > 0 {
				line += "; last 30m: " + strings.Join(context, "; ")
			}
			if err := w.s.store.Audit(&env, env.Spec.Team, state.Event{At: w.s.now().UTC(), By: state.Party{Name: "fleet-watch", Team: env.Spec.Team}, Verb: "watch.fleet", Detail: line}); err != nil {
				return err
			}
		}
	}
	w.alertState[key] = next
	return nil
}

func (w *fleetWatch) applyUpgrades(env v1alpha1.Environment, objects []upgrade.Object) error {
	st, err := w.s.store.Read()
	if err != nil {
		return err
	}
	now := w.s.now()
	installation := env.Spec.Installation
	running := upgrade.Detect(objects, now, func(cluster string) bool { return upgrade.HeldClusters(st, now)(installation, cluster) })
	desired := *st
	desired.Holds = slices.Clone(st.Holds)
	upgrade.Reconcile(&desired, installation, running, now, state.Party{Name: "fleet-watch", Team: env.Spec.Team})
	// Kubernetes store writes one Hold per update, retaining optimistic conflict
	// checks. Never reconcile several holds in a single store transaction.
	targets := map[string]bool{}
	for _, h := range st.Holds {
		if strings.HasPrefix(h.Target, upgrade.HoldPrefix+installation+"/") {
			targets[h.Target] = true
		}
	}
	for _, h := range desired.Holds {
		if strings.HasPrefix(h.Target, upgrade.HoldPrefix+installation+"/") {
			targets[h.Target] = true
		}
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, target := range keys {
		err := w.s.store.Update(func(st *state.State) ([]state.Event, error) {
			var before, after *state.Hold
			for _, h := range st.Holds {
				if h.Target == target {
					copy := h
					before = &copy
				}
			}
			copyState := *st
			copyState.Holds = slices.Clone(st.Holds)
			upgrade.Reconcile(&copyState, installation, running, now, state.Party{Name: "fleet-watch", Team: env.Spec.Team})
			st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool { return h.Target == target })
			for _, h := range copyState.Holds {
				if h.Target == target {
					st.Holds = append(st.Holds, h)
					copy := h
					after = &copy
				}
			}
			if reflect.DeepEqual(before, after) {
				return nil, nil
			}
			return []state.Event{{At: now.UTC(), By: state.Party{Name: "fleet-watch", Team: env.Spec.Team}, Verb: "watch.upgrade", Detail: "UPGRADE " + target}}, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *fleetWatch) applyLanes(env v1alpha1.Environment, raw string) error {
	if _, err := fleetList(raw, "items"); err != nil {
		return err
	}
	hrs, err := merge.ParseHelmReleases([]byte(raw))
	if err != nil {
		return err
	}
	lanes, err := w.s.store.Lanes()
	if err != nil {
		return err
	}
	levels := map[string]string{}
	for _, lane := range lanes {
		if lane.Spec.Installation != env.Spec.Installation {
			continue
		}
		err = w.s.store.Update(func(st *state.State) ([]state.Event, error) {
			for i := range st.Merges {
				m := &st.Merges[i]
				if m.Lane != lane.Name || m.Phase != "settling" {
					continue
				}
				cfg := config.Lane{Name: lane.Name, Installation: env.Spec.Installation, Repositories: lane.Spec.Repositories}
				settle := w.s.cfg.Merge.Settle.Duration
				if settle == 0 {
					settle = 5 * time.Minute
				}
				ready, why := merge.Ready(cfg, hrs, m, w.s.now(), settle)
				if ready {
					st.Merges = slices.Delete(st.Merges, i, i+1)
					return []state.Event{{At: w.s.now().UTC(), By: state.Party{Name: "fleet-watch", Team: env.Spec.Team}, Verb: "watch.lane", Detail: "LANE SETTLED " + lane.Name}}, nil
				}
				timeout := w.s.cfg.Merge.SettleTimeout.Duration
				if timeout == 0 {
					timeout = 30 * time.Minute
				}
				if w.s.now().Sub(m.Finished) >= timeout {
					levels[lane.Name] = "LANE STALLED " + lane.Name + ": " + why
				}
			}
			return nil, nil
		})
		if err != nil {
			return err
		}
	}
	return w.publish(env, env.Spec.Installation+"/lanes", levels)
}

func (w *fleetWatch) expireGrants(env v1alpha1.Environment) error {
	return w.s.store.Update(func(st *state.State) ([]state.Event, error) {
		ttl := w.s.cfg.GrantTTL.Duration
		if ttl == 0 {
			ttl = 30 * time.Minute
		}
		pending := lease.Pending(st, env.Spec.Installation, env.Status.Holder != nil, w.s.now(), ttl)
		count := 0
		st.Grants = slices.DeleteFunc(st.Grants, func(g state.Grant) bool {
			if g.Resource != env.Spec.Installation {
				return false
			}
			count++
			return true
		})
		st.Grants = append(st.Grants, pending...)
		if count == len(pending) {
			return nil, nil
		}
		return []state.Event{{At: w.s.now().UTC(), By: state.Party{Name: "fleet-watch", Team: env.Spec.Team}, Verb: "watch.grants", Detail: "GRANT EXPIRED " + env.Spec.Installation}}, nil
	})
}

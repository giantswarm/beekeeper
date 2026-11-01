package cmd

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

type executionStartOptions struct {
	Host, Repository, GitRef, Harness, Model, Mode, Dir, Task  string
	Desktop, Builds, Force, Review, ReviewOfSelfHosted, DryRun bool
	Exclude                                                    []string
}

func (a *app) startSSHExecution(ctx context.Context, name, briefPath string, o executionStartOptions) error {
	host, err := a.executionHost(o.Host)
	if err != nil {
		return err
	}
	if o.Repository == "" || o.GitRef == "" || o.Dir != "" || o.Desktop || o.Mode != "" && o.Mode != "headless" {
		return usageErr("remote starts require --repo and --ref, use headless mode and do not accept --dir or --desktop")
	}
	if o.Harness != harnessAuto && (!execution.SupportedHarness(o.Harness) || !host.Supports(o.Harness)) {
		return refused("execution host %s does not list harness %s; it lists %s", host.Identity, o.Harness, strings.Join(host.Harnesses, ", "))
	}
	if _, ok := host.Repositories[o.Repository]; !ok {
		return refused("repository is not mapped on the selected execution host")
	}
	if err := a.sandboxPaths([]string{briefPath}, nil); err != nil {
		return err
	}
	brief, err := readBrief(briefPath)
	if err != nil {
		return err
	}
	if len(guard.CredentialShapes(brief)) > 0 {
		return refused("brief contains credential-shaped data; provision credentials independently on the execution host")
	}
	k := a.liveCapacity()
	if o.Harness == harnessAuto {
		if len(k.Choices) == 0 {
			return usageErr("--harness auto: no capacity.choices configured")
		}
		if m := strings.TrimSpace(o.Model); m != "" && !choicesHaveModel(k.Choices, m) {
			return usageErr("--harness auto and --model %q: model is not in capacity.choices", m)
		}
		k = filterChoiceModel(k, o.Model)
	}
	by, err := a.caller()
	if err != nil {
		return err
	}
	caps, err := a.probeExecution(ctx, host)
	if err != nil {
		return err
	}
	readings := a.executionReadings(ctx, k)
	var legacy *usageView
	if len(k.Providers) == 0 {
		k, legacy = a.resolveUsage(ctx, k)
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if o.Harness != harnessAuto && caps.Harnesses[o.Harness] == "" {
		return refused("execution host %s does not offer harness %s: %s", host.Identity, o.Harness, caps.Unavailable[o.Harness])
	}
	exclude := append(slices.Clone(o.Exclude), hostUnoffered(caps)...)
	verdict := a.executionCapacity(st, sessions, k, host, caps.Resources, readings, legacy, o.Builds, exclude, startKind(o.Review, o.ReviewOfSelfHosted), o.Harness, o.Model)
	if o.Harness == harnessAuto {
		choice, err := pickAutoChoice(verdict, o.Force)
		if err != nil {
			return err
		}
		o.Harness, o.Model = choice.Harness, choice.Model
	}
	if !execution.SupportedHarness(o.Harness) || slices.Contains(exclude, o.Harness) || o.Model == "" {
		return usageErr("remote %s requires an explicit --model or a configured auto choice this host offers", o.Harness)
	}
	if _, err := gateStart(verdict, o.Force); err != nil {
		return err
	}
	id := execution.ExecutionID(uuid.NewString())
	control, err := execution.NewControlKey()
	if err != nil {
		return err
	}
	task := strings.TrimSpace(o.Task)
	if task == "" {
		task = briefTask(brief)
	}
	workspace := path.Join(host.WorkspaceRoot, string(id), "workspace")
	prompt := strings.ReplaceAll(taskPrompt(brief), scratchpadMark, path.Join(workspace, ".beekeeper-scratch"))
	prompt += "\n\nSSH execution publication: work in this owned workspace. Commit on a feature branch locally, then use `beekeeper agents publish --title <conventional-title> --body-file <absolute-workspace-file>` instead of raw git push or gh pr create. The coordinator chooses an execution-owned branch and returns the PR URL. Publish review fixes with `beekeeper agents publish`; retry identical publication after a lost response to recover the same PR. To merge upstream changes without a GitHub credential on this host, use `beekeeper agents publish --sync-base`, resolve any merge conflict and commit, then publish again. These steps use the coordinator's GitHub account and never require a host write credential. Park on the returned PR for independent review. Merge only with the supervisor's exact-head component authorization.\n"
	r := execution.StartRequest{LeaseRequired: true, Messaging: true, Ref: execution.Ref{Host: host.Identity, Execution: id, Generation: 1}, RequestID: uuid.NewString(), Coordinator: a.cfg.Identity.Host, Name: name, Task: task, Repository: o.Repository, GitRef: o.GitRef, Harness: o.Harness, Model: o.Model, Prompt: prompt, PolicyDigest: host.PolicyDigest, ConversationID: uuid.NewString(), ControlKey: control, Builds: o.Builds}
	if err := r.Validate(); err != nil {
		return usageErr("%v", err)
	}
	if o.DryRun {
		_, err := fmt.Fprintf(a.out, "dry run: %s, host %s, %s at %s, harness %s on %s; no reservation, workspace or process created\n", name, host.Identity, o.Repository, o.GitRef, o.Harness, o.Model)
		return err
	}
	provider, bucket := resolveAgentProviderBucket(k, r.Harness, r.Model)
	err = a.store.Update(func(current *state.State) ([]state.Event, error) {
		if err := a.refuseStopped(current, "starting an SSH execution"); err != nil {
			return nil, err
		}
		// Provider observations are read once before admission. Only counts
		// are recomputed under the store lock; no SSH or paid call holds it.
		if len(k.Providers) > 0 && !reflect.DeepEqual(k.Providers, a.liveCapacity().Providers) {
			return nil, refused("provider configuration changed during admission; retry")
		}
		v := a.executionCapacity(current, sessions, k, host, caps.Resources, readings, legacy, o.Builds, exclude, startKind(o.Review, o.ReviewOfSelfHosted), r.Harness, r.Model)
		over, err := gateStart(v, o.Force)
		if err != nil {
			return nil, err
		}
		for _, ag := range current.Agents {
			if strings.EqualFold(ag.Name, name) && (ag.Task != "" || ag.Execution != nil && executionReserved(current, ag)) {
				return nil, refused("agent name is reserved by an unfinished execution or task; inspect it before replacement")
			}
		}
		if executionHostBusy(current, host.Identity) >= host.MaxBusy {
			return nil, refused("execution host maxBusy reached")
		}
		rec := &state.ExecutionRecord{Handle: execution.Handle{Ref: r.Ref, Repository: r.Repository, Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}, Request: r, RequestDigest: r.Digest(), Selector: o.Host, Phase: executionPending, Provider: provider, Bucket: bucket, ReservedAt: a.now.UTC(), Observation: execution.Observation{ProcessState: execution.Unknown, Reachability: execution.Unknown}}
		s := state.Start{Party: state.Party{Session: r.ConversationID, Name: name, Host: string(host.Identity), Person: by.Person, Team: by.Team}, Mode: "acceptEdits", By: by, At: a.now.UTC(), Harness: r.Harness, Model: r.Model, Execution: rec}
		if _, err := recordStart(current, s, task, briefIssue(brief), func(p state.Party) bool { _, live := claude.Live(sessions, p); return live }); err != nil {
			return nil, err
		}
		ref := r.Ref
		current.Agents[len(current.Agents)-1].Execution = &ref
		events := []state.Event{event(by, "execution.reserved", "%s: host %s, repository %s, provider %s", name, host.Identity, r.Repository, provider)}
		if over != "" {
			events = append(events, event(by, verbStart, "%s: started over %s (--force)", name, over))
		}
		return events, nil
	})
	if err != nil {
		return err
	}
	return a.activateExecution(ctx, r.Ref)
}

func (a *app) executionReadings(ctx context.Context, k config.Capacity) map[string]providerReadingResult {
	readings := map[string]providerReadingResult{}
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if p.SelfHosted() {
			// A self-hosted provider reads no subscription usage; its ceiling
			// may follow the serving mode of the model it serves, and a node
			// constraint holds the model down to the mode free of the node
			// before any switch ran.
			readings[name] = a.serveModeReadings(ctx, name, p)
			continue
		}
		if !p.Enabled() {
			continue
		}
		r, err := a.resolveReading(ctx, usageProviderFile(name), p.Usage)
		readings[name] = providerReadingResult{reading: r, err: err, ok: err == nil}
	}
	return readings
}

func (a *app) executionCapacity(st *state.State, sessions []*claude.Session, k config.Capacity, host execution.Host, resources execution.Resources, readings map[string]providerReadingResult, legacy *usageView, builds bool, exclude []string, kind startFor, harness, model string) *capacityView {
	// A host runs only the harnesses it has an adapter for: its verdict
	// weighs those choices alone, so a choice it cannot run never shows as
	// its room.
	exclude = append(slices.Clone(exclude), hostUnsupportedHarnesses...)
	for _, name := range []string{claudeComm, codex.Harness} {
		if !host.Supports(name) {
			exclude = append(exclude, name)
		}
	}
	// Machine admission is evaluated by the target host's component. This
	// placeholder only carries summary figures into the shared pool builder.
	h := &headroom{AvailableMiB: resources.AvailableMiB, Load1: resources.Load1, Slots: 1, SlotsFree: 1}
	counts := countAgents(st, sessions, a.now)
	var v *capacityView
	if len(k.Providers) > 0 || len(k.Choices) > 0 {
		v = buildProvidersCapacity(counts, countProviderAgents(st, sessions, k, a.now), k, readings, h, false, exclude, kind, a.now)
	} else {
		v = newStartCapacity(counts, k, h, false, kind).withUsage(legacy, a.now)
	}
	v = v.forTarget(k, harness, model)
	v.Builds = builds
	blocks := append([]string(nil), resources.Blocks...)
	if builds {
		blocks = append(blocks, resources.BuildBlocks...)
	}
	if executionHostBusy(st, host.Identity) >= host.MaxBusy {
		blocks = append(blocks, "execution host maxBusy reached")
	}
	if err := resources.Validate(time.Now()); err != nil {
		blocks = append(blocks, err.Error())
	}
	for _, b := range blocks {
		v.Blocks = append(v.Blocks, "host "+string(host.Identity)+": "+b)
	}
	if len(blocks) > 0 {
		v.Room = 0
	}
	return v.stopped(a, st.Stop)
}

// hostUnsupportedHarnesses are the harnesses an execution host has no
// adapter for. Of the others, Claude Code and Codex, a host runs those its
// catalogue entry lists (executionCapacity) and its check offers
// (hostUnoffered). hostRunsHarness is the other side, a harness some host
// runs ("" is Claude Code).
var hostUnsupportedHarnesses = []string{harnessAgy, omp.Harness}

// hostUnoffered are the remote harnesses a host's check did not offer: a
// harness whose version, login or guard failed there is no choice on it.
func hostUnoffered(caps execution.Capabilities) []string {
	var out []string
	for _, name := range []string{claudeComm, codex.Harness} {
		if caps.Harnesses[name] == "" {
			out = append(out, name)
		}
	}
	return out
}

// hostRunsHarness says whether one of hosts can run harness: Claude Code on
// any host, as before, and Codex on a host whose entry lists it.
func hostRunsHarness(harness string, hosts []execution.Host) bool {
	h := strings.TrimSpace(harness)
	if h == "" || h == claudeComm {
		return true
	}
	for _, host := range hosts {
		if execution.SupportedHarness(h) && host.Supports(h) {
			return true
		}
	}
	return false
}

func executionReserved(st *state.State, ag state.Agent) bool {
	if ag.Execution == nil {
		return false
	}
	r, _ := executionRecord(st, *ag.Execution)
	return r == nil || !r.Terminal()
}

func executionHostBusy(st *state.State, host execution.HostID) int {
	n := 0
	for _, s := range st.Starts {
		if r := s.Execution; r != nil && r.Handle.Ref.Host == host && !r.Terminal() {
			n++
		}
	}
	for _, ag := range st.Agents {
		if ag.Execution != nil && ag.Execution.Host == host {
			if rec, _ := executionRecord(st, *ag.Execution); rec == nil {
				n++
			}
		}
	}
	return n
}

func (a *app) activateExecution(ctx context.Context, ref execution.Ref) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	rec, _ := executionRecord(st, ref)
	if rec == nil {
		return refused("execution is not registered")
	}
	host, err := a.executionHost(rec.Selector)
	if err != nil {
		return err
	}
	if host.Identity != ref.Host {
		return refused("execution host identity changed")
	}
	client := a.executionClient(host)
	if rec.StopRequest != nil {
		return a.stopExecution(ctx, ref, rec.StopRequest.Reason)
	}
	if rec.Terminal() {
		return refused("execution is terminal; its workspace and unfinished task are preserved")
	}
	r := rec.Request
	if rec.Phase == executionPending {
		handle, err := client.Start(ctx, r)
		if err != nil {
			a.executionFailure(ref, err)
			return fmt.Errorf("remote preparation failed or was interrupted: %w; recover the recorded execution", err)
		}
		if err := execution.ValidateHandle(host, r, handle); err != nil {
			return err
		}
		if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
			x, i := executionRecord(current, ref)
			if x == nil {
				return nil, refused("registration disappeared")
			}
			if err := a.refuseStopped(current, "activating an SSH execution"); err != nil {
				return nil, err
			}
			if x.StopRequest != nil {
				return nil, refused("execution has a stop request")
			}
			x.Handle, x.Phase, current.Starts[i].Dir = handle, execution.Prepared, handle.Workspace
			return []state.Event{event(current.Starts[i].By, "execution.prepared", "%s: registered handle on %s", current.Starts[i].Name, ref.Host)}, nil
		}); err != nil {
			return err
		}
	}
	// Inspect a recovered execution before any activation attempt. A lost
	// acknowledgement never becomes permission to create another process.
	observation, err := client.Inspect(ctx, ref)
	if err != nil {
		a.executionFailure(ref, err)
		return err
	}
	if err := a.saveExecutionObservation(ref, observation); err != nil {
		return err
	}
	if observation.StopRequested {
		return refused("execution has a remote stop request")
	}
	if observation.ProcessState == execution.Prepared {
		key, err := execution.SigningKey(filepath.Join(a.cfg.StateDir, "execution-coordinator"))
		if err != nil {
			return err
		}
		if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
			x, _ := executionRecord(current, ref)
			if x == nil || x.StopRequest != nil {
				return nil, refused("execution reservation is no longer active")
			}
			if err := a.refuseStopped(current, "activating an SSH execution"); err != nil {
				return nil, err
			}
			x.Phase = execution.Activating
			return nil, nil
		}); err != nil {
			return err
		}
		r.RegistrationReceipt = execution.Sign(r, key, time.Now().UTC())
		if _, err := client.Start(ctx, r); err != nil {
			a.executionFailure(ref, err)
			return fmt.Errorf("activation outcome unconfirmed: %w; inspect or recover this execution", err)
		}
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		observation, err = client.Inspect(deadline, ref)
		if err != nil {
			a.executionFailure(ref, err)
			return err
		}
		if err := a.saveExecutionObservation(ref, observation); err != nil {
			return err
		}
		if observation.Ready && (observation.ProcessState == execution.Running || observation.ProcessState == execution.Exited) {
			if a.json {
				return a.printJSON(observation)
			}
			_, err := fmt.Fprintf(a.out, "started %s: execution %s on %s, workspace %s, unit %s, conversation %s, process %s\n", r.Name, ref.Execution, ref.Host, observation.Handle.Workspace, observation.Handle.Unit, observation.Handle.ConversationID, observation.ProcessState)
			return err
		}
		if observation.ProcessState == execution.Failed || observation.ProcessState == execution.Unknown || observation.StopRequested {
			return refused("execution has no confirmed first task: %s; inspect/recover it; no replacement was started", observation.ProcessState)
		}
		select {
		case <-deadline.Done():
			return refused("first-task acceptance unconfirmed; inspect/recover the registered execution")
		case <-tick.C:
		}
	}
}

func (a *app) executionFailure(ref execution.Ref, failure error) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, nil
		}
		r.Observation = r.Observation.FailedContact(failure, time.Now().UTC())
		return []state.Event{event(st.Starts[i].By, "execution.unconfirmed", "%s: %s", st.Starts[i].Name, failure.Error())}, nil
	})
}

func (a *app) saveExecutionObservation(ref execution.Ref, o execution.Observation) error {
	if o.Handle.Ref != ref {
		return refused("remote observation identity mismatch")
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution is not registered")
		}
		if o.Handle.Unit != execution.UnitName(ref) || o.Handle.ConversationID != r.Request.ConversationID || o.Handle.Repository != r.Request.Repository {
			return nil, refused("remote observation ownership mismatch")
		}
		host, err := a.executionHost(r.Selector)
		if err != nil {
			return nil, err
		}
		if o.Handle.Workspace != "" && o.Handle.Workspace != path.Join(host.WorkspaceRoot, string(ref.Execution), "workspace") {
			return nil, refused("remote observation workspace mismatch")
		}
		if r.Handle.Commit != "" && o.Handle.Commit != r.Handle.Commit {
			return nil, refused("remote observation changed its pinned commit")
		}
		switch o.ProcessState {
		case execution.Preparing, execution.Prepared, execution.Running, execution.Exited, execution.Failed, execution.Unknown:
		default:
			return nil, refused("invalid remote process observation")
		}
		if r.Observation.StopAcknowledged {
			o.StopAcknowledged = true
			o.StopRequested = true
			o.ProcessState = execution.Exited
		}
		o.Reachability, o.ReachabilityAt = execution.Reachable, time.Now().UTC()
		r.Observation = o
		var evs []state.Event
		switch r.Phase {
		case executionPending, execution.Preparing, execution.Prepared, execution.Activating:
			if o.Ready {
				// The line every other harness's start leaves: the supervisor
				// loop reads a claimed next start back from it and takes no
				// second one after a restart. Logged once, on the first
				// observation that confirms the run, start or recovery alike.
				evs = append(evs, event(st.Starts[i].By, "agents.start", "%s: SSH execution %s on %s, workspace %s, %s", st.Starts[i].Name, ref.Execution, ref.Host, o.Handle.Workspace, o.ProcessState))
				r.Phase = execution.Running
			}
		}
		if o.ProcessState == execution.Exited || o.ProcessState == execution.Failed {
			r.Phase = o.ProcessState
		}
		return evs, nil
	})
}

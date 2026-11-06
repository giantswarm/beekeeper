package cmd

import (
	"cmp"
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
	Host, Repository, GitRef, Harness, Model, Effort, Mode, Dir, Task string
	Desktop, Builds, Force, Review, ReviewOfSelfHosted, DryRun        bool
	Exclude                                                           []string
}

func (a *app) startSSHExecution(ctx context.Context, name, briefPath string, o executionStartOptions) error {
	if err := a.cfg.GitHub.WorkerApp.Validate(); err != nil {
		return err
	}
	if a.cfg.GitHub.WorkerApp != nil {
		owner, _, _ := strings.Cut(strings.ToLower(o.Repository), "/")
		if !slices.Contains(a.cfg.GitHub.AllowedOrgs, owner) {
			return refused("task owner needs a github.allowedOrgs entry")
		}
	}
	host, err := a.executionHost(o.Host)
	if err != nil {
		return err
	}
	if err := a.admitExecution(o.Host, host); err != nil {
		return err
	}
	if o.Repository == "" || o.GitRef == "" || o.Dir != "" || o.Desktop || o.Mode != "" && o.Mode != "headless" {
		return usageErr("remote starts require --repo and --ref, use headless mode and do not accept --dir or --desktop")
	}
	if o.Harness != harnessAuto && (!execution.SupportedHarness(o.Harness) || !host.Supports(o.Harness)) {
		return refused("execution host %s does not list harness %s; it lists %s", host.Identity, o.Harness, strings.Join(host.Harnesses, ", "))
	}
	if !host.AdmitsRepository(o.Repository) {
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
	issues := briefIssues(brief)
	for _, iss := range issues {
		if err := a.claimGate(ctx, name, iss, o.Force, o.DryRun); err != nil {
			return err
		}
	}
	if err := a.openPullStartGate(ctx, name, brief, issues, o.Review || o.ReviewOfSelfHosted, o.Force, o.DryRun); err != nil {
		return err
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
	if a.cfg.GitHub.WorkerApp != nil && !caps.GitHub {
		return refused("execution host lacks the per-worker GitHub adapter; update and check the host before starting")
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
	if o.Harness == execution.HarnessAgy {
		if err := refuseAgyEffort(o.Effort); err != nil {
			return err
		}
	}
	if _, err := gateStart(verdict, o.Force); err != nil {
		return err
	}
	if o.Force {
		if _, err := fmt.Fprintln(a.out, "--force overrides coordinator capacity only; execution-host admission still enforces memory, swap, load, disk, build slots (with --builds) and maxBusy, plus authentication, identity, version and policy checks. Remote preparation can still be refused."); err != nil {
			return err
		}
	}
	var bundleDigest string
	if !o.DryRun {
		if bundleDigest, err = a.syncHostBundle(ctx, host); err != nil {
			return err
		}
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
	if o.Review {
		prompt += "\n\nExecution review access: use `beekeeper agents review` to read the assigned pull request and linked issues, and `beekeeper agents review --diff` to read its diff through the coordinator. Post the required review with `beekeeper agents review --body-file <file>`; it returns the exact review URL for your completion report. If the assignment is not available yet, retry after the supervisor finishes recording the start. Review the assigned head; do not publish or park as an author.\n"
	} else if a.cfg.GitHub.WorkerApp != nil {
		prompt += "\n\nWorker GitHub access: use gh for issues, PRs and checks across repositories you can reach. Cross-repository calls name --repo owner/repo; gh api uses repos/owner/repo/.... Commit in the owned workspace (or a cloned repository under .beekeeper-scratch) and push with git push origin HEAD:refs/heads/" + execution.GitHubBranch(execution.Ref{Execution: id, Generation: 1}) + ". Open each PR with gh pr create, then park on its URL for independent review. Merges stay on the coordinator. Never expose credential environment values.\n"
	} else {
		prompt += "\n\nSSH execution publication: work in this owned workspace. Commit on a feature branch locally, then use `beekeeper agents publish --title <conventional-title> --body-file <absolute-workspace-file>` instead of raw git push or gh pr create. The coordinator chooses an execution-owned branch and returns the PR URL. Publish review fixes with `beekeeper agents publish`; retry identical publication after a lost response to recover the same PR. Before the desk review approves the PR, merge upstream changes without a GitHub credential on this host with `beekeeper agents publish --sync-base`, resolve any merge conflict and commit, then publish again; after approval only the merge moves the head (it updates a behind branch itself), so sync the base then only to resolve a conflict the supervisor reports. These steps use the coordinator's GitHub account and never require a host write credential. Park on the returned PR for independent review. Merge only with the supervisor's exact-head component authorization.\n"
	}

	r := execution.StartRequest{BundleDigest: bundleDigest, GitHub: a.cfg.GitHub.WorkerApp != nil, MCP: len(a.cfg.Agents.RemoteMCP) > 0, LeaseRequired: true, Messaging: true, Ref: execution.Ref{Host: host.Identity, Execution: id, Generation: 1}, RequestID: uuid.NewString(), Coordinator: a.cfg.Identity.Host, Name: name, Task: task, Repository: o.Repository, GitRef: o.GitRef, Harness: o.Harness, Model: o.Model, Effort: o.Effort, Prompt: prompt, PolicyDigest: host.PolicyDigest, ConversationID: uuid.NewString(), ControlKey: control, Builds: o.Builds}
	if err := r.Validate(); err != nil {
		return usageErr("%v", err)
	}
	if o.DryRun {
		_, err := fmt.Fprintf(a.out, "dry run: %s, host %s, %s at %s, harness %s on %s; no reservation, workspace or process created\n", name, host.Identity, o.Repository, o.GitRef, o.Harness, o.Model+effortText(o.Effort))
		return err
	}
	provider, bucket := resolveAgentProviderBucket(k, r.Harness, r.Model)
	err = a.store.Update(func(current *state.State) ([]state.Event, error) {
		if err := a.refuseStopped(current, "starting an SSH execution"); err != nil {
			return nil, err
		}
		// Another coordinator can reserve a different route after our catalog
		// read. Serialize route pinning with admission, just like capacity.
		if err := validateRetainedExecutionRoute(current, o.Host, host); err != nil {
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
		endpoint := host.Endpoint
		rec := &state.ExecutionRecord{Endpoint: &endpoint, Handle: execution.Handle{Ref: r.Ref, Repository: r.Repository, Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}, Request: r, RequestDigest: r.Digest(), Selector: o.Host, Phase: executionPending, StartUnsent: true, Provider: provider, Bucket: bucket, ReservedAt: a.now.UTC(), Observation: execution.Observation{ProcessState: execution.Unknown, Reachability: execution.Unknown}}
		s := state.Start{Party: state.Party{Session: r.ConversationID, Name: name, Host: string(host.Identity), Person: by.Person, Team: by.Team}, Mode: "acceptEdits", By: by, At: a.now.UTC(), Harness: r.Harness, Model: r.Model, Effort: r.Effort, Execution: rec}
		if _, err := a.recordStart(current, s, task, strings.Join(issues, ","), func(p state.Party) bool { _, live := claude.Live(sessions, p); return live }); err != nil {
			return nil, err
		}
		ref := r.Ref
		current.Agents[len(current.Agents)-1].Execution = &ref
		events := []state.Event{
			event(by, "execution.reserved", "%s: host %s, repository %s, provider %s", name, host.Identity, r.Repository, provider),
			event(by, verbStart, "%s: kind %s, host %s, harness %s, model %s; %s", name, a.selectionKind, host.Identity, r.Harness, r.Model, cmp.Or(startDecision(verdict), "explicit selection or configured default")),
		}
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
	for _, name := range []string{claudeComm, codex.Harness, harnessAgy, omp.Harness} {
		if !host.Supports(name) {
			exclude = append(exclude, name)
		}
	}
	// Machine admission is evaluated by the target host's component. This
	// placeholder only carries summary figures into the shared pool builder.
	h := &headroom{AvailableMiB: resources.AvailableMiB, Load1: resources.Load1, Slots: 1, SlotsFree: 1}
	counts := countAgents(st, sessions, a.now, a.cfg)
	var v *capacityView
	if len(k.Providers) > 0 || len(k.Choices) > 0 {
		choicesCap := k
		if harness == harnessAuto {
			choicesCap = filterChoiceModel(k, model)
		}
		v = buildProvidersCapacity(counts, countProviderAgents(st, sessions, k, a.now, a.cfg), choicesCap, readings, h, false, exclude, kind, a.now)
		if len(k.Choices) > 0 && len(choicesCap.Choices) == 0 {
			reason := fmt.Sprintf("model %q is not in capacity.choices", strings.TrimSpace(model))
			v.Choice = &choiceView{Review: kind != forWorker, ReviewOfSelfHosted: kind == forReviewOfSelfHosted, Reason: reason}
			v.Room, v.Blocks = 0, []string{"no choice has room: " + reason}
		}
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
	return v.stopped(a, st)
}

// hostUnsupportedHarnesses are the harnesses an execution host has no
// adapter for. hostUnsupportedHarnesses is empty now that every harness has
// a remote adapter; the loop's verdict weighs the harnesses its catalogue
// entry lists (executionCapacity) and its check offers (hostUnoffered).
// hostRunsHarness is the other side, a harness some host runs ("" is Claude Code).
var hostUnsupportedHarnesses = []string{}

// hostUnoffered are the remote harnesses a host's check did not offer: a
// harness whose version, login or guard failed there is no choice on it.
func hostUnoffered(caps execution.Capabilities) []string {
	var out []string
	for _, name := range []string{claudeComm, codex.Harness, harnessAgy, omp.Harness} {
		if caps.Harnesses[name] == "" {
			out = append(out, name)
		}
	}
	return out
}

// hostRunsHarness says whether one of hosts can run harness: Claude Code on
// any host, as before, and Codex, Agy or omp on a host whose entry lists it.
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
	now := time.Now()
	for _, s := range st.Starts {
		if r := s.Execution; r != nil && r.Handle.Ref.Host == host && !r.Terminal() && !executionAgentKept(st, r.Handle.Ref, now) {
			n++
		}
	}
	for _, ag := range st.Agents {
		if ag.Execution != nil && ag.Execution.Host == host && keptBy(st, ag, now) == "" {
			if rec, _ := executionRecord(st, *ag.Execution); rec == nil {
				n++
			}
		}
	}
	return n
}

func executionAgentKept(st *state.State, ref execution.Ref, now time.Time) bool {
	for _, ag := range st.Agents {
		if ag.Execution != nil && *ag.Execution == ref && keptBy(st, ag, now) != "" {
			return true
		}
	}
	return false
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
	if rec.Released() {
		return refused("execution reservation was released before its host acknowledged a start (%s); start a new one", rec.ReleaseReason)
	}
	if rec.Terminal() {
		return refused("execution is terminal; its workspace and unfinished task are preserved")
	}
	r := rec.Request
	if rec.Phase == executionPending {
		var sentAt time.Time
		r, sentAt, err = a.preparePendingExecution(ctx, client, host, rec)
		if err != nil {
			// A start the host never acknowledged holds no slot past its
			// failure: the reservation is released, and a new start runs
			// under the host's current policy.
			released, releaseErr := a.releaseFailedStart(ctx, client, ref, sentAt, err)
			switch {
			case releaseErr != nil:
				return fmt.Errorf("%w; releasing the reservation failed: %v", err, releaseErr)
			case released:
				return fmt.Errorf("%w; the start never reached its host and its reservation is released: it holds no host or desk slot, start anew", err)
			}
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
			if observation, err = a.rebindExecutionPolicy(ctx, host, client, r.Name, ref, observation); err != nil {
				return err
			}
			if a.json {
				return a.printJSON(observation)
			}
			_, err := fmt.Fprintf(a.out, "started %s: execution %s on %s, workspace %s, unit %s, conversation %s, process %s%s\n", r.Name, ref.Execution, ref.Host, observation.Handle.Workspace, observation.Handle.Unit, observation.Handle.ConversationID, observation.ProcessState, effortText(r.Effort))
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
	expiredGitHubLease := false
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, nil
		}
		expiredGitHubLease = r.Request.GitHub && r.Request.LeaseRequired && (r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()))
		wasUnconfirmed := r.Observation.Reachability == execution.Unreachable || r.Observation.LastError != ""
		r.Observation = r.Observation.FailedContact(failure, time.Now().UTC())
		if wasUnconfirmed {
			return nil, nil
		}
		return []state.Event{event(st.Starts[i].By, "execution.unconfirmed", "%s: %s", st.Starts[i].Name, failure.Error())}, nil
	})
	if expiredGitHubLease {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := a.revokeWorkerGitHub(ctx, ref); err != nil {
			_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if _, i := executionRecord(st, ref); i >= 0 {
					return []state.Event{event(st.Starts[i].By, "execution.github-revocation-pending", "%s: %s", st.Starts[i].Name, err)}, nil
				}
				return nil, nil
			})
		}
	}
}

func (a *app) saveExecutionObservation(ref execution.Ref, o execution.Observation) (saveErr error) {
	defer func() {
		if saveErr == nil && (o.ProcessState == execution.Exited || o.ProcessState == execution.Failed || o.StopRequested) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			saveErr = a.revokeWorkerGitHub(ctx, ref)
		}
	}()
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
		if o.LastActive.After(r.LastActive) {
			r.LastActive = o.LastActive
		}
		var evs []state.Event
		switch r.Phase {
		case executionPending, execution.Preparing, execution.Prepared, execution.Activating:
			if o.Ready {
				// The line every other harness's start leaves: the supervisor
				// loop reads a claimed next start back from it and takes no
				// second one after a restart. Logged once, on the first
				// observation that confirms the run, start or recovery alike.
				evs = append(evs, event(st.Starts[i].By, "agents.start", "%s: SSH execution %s on %s, workspace %s, %s%s", st.Starts[i].Name, ref.Execution, ref.Host, o.Handle.Workspace, o.ProcessState, effortText(st.Starts[i].Effort)))
				r.Phase = execution.Running
			}
		}
		if o.ProcessState == execution.Exited || o.ProcessState == execution.Failed {
			r.Phase = o.ProcessState
		}
		return evs, nil
	})
}

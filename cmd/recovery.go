package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/loopworkflow"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	recoveryUnit = "beekeeper-recovery.service"
	// recoveryEvery is the most that passes between two evaluations.
	recoveryEvery = time.Minute
	// recoveryProbeEvery repeats a passed probe; a failed one repeats at once.
	recoveryProbeEvery = 5 * time.Minute
	// recoveryRestartTicks bounds the wait for processes to leave the failed
	// candidate at their quiet moments, before their managed services restart.
	recoveryRestartTicks = 10
	// recoveryFailures are the failed attempts at a rollback before it holds.
	recoveryFailures = 3
	// recoveryPreparedTicks bounds the wait for a replacement a crash ended.
	recoveryPreparedTicks = 10
	// recoveryJournalWait bounds a journal write behind a running updater.
	recoveryJournalWait = 15 * time.Second
	// recoveryLoopRecent is how far before an install a heartbeat says this
	// machine runs a loop.
	recoveryLoopRecent = 24 * time.Hour

	recoveryHoldFile = "recovery-hold.json"
	recoveryLockFile = "recovery-controller.lock"
	verbRecovery     = "self-update.recovery"

	recoveryControllerFlag = "recovery-controller"
	recoveryTargetFlag     = "recovery-target"
	recoveryEveryFlag      = "recovery-every"
	recoveryStepFlag       = "recovery-step"
)

// recoveryHold is what a failed recovery leaves in the state: no worker starts
// and no update installs while Reason stands.
type recoveryHold struct {
	Reason string
	At     time.Time
}

// recoveryProbe drives the candidate through the isolated workflow; the tests
// replace it.
var recoveryProbe = loopworkflow.CandidateProbe

// recoveryGitHubUp says whether GitHub answers at all; the tests replace it. A
// failure streak during an outage is GitHub's, not the candidate's.
var recoveryGitHubUp = func(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, github.APIURL()+"/rate_limit", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < http.StatusInternalServerError
}

// recoveryRecord writes one recovery event; the tests replace it.
var recoveryRecord = func(s state.Store, e state.Event) error { return s.Record(e) }

var (
	recoveryReadProcs     = proc.Read
	recoveryProcessBinary = processBinary
)

// recoveryController supervises one candidate's probation from outside it. It
// runs the retained last-known-good bytes in a unit of its own, so that neither
// the candidate's death nor its loop's restart reaches it. It reads what the
// candidate left behind and writes only events and side files: a release older
// than the live state's writer can do both.
type recoveryController struct {
	a        *app
	store    *state.FileStore
	w        io.Writer
	target   string
	server   string
	scratch  string
	step     time.Duration
	prepared int
	// now and moveProcesses are the clock and restart; the tests replace them.
	now           func() time.Time
	moveProcesses func(ctx context.Context, attempt int) bool
}

// launchRecovery starts the controller of the candidate about to replace
// target. Only the updater's own bytes are known to carry the controller: an
// older recovery target would die on its flags, unnoticed.
func (a *app) launchRecovery(_ context.Context, previous update.Binary, artifact, target string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	digest, err := update.Digest(self)
	if err != nil {
		return err
	}
	if digest != previous.Digest {
		return errors.New("this updater is not the recovery target")
	}
	return a.startRecovery(artifact, target)
}

// startRecovery runs the controller in a unit outside the caller's, or where
// no launcher serves in a session of its own.
func (a *app) startRecovery(artifact, target string) error {
	argv := []string{artifact, selfUpdateCmdName, "--" + recoveryControllerFlag, "--" + recoveryTargetFlag, target}
	if a.cfgPath != "" {
		argv = append(argv, "--"+configFlag, a.cfgPath)
	}
	if plat.Launcher.Available() {
		if err := plat.Launcher.Start(platform.Unit{Name: recoveryUnit, Argv: argv, TermIsSuccess: true}); err == nil {
			return nil
		}
	}
	c := proc.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // the verified recovery binary
	platform.Detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// rearmRecovery starts a supervised candidate's controller again where none
// runs: a reboot ends it, and a watch of either binary brings it back.
func (a *app) rearmRecovery() {
	target, err := loop.ExecutablePath()
	if err != nil || a.cfg == nil || !update.WatchedProbation(target) {
		return
	}
	l := flock.New(filepath.Join(a.cfg.StateDir, recoveryLockFile))
	if ok, err := l.TryLock(); err != nil || !ok {
		return
	}
	_ = l.Unlock()
	if artifact, err := update.RecoveryArtifact(target); err == nil {
		_ = a.startRecovery(artifact, target)
	}
}

// runRecoveryController ticks until the probation at target has settled. Only
// one runs per state directory; a second one has nothing to do.
func (a *app) runRecoveryController(ctx context.Context, w io.Writer, target string, every, step time.Duration) error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return err
	}
	a.store = store
	l := flock.New(filepath.Join(a.cfg.StateDir, recoveryLockFile))
	if ok, err := l.TryLock(); err != nil || !ok {
		return err
	}
	defer func() { _ = l.Unlock() }()
	server, err := os.Executable()
	if err != nil {
		return err
	}
	c := &recoveryController{a: a, store: store, w: w, target: target, server: server, scratch: a.cfg.StateDir, step: step, now: clk.Now}
	c.moveProcesses = c.restart
	t := clk.NewTicker(every)
	defer t.Stop()
	for {
		settled, err := c.tick(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "recovery evaluation: %v\n", err)
		}
		if settled {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *recoveryController) observe(ctx context.Context, change func(*update.Observed)) error {
	ctx, cancel := context.WithTimeout(ctx, recoveryJournalWait)
	defer cancel()
	return update.Observe(ctx, c.target, func(j *update.Journal) error {
		if j.Observed == nil {
			j.Observed = &update.Observed{}
		}
		change(j.Observed)
		return nil
	})
}

// tick is one evaluation. Every decision comes from the durable journal, so a
// controller killed at any point resumes where it stopped.
func (c *recoveryController) tick(ctx context.Context) (bool, error) {
	j, err := update.Inspect(c.target)
	if err != nil {
		return true, c.hold(ctx, "", err)
	}
	switch {
	case j.RolledBack():
		return c.finish(ctx, j)
	case j.Settled():
		return true, nil
	case !j.Pending():
		// A crash between the rename and its journal write reconciles here.
		c.prepared++
		err := c.observe(ctx, func(*update.Observed) {})
		return c.prepared >= recoveryPreparedTicks, err
	case j.RollingBack():
		return c.rollback(ctx, j, j.Quarantine[j.Candidate.Digest])
	}
	o := update.Observed{}
	if j.Observed != nil {
		o = *j.Observed
	}
	o.Elapsed += c.step
	c.probe(ctx, j, &o)
	s := c.signals(ctx, j)
	o.NoteLoop(s)
	verdict, reason := update.Decide(o, s)
	if verdict == update.RollBack && reason == update.MissingHeartbeatReason {
		c.refreshHeartbeat(j, &s)
		if s.Heartbeat || s.Unproven != "" {
			o.NoteLoop(s)
			verdict, reason = update.Decide(o, s)
		}
	}
	if err := c.observe(ctx, func(saved *update.Observed) { *saved = o }); err != nil {
		return false, err
	}
	switch verdict {
	case update.RollBack:
		if reason == update.MissingHeartbeatReason {
			c.refreshHeartbeat(j, &s)
			if s.Heartbeat || s.Unproven != "" {
				o.NoteLoop(s)
				verdict, reason = update.Decide(o, s)
				if err := c.observe(ctx, func(saved *update.Observed) { *saved = o }); err != nil {
					return false, err
				}
				if verdict == update.Promote {
					return c.promote(ctx, j)
				}
				if verdict != update.RollBack {
					return false, nil
				}
			}
			if reason == update.MissingHeartbeatReason {
				reason += ": " + c.loopSilence(j.Candidate.Version, j.InstalledAt)
			}
		}
		return c.rollback(ctx, j, reason)
	case update.Promote:
		return c.promote(ctx, j)
	}
	return false, nil
}

// probe runs the isolated workflow when one is due: at once, after a failure,
// and every recoveryProbeEvery after a pass.
func (c *recoveryController) probe(ctx context.Context, j *update.Journal, o *update.Observed) {
	if o.ProbePassed && o.ProbeFailures == 0 && o.Elapsed-o.ProbedAt < recoveryProbeEvery {
		return
	}
	err := recoveryProbe(ctx, c.target, c.server, c.scratch)
	o.ProbedAt = o.Elapsed
	switch {
	case err == nil:
		o.ProbePassed, o.ProbeFailures, o.LastPassAt = true, 0, o.Elapsed
		o.ProbeFailure = ""
	case errors.Is(err, loopworkflow.ErrInconclusive):
		o.ProbePassed = false
		_, _ = fmt.Fprintf(c.w, "probe of %s: %v\n", j.Candidate.Version, err)
	default:
		o.ProbePassed = false
		// The control: what the last known good cannot do either says nothing
		// about the candidate, whose rollback would fix nothing.
		if cerr := recoveryProbe(ctx, c.server, c.server, c.scratch); cerr != nil {
			_, _ = fmt.Fprintf(c.w, "probe of %s failed, and so did its control: %v\n", j.Candidate.Version, cerr)
			return
		}
		o.ProbeFailures++
		// Probe runs with isolated fixture configuration and no credentials. Keep
		// enough of its cause for the durable rollback line, without log growth.
		o.ProbeFailure = strings.Join(strings.Fields(err.Error()), " ")
		const failureLimit = 512
		if len(o.ProbeFailure) > failureLimit {
			o.ProbeFailure = o.ProbeFailure[:failureLimit] + "..."
		}
		_, _ = fmt.Fprintf(c.w, "probe of %s failed: %v\n", j.Candidate.Version, err)
	}
}

// refreshHeartbeat updates s's heartbeat fields from the latest throughput
// observation and supervisor-loop lifecycle checkpoint.
func (c *recoveryController) refreshHeartbeat(j *update.Journal, s *update.Signals) bool {
	version := loop.NormalizeLoopVersion(j.Candidate.Version)
	now := c.now()
	var obs throughputObservation
	found, err := c.store.ReadFile(throughputObservationFile, &obs)
	if err != nil {
		s.Unproven = "heartbeat unreadable"
		return false
	}
	s.Heartbeat, s.HeartbeatAgo = false, 0
	if found {
		if obs.At.IsZero() {
			s.Unproven = "heartbeat unreadable"
			return false
		}
		// A machine whose loop ran lately owes a heartbeat; an execution host,
		// which runs none, has only its probe.
		s.HeartbeatRequired = j.InstalledAt.Sub(obs.At) < recoveryLoopRecent
		s.Heartbeat = obs.Candidate == version && !obs.At.Before(j.InstalledAt) && !obs.At.After(now) && now.Sub(obs.At) <= 3*throughputCadence
		if s.Heartbeat {
			s.HeartbeatAgo = now.Sub(obs.At)
		}
	}
	// The heartbeat's grace starts with the candidate's own loop: a loop of the
	// old release re-executes only at its next quiet pass, minutes after the
	// install. An unreadable lifecycle leaves the grace counting from install.
	if life, otherAlive, err := c.candidateLoop(j.Candidate.Version, j.InstalledAt); err == nil {
		if life != nil {
			// A newly seen start can predate this tick by wall-clock hours
			// after sleep or controller downtime. Only this tick was observed;
			// never subtract that gap from the candidate's observed grace.
			s.LoopStarted, s.LoopStartedAgo = true, min(now.Sub(life.Started), c.step)
			s.HeartbeatRequired = true
			if life.Exited && life.Code != 0 {
				s.Heartbeat = false
			} else if !life.Exited && life.Phase != "" && life.Phase != loop.LoopPhaseStartup && now.Sub(life.At) <= 3*throughputCadence {
				if !s.Heartbeat || now.Sub(life.At) < s.HeartbeatAgo {
					s.HeartbeatAgo = max(now.Sub(life.At), 0)
				}
				s.Heartbeat = true
			}
		}
		s.LoopPending = otherAlive
	}
	return true
}

// signals reads the candidate's durable health: the same streaks the watches
// evaluate, and the heartbeat its loop writes every pass.
func (c *recoveryController) signals(ctx context.Context, j *update.Journal) update.Signals {
	s := update.Signals{}
	st, err := c.store.Read()
	if err != nil {
		s.Unproven = "state unreadable"
		return s
	}
	s.Intentional = st.Stop != nil
	version := loop.NormalizeLoopVersion(j.Candidate.Version)
	if !c.refreshHeartbeat(j, &s) {
		return s
	}
	evs, err := state.FollowSnapshot(c.store, "recovery-throughput-outcomes", "v1", func(e state.Event) bool {
		return e.Verb == throughputOutcomeVerb
	})
	if err != nil {
		s.Unproven = "events unreadable"
		return s
	}
	var outcomes []throughputOutcome
	for _, e := range evs {
		if e.At.Before(j.InstalledAt) {
			continue
		}
		var o throughputOutcome
		if err := json.Unmarshal([]byte(e.Detail), &o); err != nil {
			s.Unproven = "events unreadable"
			return s
		}
		if o.Candidate == version {
			outcomes = append(outcomes, o)
		}
	}
	// Re-check heartbeat after reading events so a candidate checkpoint that
	// landed during the event scan is never missed by this tick.
	if !c.refreshHeartbeat(j, &s) {
		return s
	}
	now := c.now()
	// Streaks only: a loop that died must not read as unknown here.
	status, _ := evaluateThroughput(now, time.Hour, throughputObservation{At: now, Candidate: version}, outcomes, false)
	switch {
	case status == unknownState:
		s.Unproven = "an operation's outcome is unknown"
	case strings.HasPrefix(status, "failed-"):
		if recoveryGitHubUp(ctx) {
			s.Failure = status
		} else {
			s.Unproven = "GitHub unavailable"
		}
	}
	return s
}

func (c *recoveryController) rollback(ctx context.Context, j *update.Journal, reason string) (bool, error) {
	rctx, cancel := context.WithTimeout(ctx, recoveryJournalWait)
	defer cancel()
	if err := update.Rollback(rctx, c.target, reason, c.store.RecoverVersion); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return false, err // An updater holds the lock: the next tick retries.
		}
		failures := 0
		if serr := c.observe(ctx, func(o *update.Observed) { o.RollbackFailures++; failures = o.RollbackFailures }); serr != nil {
			return true, c.hold(ctx, j.Candidate.Version, errors.Join(err, serr))
		}
		if failures < recoveryFailures {
			return false, err
		}
		return true, c.hold(ctx, j.Candidate.Version, err)
	}
	c.a.skipRelease(j.Candidate.Version, reason)
	if err := c.observe(ctx, func(o *update.Observed) { o.Reason = reason }); err != nil {
		return false, err
	}
	next, err := update.Inspect(c.target)
	if err != nil {
		return false, err
	}
	return c.finish(ctx, next)
}

// finish tells the person once and moves the processes off the failed
// candidate; both retry on the next tick until they are done.
func (c *recoveryController) finish(ctx context.Context, j *update.Journal) (bool, error) {
	o := update.Observed{}
	if j.Observed != nil {
		o = *j.Observed
	}
	var failed error
	if !o.Notified {
		line := fmt.Sprintf("RELEASE ROLLED BACK: %s failed (%s); restored %s", j.Candidate.Version, o.Reason, j.Previous.Version)
		if failed = c.report(line); failed == nil {
			o.Notified = true
		}
	}
	if !o.Restarted {
		o.Restarts++
		o.Restarted = c.moveProcesses(ctx, o.Restarts)
	}
	// Restoration already verified compatibility under the state lock. End the
	// broad writer ceiling automatically, retaining a durable rejection of the
	// failed version so its surviving processes cannot resume writes.
	if err := c.store.CompleteRollback(j.Previous.Version, j.Candidate.Version); err != nil {
		return false, err
	}
	if err := c.observe(ctx, func(saved *update.Observed) {
		saved.Notified, saved.Restarts, saved.Restarted = o.Notified, o.Restarts, o.Restarted
	}); err != nil {
		return false, err
	}
	return o.Notified && o.Restarted, failed
}

// report files one line for the guide, the person's channel, in the state.
func (c *recoveryController) report(line string) error {
	now := c.now()
	key := "recovery " + now.UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf("%s reported at %s%s%s", loop.LoopParty.Name, now.UTC().Format(time.RFC3339), loop.FileReportSeparator, line)
	if err := recoveryRecord(c.store, event(loop.LoopParty, loop.LoopFileVerb, "[%s] %s", key, body)); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(c.w, line)
	return nil
}

// restart requests cooperative re-execution first, then restarts managed loop
// and standby services that cannot reach a quiet point. Only a subsequent
// process inspection can confirm that the failed binary has gone.
func (c *recoveryController) restart(ctx context.Context, attempt int) bool {
	t, err := recoveryReadProcs()
	if err != nil {
		_, _ = fmt.Fprintf(c.w, "recovery restart: read processes: %v\n", err)
		return false
	}
	due, err := requestRestarts(c.w, c.a.cfg.StateDir, c.target, t, recoveryProcessBinary, c.now(), false)
	if err != nil {
		_, _ = fmt.Fprintf(c.w, "recovery restart: %v\n", err)
		return false
	}
	var written time.Time
	if fi, err := os.Stat(c.target); err == nil {
		written = fi.ModTime()
	}
	brokerPending := !written.IsZero() && restartBrokerOn(ctx, c.w, c.target, written, t, false, c.a.cfg.Secret.Session)
	settled := len(due) == 0 && !brokerPending
	if !plat.Launcher.Available() {
		return settled
	}
	for _, service := range []struct {
		unit string
		due  func(*proc.Process) bool
	}{
		{loopUnit, isSupervisorLoop},
		{standbyUnit, func(p *proc.Process) bool {
			return slices.Contains(p.Args, watchName) && slices.Contains(p.Args, "--standby")
		}},
	} {
		matched := slices.ContainsFunc(due, service.due)
		known := false
		if err := c.observe(ctx, func(o *update.Observed) {
			known = slices.Contains(o.RestartServices, service.unit)
			if matched && !known {
				o.RestartServices = append(o.RestartServices, service.unit)
				known = true
			}
		}); err != nil {
			_, _ = fmt.Fprintf(c.w, "recovery restart journal: %v\n", err)
			return false
		}
		if !known {
			continue // Never start a service this recovery has not seen running.
		}
		status := plat.Launcher.State(ctx, service.unit)
		if status == "" || status == unitNotFound {
			settled = false
			continue
		}
		if unitComingUp(status) {
			if attempt < recoveryRestartTicks || !matched {
				continue
			}
			if err := plat.Launcher.Stop(ctx, service.unit); err != nil {
				_, _ = fmt.Fprintf(c.w, "recovery restart %s: %v\n", service.unit, err)
				settled = false
				continue
			}
		}
		settled = false
		if err := plat.Launcher.Resume(ctx, service.unit); err != nil {
			_, _ = fmt.Fprintf(c.w, "recovery resume %s: %v\n", service.unit, err)
		}
	}
	return settled
}

func (c *recoveryController) promote(ctx context.Context, j *update.Journal) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, recoveryJournalWait)
	defer cancel()
	if err := update.CompleteProbation(pctx, c.target, j.Candidate.Version); err != nil {
		return false, err
	}
	if err := c.store.ResumeRecoveredWrites(j.Candidate.Version); err != nil {
		return true, err
	}
	return true, recoveryRecord(c.store, event(loop.LoopParty, verbRecovery, "%s passed its probation: last known good", j.Candidate.Version))
}

// hold is a recovery that cannot go on: nothing is destroyed, no update
// installs and no worker starts until an operator clears it.
func (c *recoveryController) hold(ctx context.Context, version string, cause error) error {
	line := fmt.Sprintf("RELEASE RECOVERY FAILED: %s stays installed, updates and starts are held: %v", cmpVersion(version), cause)
	errs := []error{c.store.WriteFile(recoveryHoldFile, recoveryHold{Reason: line, At: c.now()})}
	if version != "" {
		errs = append(errs, c.observe(ctx, func(o *update.Observed) { o.Held = line }))
	}
	return errors.Join(append(errs, c.report(line))...)
}

func cmpVersion(version string) string {
	if version == "" {
		return "the installed release"
	}
	return version
}

// recoveryHeld is why a failed recovery holds this machine, "" when none does.
func (a *app) recoveryHeld() string {
	var h recoveryHold
	if found, err := a.store.ReadFile(recoveryHoldFile, &h); err != nil || !found {
		return ""
	}
	return h.Reason
}

// clearRecoveryHold is self-update --clear-recovery-hold, once an operator has
// dealt with what stopped the recovery.
func (a *app) clearRecoveryHold(ctx context.Context, path string) error {
	if err := a.loadConfig(); err != nil {
		return err
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return err
	}
	if err := update.ClearHold(ctx, path); err != nil {
		return err
	}
	return store.WriteFile(recoveryHoldFile, recoveryHold{})
}

package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
	"github.com/giantswarm/beekeeper/internal/transcripts"
)

const progressEvent = "supervisor.board-progress"

// First slice: the first five actionable In Progress candidates, with P1s
// first and the configured board order as the tie breaker. Up Next stays
// with normal intake. Polling, commentary and a process heartbeat are not
// progress: only tool calls, heads, check results and review state are.
func loopBoardProgress(l *loop.Loop, ctx context.Context, st *state.State, in superloop.Input, unread map[string]bool) (bool, error) {
	a := asApp(l.Host.SnapshotAtNow())
	if a.cfg.Board.Owner == "" || a.cfg.Board.Project == 0 || !slices.ContainsFunc(a.cfg.Board.Order, func(step config.BoardStep) bool {
		return slices.ContainsFunc(step.Status, func(status string) bool { return strings.HasPrefix(strings.ToLower(status), "in progress") })
	}) {
		return false, nil
	}
	var saved loop.BoardProgress
	if _, err := a.store.ReadFile(loop.BoardProgressFile, &saved); err != nil {
		return false, err
	}
	if saved.Entries == nil {
		saved.Entries = map[string]loop.ItemProgress{}
	}
	if saved.BoardAt.IsZero() || a.now.Sub(saved.BoardAt) >= time.Minute {
		var snap *board.Snapshot
		err := a.LoopGitHub(ctx, "board progress", func() error {
			var err error
			snap, err = a.boardClient().Read(ctx, a.now)
			return err
		})
		if err != nil {
			return false, err
		}
		if len(snap.Missing) > 0 {
			return false, fmt.Errorf("board progress unread: %s", strings.Join(snap.Missing, "; "))
		}
		saved.Items = nil
		for _, c := range board.Rank(snap, a.cfg.Board, a.now) {
			if strings.HasPrefix(strings.ToLower(c.Status), "in progress") && c.Skip == "" && len(c.Missing) == 0 {
				saved.Items = append(saved.Items, c)
			}
		}
		slices.SortStableFunc(saved.Items, func(x, y board.Candidate) int {
			return cmp.Compare(progressPriority(x), progressPriority(y))
		})
		saved.Items = saved.Items[:min(5, len(saved.Items))]
		saved.BoardAt = a.now
		for ref := range saved.Entries {
			if !slices.ContainsFunc(saved.Items, func(c board.Candidate) bool { return c.Ref == ref }) {
				delete(saved.Entries, ref)
			}
		}
	}
	sessions, _, err := freshSessions(a)
	if err != nil && !platform.Missing(err) {
		return false, err
	}
	var stalled []board.Candidate
	for _, c := range saved.Items {
		if heldByDecision(st.Notes, c.Item) != "" || repositoryStartReason(st, a.now, c.Ref) != "" {
			delete(saved.Entries, c.Ref)
			continue
		}
		signal, known := a.boardProgressSignal(st, sessions, in, unread, c.Ref)
		if !known {
			continue
		}
		p := saved.Entries[c.Ref]
		if p.Changed.IsZero() || signal != p.Signal {
			p = loop.ItemProgress{Signal: signal, Changed: a.now}
		}
		p.Seen = a.now
		saved.Entries[c.Ref] = p
		if p.Stalled(a.now) && (p.Attempt.IsZero() || a.now.Sub(p.Attempt) >= time.Minute) {
			stalled = append(stalled, c)
		}
	}
	if err := a.store.WriteFile(loop.BoardProgressFile, saved); err != nil {
		return false, err
	}
	if a.noStart(st) != "" || l.HoldingStarts() || len(stalled) == 0 || !l.BeginInFlight(loop.LoopFillStream) {
		return false, nil
	}
	defer l.EndInFlight(loop.LoopFillStream)
	for _, c := range stalled {
		// A concurrent action may have staffed this item since observation.
		current, err := a.store.Read()
		if err != nil {
			return false, err
		}
		if a.noStart(current) != "" || heldByDecision(current.Notes, c.Item) != "" || l.RepositoryStart(c.Ref) != nil {
			continue
		}
		p := saved.Entries[c.Ref]
		p.Attempt = a.now
		saved.Entries[c.Ref] = p
		if err := a.store.WriteFile(loop.BoardProgressFile, saved); err != nil {
			return false, err
		}
		acted, reason, err := a.recoverBoardProgress(loop.WithProgressItem(ctx, c.Ref), l, current, sessions, in, c)
		if err != nil {
			reason += ": " + err.Error()
		}
		if acted {
			if err == nil {
				return true, a.store.Record(about(event(loop.LoopParty, progressEvent, "%s: %s", c.URL, reason), c.Ref))
			}
		}
		if reason != "" {
			l.SayOnce("progress held "+c.Ref+reason, "SUPERVISOR PROGRESS WAITS %s: %s", c.URL, reason)
			if p.Reported != reason {
				text := fmt.Sprintf("%s has made no progress since %s; automatic recovery waits: %s", c.URL, p.Changed.UTC().Format(time.RFC3339), reason)
				line := fmt.Sprintf("%s reported at %s%s%s", loop.LoopParty.Name, a.now.UTC().Format(time.RFC3339), loop.FileReportSeparator, text)
				if err := l.Record(superloop.Action{Kind: superloop.KindFile, Key: "board progress " + c.Ref + " " + p.Changed.UTC().Format(time.RFC3339), Ref: c.Ref}, line); err != nil {
					return false, err
				}
				p.Reported = reason
				saved.Entries[c.Ref] = p
				if err := a.store.WriteFile(loop.BoardProgressFile, saved); err != nil {
					return false, err
				}
			}
		}
	}
	return false, nil
}

func progressPriority(c board.Candidate) int {
	if strings.HasPrefix(strings.ToUpper(c.Title), "P1:") || slices.ContainsFunc(c.Labels, func(label string) bool {
		return strings.EqualFold(label, "p1") || strings.EqualFold(label, "priority/p1")
	}) {
		return 0
	}
	return 1
}

func progressPulls(st *state.State, in superloop.Input, ref string) []string {
	var refs []string
	for _, d := range st.Deliveries {
		if strings.EqualFold(d.Ref, ref) {
			refs = append(refs, d.Pulls...)
		}
	}
	for key, p := range in.Pulls {
		if slices.ContainsFunc(p.Closes, func(r string) bool { return strings.EqualFold(r, ref) }) {
			refs = append(refs, key)
		}
	}
	for _, rp := range st.ReviewParks {
		if strings.EqualFold(rp.Serves, ref) {
			refs = append(refs, rp.On)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func progressWorker(st *state.State, ag state.Agent, ref string, pulls []string) bool {
	if ag.Done || ag.Task == "" || roleRun(st, ag.Party) {
		return false
	}
	if strings.EqualFold(loop.LoopServes(st, ag), ref) || strings.EqualFold(loop.LoopTaskServes(ag, ""), ref) {
		return true
	}
	// namedRefs lower-cases; pull refs keep the case GitHub gives the repository.
	inPulls := func(r string) bool {
		return slices.ContainsFunc(pulls, func(p string) bool { return strings.EqualFold(p, r) })
	}
	if ag.Park != nil && inPulls(loop.LoopParkOn(ag.Park)) {
		return true
	}
	return slices.ContainsFunc(namedRefs(ag.Task, ""), inPulls)
}

// Keep only tool metadata, never transcript text or tool arguments, in the
// progress signal. The transcript store already collects remote tool calls.
func (a *app) progressTools(st *state.State, sessions []*claude.Session, ag state.Agent) (string, bool) {
	var turns []transcripts.Turn
	if ag.Execution != nil {
		r, i := executionRecord(st, *ag.Execution)
		if r == nil || r.Observation.Reachability != execution.Reachable {
			return "", false
		}
		meta, _ := a.executionTranscriptMeta(st, st.Starts[i], r)
		var err error
		turns, err = a.transcriptStore().ReadTurns(meta.Key, transcripts.ReadOptions{Tools: true})
		if err != nil {
			return "", false
		}
	} else {
		s, ok := claude.Live(sessions, ag.Party)
		if !ok {
			s = &claude.Session{ID: ag.Session, HostID: ag.HostSession, Transcript: transcriptOf(a.cfg, ag.Session)}
			if h, _, nonClaude := harnessForHost(ag.HostSession); nonClaude {
				for _, ended := range h.Ended(a.cfg, sessions, ag.AssignedAt) {
					if ended.Party().Is(ag.Party) {
						s = ended
						break
					}
				}
			}
		}
		_, activity := harnessForSession(s).ReadTranscript(a, s, a.now)
		return fmt.Sprintf("%s/%s:%s", ag.Session, ag.HostSession, activity.LastTool), true
	}
	var last time.Time
	n := 0
	for _, t := range turns {
		if t.Role == transcripts.RoleTool {
			n++
			last = t.At
		}
	}
	return fmt.Sprintf("%s/%s:%d:%s", ag.Session, ag.HostSession, n, last.Format(time.RFC3339Nano)), true
}

func (a *app) boardProgressSignal(st *state.State, sessions []*claude.Session, in superloop.Input, unread map[string]bool, ref string) (string, bool) {
	pulls := progressPulls(st, in, ref)
	var parts []string
	for _, key := range pulls {
		p, ok := in.Pulls[key]
		if !ok || unread[key] {
			return "", false
		}
		checks := slices.Clone(p.CheckStates)
		slices.Sort(checks)
		parts = append(parts, fmt.Sprintf("%s:%s:%s:%t:%v:%v", key, p.State, p.Head, p.Green, checks, p.Red))
		for _, r := range p.Reviews {
			parts = append(parts, fmt.Sprintf("%s:%s:%s:%s", r.ID, r.Head, r.Verdict, r.At.Format(time.RFC3339Nano)))
		}
		for _, r := range st.DeskReviews {
			if r.Ref == key {
				parts = append(parts, fmt.Sprintf("%s:%s:%s:%v", r.ID, r.Head, r.Released, r.Result != nil))
			}
		}
	}
	for _, ag := range st.Agents {
		if !progressWorker(st, ag, ref, pulls) {
			continue
		}
		tools, known := a.progressTools(st, sessions, ag)
		if !known {
			return "", false
		}
		parts = append(parts, tools)
	}
	slices.Sort(parts)
	raw, _ := json.Marshal(parts)
	return string(raw), true
}

func progressLive(st *state.State, sessions []*claude.Session, ag state.Agent) bool {
	if ag.Execution != nil {
		r, _ := executionRecord(st, *ag.Execution)
		// Unknown is not dead. Only an observed terminal process permits replacement.
		return r == nil || r.Observation.Reachability != execution.Reachable ||
			(r.Observation.ProcessState != execution.Failed && r.Observation.ProcessState != execution.Exited)
	}
	_, live := liveSession(sessions, ag.Party)
	return live
}

func (a *app) recoverBoardProgress(ctx context.Context, l *loop.Loop, st *state.State, sessions []*claude.Session, in superloop.Input, c board.Candidate) (bool, string, error) {
	pulls := progressPulls(st, in, c.Ref)
	// Repair a missing assignment before starting any competing fixer. Its
	// durable supervisor start and pinned commit supply the authority.
	for _, ref := range pulls {
		p := in.Pulls[ref]
		if p.State != superloop.Open {
			continue
		}
		pr, ok := parseRef(ref)
		if !ok {
			continue
		}
		for _, start := range st.Starts {
			pos := strings.LastIndex(start.Name, " r")
			if pos < 0 {
				continue
			}
			round, err := strconv.Atoi(start.Name[pos+2:])
			if err != nil || round < 1 || start.Name != superloop.ReviewerName(ref, round) || start.Kind != capacity.TaskKindReview || !start.By.Is(loop.LoopParty) {
				continue
			}
			if start.Execution == nil || start.Execution.Request.Repository != pr.Repo || start.Execution.Handle.Commit != p.Head || start.Execution.Completed || start.Execution.StopRequest != nil {
				continue
			}
			if slices.ContainsFunc(st.DeskReviews, func(r state.DeskReview) bool {
				return sameReviewSession(r.Reviewer, start.Party) || r.Ref == ref && r.Head == p.Head && r.Round == round && r.Released.IsZero()
			}) {
				continue
			}
			agent := ""
			if rp, ok := st.ReviewParkFor(ref); ok {
				agent = rp.Agent
			}
			act := superloop.Action{Kind: superloop.KindReview, Ref: ref, URL: p.URL, Head: p.Head, Round: round, Agent: agent}
			return true, "assigned stalled review at its pinned head", a.assignExecutionReview(ctx, act)
		}
	}
	var workers []state.Agent
	for _, ag := range st.Agents {
		if progressWorker(st, ag, c.Ref, pulls) {
			workers = append(workers, ag)
			if ag.Park == nil && progressLive(st, sessions, ag) {
				return false, "worker process is live; no automatic replacement", nil
			}
		}
	}
	for _, ag := range workers {
		if ag.Execution != nil {
			if r, _ := executionRecord(st, *ag.Execution); r != nil && r.StopRequest != nil {
				return false, "worker has an explicit stop request", nil
			}
		}
		if ag.Keep != nil || !ag.ContinuationExhausted.IsZero() || parkedOn(waitsOf(st.Records, ag), a.cfg.Guide.Person) != "" {
			return false, "worker has an explicit wait", nil
		}
		if ag.Park != nil && len(ag.Park.Continuation) == 0 {
			continue // explicit parks are settled by their own event
		}
		if ag.Park == nil {
			err := a.store.Update(func(current *state.State) ([]state.Event, error) {
				for i := range current.Agents {
					cur := &current.Agents[i]
					if cur.Is(ag.Party) && cur.Park == nil && !cur.Done && cur.Task == ag.Task && !progressLive(current, sessions, *cur) {
						cur.Park = &state.Park{By: loop.LoopParty, At: a.now, On: c.Ref, RefKind: github.RefIssue, Waits: "failed worker continuation", Continuation: []string{"No progress for 30 minutes; the worker process ended. Continue the recorded task."}}
						if k := current.StartIndex(cur.Party); k >= 0 {
							cur.Park.Worktree = current.Starts[k].Dir
						}
						return []state.Event{about(event(loop.LoopParty, continuationQueuedEvent, "%s: failed worker on %s", ag.Name, c.URL), c.Ref)}, nil
					}
				}
				return nil, refused("worker changed before continuation")
			})
			if err != nil {
				return false, "queue failed worker", err
			}
		}
		current, err := a.store.Read()
		if err != nil {
			return false, "read queued continuation", err
		}
		// The item-scoped context must never reach another item's start.
		current.Agents = slices.DeleteFunc(slices.Clone(current.Agents), func(cur state.Agent) bool { return !cur.Is(ag.Party) })
		if err := l.ContinueParks(ctx, current); err != nil {
			return false, "continue stalled worker", err
		}
		after, err := a.store.Read()
		if err != nil {
			return false, "read continuation outcome", err
		}
		if slices.ContainsFunc(after.Agents, func(cur state.Agent) bool { return cur.Is(ag.Party) }) {
			return false, "continuation still waits on start admission", nil
		}
		return true, "continued stalled worker (pace exception for this item only)", nil
	}
	for _, ref := range pulls {
		p := in.Pulls[ref]
		if p.State != superloop.Open || len(p.Red) == 0 {
			continue
		}
		// A reviewer is also a fixer. Never compete with its live process.
		for _, r := range st.DeskReviews {
			if r.Ref == ref && r.Released.IsZero() && r.Result == nil {
				return false, "assigned review is still open", nil
			}
		}
		act := superloop.Action{Kind: superloop.KindRed, Key: "red " + ref + "@" + p.Head + ": board-progress", Ref: ref, URL: p.URL, Head: p.Head,
			Agent: "Fix stalled " + strings.ReplaceAll(ref, "/", " "), Message: fmt.Sprintf("Continue %s. No progress for 30 minutes; fix the red checks on %s: %v", c.URL, p.URL, p.Red)}
		if err := l.StartFixer(ctx, st, act); err != nil {
			return false, "start fixer for stalled red head", err
		}
		return true, "started fixer for stalled red head", l.Record(act, "started stalled-head fixer")
	}
	if len(workers) == 0 && len(pulls) == 0 {
		// Normal intake never offers an item that board next names an owner
		// for; the pace exception must not start a second worker there either.
		if owner := a.progressOwner(st, sessions, c); owner != "" {
			return false, "served by " + owner + "; no automatic start", nil
		}
		if other, err := a.OtherDeskClaim(ctx, c.Ref); err != nil || other != "" {
			return false, "item claim is unread or held by another desk", err
		}
		act := superloop.Action{Kind: superloop.KindNext, Key: "next board-progress " + c.Ref, Ref: c.Ref, URL: c.URL}
		what, err := l.StartItem(ctx, act, loop.LoopBoardItem{Ref: c.Ref, URL: c.URL, Epic: c.Epic, WIPExempt: c.WIPExempt})
		if err != nil {
			return false, "start stalled item", err
		}
		return true, "started stalled item (pace exception for this item only)", l.Record(act, what)
	}
	return false, "no known automatic repair", nil
}

// progressOwner is who board next names as the owner of c (boardOwners,
// titleOwners): a live session's serve record, a busy or parked agent, a task
// naming the item anywhere, or a running session that works on it. The
// progress signal follows only the workers progressWorker finds, so one of
// these can be busy on an item whose signal stands still.
func (a *app) progressOwner(st *state.State, sessions []*claude.Session, c board.Candidate) string {
	alive := func(p state.Party) bool {
		return slices.ContainsFunc(sessions, func(s *claude.Session) bool { return s.Party().Is(p) })
	}
	sc := pickScope{me: loop.LoopParty, alive: alive, listed: a.now, owner: a.cfg.Board.Owner,
		gone:  func(ag state.Agent) bool { return agyQuotaError(a.cfg, ag) != nil },
		works: servingWork(st, sessions, a.sessionWorks(sessions), a.now.Add(-a.cfg.Overlaps.ActiveWithin.Duration))}
	owners := coverTitled(boardOwners(st, sc), titleOwners(st, sc), []board.Candidate{c})
	return owners[strings.ToLower(c.Ref)]
}

func (a *app) validateProgressItem() error {
	if a.progressItem == "" {
		return nil
	}
	var saved loop.BoardProgress
	if _, err := a.store.ReadFile(loop.BoardProgressFile, &saved); err != nil {
		return err
	}
	if a.as != loop.LoopParty.Name || !saved.Entries[a.progressItem].Stalled(a.now) {
		return refused("progress pace exception requires a freshly observed stalled top board item")
	}
	return nil
}

func (a *app) validateProgressTask(brief string) error {
	// namedRefs lower-cases; a board ref keeps the case GitHub gives the repository.
	if a.progressItem != "" && !slices.ContainsFunc(namedRefs(brief, ""), func(r string) bool { return strings.EqualFold(r, a.progressItem) }) {
		return refused("progress pace exception applies only to the named item's task")
	}
	return nil
}

func (a *app) withProgressPace(readings map[string]capacity.ProviderReadingResult) {
	if a.progressItem == "" || a.validateProgressItem() != nil {
		return
	}
	for name, r := range readings {
		r.PaceOff = &capacity.PaceOffView{At: a.now, Until: a.now.Add(2 * time.Minute), By: loop.LoopParty.Name, Reason: "30-minute stalled item " + issueURL(a.progressItem)}
		readings[name] = r
	}
}

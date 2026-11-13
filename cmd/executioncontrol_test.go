package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const sshTestCoordinator = "coordinator"

const sshTestExecutable = "/usr/local/bin/beekeeper"
const sshTestConfig = "/etc/beekeeper/config.yaml"
const sshTestSelector = "host-a"
const sshTestRepository = "giantswarm/beekeeper"
const sshTestTask = "implement the fixture task"
const completedFixtureTask = "Completed the fixture task"

func executionTestApp(t *testing.T) (*app, execution.StartRequest) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	key, err := execution.NewControlKey()
	if err != nil {
		t.Fatal(err)
	}
	r := execution.StartRequest{Ref: execution.Ref{Host: sshTestSelector, Execution: execution.ExecutionID(uuid.NewString()), Generation: 1}, Coordinator: sshTestCoordinator, RequestID: uuid.NewString(), Name: "remote-worker", Task: sshTestTask, Repository: sshTestRepository, GitRef: mainBranch, Harness: execution.HarnessClaude, Model: "opus", Prompt: "complete the fixture task", PolicyDigest: execution.Digest([]byte("fixture policy")), ConversationID: uuid.NewString(), ControlKey: key}
	h := execution.Host{Endpoint: execution.Endpoint{SSH: "host-a.example", Executable: sshTestExecutable, Config: sshTestConfig}, Identity: r.Ref.Host, WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{r.Repository: filepath.Join(root, "source")}, Harnesses: []string{execution.HarnessClaude}, PolicyDigest: r.PolicyDigest, MaxBusy: 2}
	a := &app{store: store, out: io.Discard, now: time.Now().UTC(), cfg: &config.Config{StateDir: root, Identity: config.Identity{Host: r.Coordinator}, Agents: config.Agents{Hosts: map[string]config.ExecutionHost{sshTestSelector: h}}, Guide: config.Guide{Person: litReviewer}}}
	party := state.Party{Session: r.ConversationID, Name: r.Name, Host: string(r.Ref.Host)}
	err = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: party, Task: r.Task, Execution: &r.Ref}}
		st.Starts = []state.Start{{Party: party, Execution: &state.ExecutionRecord{Handle: execution.Handle{Ref: r.Ref, ConversationID: r.ConversationID, Repository: r.Repository, Unit: execution.UnitName(r.Ref)}, Request: r, RequestDigest: r.Digest(), Selector: sshTestSelector, Phase: execution.Activating, Observation: execution.Observation{ProcessState: execution.Running, Reachability: execution.Reachable, Ready: true}}}}
		st.Records = []state.Record{{Session: party, Issue: "https://github.com/giantswarm/beekeeper/issues/1711"}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, r
}

func coordinatorTestCall(t *testing.T, a *app, r execution.StartRequest, operation string, payload, result any, drop bool) error {
	t.Helper()
	endpoint := execution.Endpoint{SSH: "coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig}
	return execution.CoordinatorCall(context.Background(), endpoint, string(r.Ref.Host), operation, r.Ref, payload, r.ControlKey, result, func(ctx context.Context, _ []string, raw []byte) ([]byte, error) {
		var out bytes.Buffer
		if err := a.serveExecutionCoordinator(ctx, string(r.Ref.Host), bytes.NewReader(raw), &out); err != nil {
			return nil, err
		}
		if drop {
			return nil, errors.New("fixture reply lost after apply")
		}
		return out.Bytes(), nil
	})
}

func TestExecutionCoordinatorReceiptsSurviveLostReplies(t *testing.T) {
	a, r := executionTestApp(t)
	n := executionNote{Draft: noteDraft{Question: "Approve the fixture change?", StatusQuo: "The change needs a decision.", Why: "The worker needs an owner decision.", Default: "Keep the change unpublished", Due: "1h", Kind: noteDecision}, Note: state.Note{Kind: noteDecision, For: supervisorName, Due: a.now.Add(time.Hour), Default: "Keep the change unpublished", Question: "Approve the fixture change?", StatusQuo: "The change needs a decision.", Refs: []string{"https://github.com/giantswarm/beekeeper/issues/1711"}, By: state.Party{Name: "forged-sender"}, Posted: "forged-gateway-handle"}}
	request := execution.ControlRequest{Ref: r.Ref, ID: "note-one", Sequence: 1, Operation: execution.ControlNote, Data: mustExecutionJSON(t, n)}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", request, &result, true); err == nil {
		t.Fatal("lost reply reported success")
	}
	// Reopen the file-backed coordinator: no process-local deduplication.
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := *a
	restarted.store = store
	if err := coordinatorTestCall(t, &restarted, r, "control", request, &result, false); err != nil {
		t.Fatal(err)
	}
	if result.Output != "note #1 for supervisor" || result.Error != "" {
		t.Fatalf("original receipt missing: %+v", result)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].By.Session != r.ConversationID || st.Notes[0].Posted != "" {
		t.Fatal("duplicate note, forged sender or lifecycle metadata persisted")
	}
	request.ID = "park-one"
	request.Sequence++
	request.Operation = execution.ControlPark
	request.Data = mustExecutionJSON(t, executionPark{On: "#1", Waits: "the supervisor decision"})
	if err := coordinatorTestCall(t, &restarted, r, "control", request, &result, false); err != nil || result.Error != "" {
		t.Fatal("park was not acknowledged", err, result.Error)
	}
	st, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[0].Task != sshTestTask || st.Agents[0].Park == nil || st.Records[0].Waits == "" {
		t.Fatal("park did not preserve the coordinator task and wait")
	}
	request.ID = "report-one"
	request.Sequence++
	request.Operation = execution.ControlReport
	request.Data = mustExecutionJSON(t, executionReport{Finished: true, Text: completedFixtureTask, Problems: []string{ghNone}})
	if err := coordinatorTestCall(t, &restarted, r, "control", request, &result, true); err == nil {
		t.Fatal("lost report reply reported success")
	}
	if err := coordinatorTestCall(t, &restarted, r, "control", request, &result, false); err != nil || result.Error != "" {
		t.Fatal("report retry failed", err, result.Error)
	}
	st, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.WorkerReports) != 1 || !st.Agents[0].Done || st.Agents[0].Task != "" || st.Notes[0].TaskEnded.IsZero() {
		t.Fatal("report was lost or applied twice")
	}
}

func mustExecutionJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExecutionCoordinatorRefusesWrongIdentityAndLiveStop(t *testing.T) {
	a, r := executionTestApp(t)
	var decision execution.ActivationDecision
	if err := coordinatorTestCall(t, a, r, "activate", r, &decision, false); err != nil || !decision.Allowed {
		t.Fatal("registered activation refused", err)
	}
	wrong := r
	wrong.Ref.Generation++
	if err := coordinatorTestCall(t, a, wrong, "activate", wrong, &decision, false); err == nil {
		t.Fatal("wrong generation authorized")
	}
	wrong = r
	wrong.ControlKey, _ = execution.NewControlKey()
	if err := coordinatorTestCall(t, a, wrong, "activate", wrong, &decision, false); err == nil {
		t.Fatal("wrong callback credential authorized")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Stop = &state.Stop{Reason: "fixture stop"}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, r, "activate", r, &decision, false); err == nil {
		t.Fatal("live stop did not revoke activation")
	}
	var authority executionAuthority
	if err := coordinatorTestCall(t, a, r, "guard", nil, &authority, false); err != nil || !authority.Stopped {
		t.Fatal("live guard ignored coordinator stop", err)
	}
	var status json.RawMessage
	if err := coordinatorTestCall(t, a, r, executionStatus, nil, &status, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(status), r.ControlKey) || strings.Contains(string(status), "controlKey") {
		t.Fatal("status exposed a component credential")
	}
}

// deskReviewWait is the wait a worker parks with for its desk review.
func TestExecutionParkKeepsReviewEvidence(t *testing.T) {
	a, r := executionTestApp(t)
	const pushed = "2222222222222222222222222222222222222222"
	pr := sshTestRepository + "#7"
	branch, repo, prHead := executionBranch(r.Ref), sshTestRepository, loopHead
	draft := false
	was := executionReviewGH
	t.Cleanup(func() { executionReviewGH = was })
	reads := 0
	executionReviewGH = func(_ context.Context, args ...string) ([]byte, error) {
		reads++
		if strings.Join(args, " ") != "api repos/"+sshTestRepository+"/pulls/7" {
			t.Fatalf("read %v", args)
		}
		return fmt.Appendf(nil, `{"draft":%t,"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}}}`, draft, branch, prHead, repo), nil
	}
	park := func(p executionPark) string {
		t.Helper()
		var line string
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			var evs []state.Event
			var err error
			line, evs, err = a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{Operation: execution.ControlPark, Data: mustExecutionJSON(t, p)})
			if len(evs) != 1 || evs[0].Verb != parkEvent {
				t.Fatalf("park events %+v", evs)
			}
			return evs, err
		}); err != nil {
			t.Fatal(err)
		}
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		got := st.Agents[0].Park.ReviewHead
		if !strings.Contains(line, parkReviewMode(got)) {
			t.Fatalf("park %+v: %s", st.Agents[0].Park, line)
		}
		return got
	}
	n := reads
	if got := park(executionPark{On: pr, Waits: "image rebuild completion timer 3155", Head: loopHead}); got != "" || reads != n {
		t.Fatalf("plain park recorded %q after %d reads", got, reads-n)
	}
	draft = true
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: loopHead}); got != "" {
		t.Fatalf("draft park recorded %q", got)
	}
	draft = false
	// The workspace's HEAD is the published head of the execution's own
	// branch: a review request, whatever the host's branch is called.
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: loopHead}); got != loopHead {
		t.Fatalf("remote park at the published head recorded %q", got)
	}
	// An older host proves nothing itself: the owned branch's head stands.
	if got := park(executionPark{On: pr, Waits: deskReviewWait}); got != loopHead {
		t.Fatalf("older host's park recorded %q", got)
	}
	// Unpublished commits on the workspace: a plain wait.
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: pushed}); got != "" {
		t.Fatalf("park ahead of the published head recorded %q", got)
	}
	// A new push, then a re-park at it: the new head's review request.
	prHead = pushed
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: pushed}); got != pushed {
		t.Fatalf("re-park after a push recorded %q", got)
	}
	// A host's own claim is no proof: another branch's pull request, a fork's
	// or one in another repository is a plain wait.
	branch = "feature"
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: pushed, ReviewHead: pushed}); got != "" {
		t.Fatalf("another branch's pull request recorded %q", got)
	}
	branch, repo = executionBranch(r.Ref), "fork/beekeeper"
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: pushed}); got != "" {
		t.Fatalf("a fork's pull request recorded %q", got)
	}
	repo = sshTestRepository
	n = reads
	if got := park(executionPark{On: "o/other#7", Waits: deskReviewWait, Head: pushed}); got != "" || reads != n {
		t.Fatalf("another repository's pull request recorded %q after %d reads", got, reads-n)
	}
	if got := park(executionPark{Waits: "no target"}); got != "" || reads != n {
		t.Fatalf("a park without a target recorded %q", got)
	}
	executionReviewGH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("unreachable") }
	if got := park(executionPark{On: pr, Waits: deskReviewWait, Head: pushed}); got != "" {
		t.Fatalf("an unread pull request recorded %q", got)
	}
}

// A managed SSH worker leaves its own park on the coordinator: resume ends
// it, wakes nobody and says what settled the wait; a second is refused.
func TestExecutionRefusesUntargetedOverlapPark(t *testing.T) {
	a, _ := executionTestApp(t)
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs, err := a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{
			Operation: execution.ControlPark,
			Data:      mustExecutionJSON(t, executionPark{Waits: "supervisor overlap coordination"}),
		})
		return evs, err
	})
	if err == nil || !strings.Contains(err.Error(), "--on <owner/repo#n>") {
		t.Fatalf("untargeted overlap: %v", err)
	}
}

func TestExecutionResumeEndsTheWorkersOwnPark(t *testing.T) {
	a, _ := executionTestApp(t)
	apply := func(op string, data any) (string, error) {
		t.Helper()
		var line string
		err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			var evs []state.Event
			var err error
			line, evs, err = a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{Operation: op, Data: mustExecutionJSON(t, data)})
			return evs, err
		})
		return line, err
	}
	if _, err := apply(execution.ControlResume, struct{}{}); err == nil || !strings.Contains(err.Error(), "is not parked") {
		t.Fatalf("resume unparked: %v", err)
	}
	if _, err := apply(execution.ControlPark, executionPark{Waits: "the supervisor's word"}); err != nil {
		t.Fatal(err)
	}
	line, err := apply(execution.ControlResume, struct{}{})
	if err != nil || !strings.Contains(line, "remote-worker: your park ended, nobody is woken. Your park on the supervisor's word is over") {
		t.Fatalf("resume: %q %v", line, err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[0].Park != nil {
		t.Fatalf("park after resume %+v", st.Agents[0].Park)
	}
	if _, err := apply(execution.ControlResume, struct{}{}); err == nil || !strings.Contains(err.Error(), "is not parked") {
		t.Fatalf("second resume: %v", err)
	}
}

// TestExecutionMoveHandsOnItsPublication: a host worker that published its
// work and then asks for the coordinator (agents move) hands the fresh
// worker there the branch, head and pull request it published, and that
// pull request stays on the coordinator beside the issue it serves, also
// when no park or desk review named it
// (https://github.com/giantswarm/beekeeper/issues/3900).
//
// control: none: agents move and its execution control are new, so the
// merge base cannot build this test; the scenario
// regression/host-worker-moves-its-task-to-the-coordinator fails on the base
func TestExecutionMoveHandsOnItsPublication(t *testing.T) {
	a, r := executionTestApp(t)
	pull := "https://github.com/" + sshTestRepository + "/pull/7"
	branch := executionBranch(r.Ref)
	var line string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Published = &state.ExecutionPublication{At: a.now, Branch: branch, Head: loopHead, URL: pull}
		var evs []state.Event
		var err error
		line, evs, err = a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{Operation: execution.ControlMove, Data: mustExecutionJSON(t, executionMove{Why: "the proof needs the coordinator's data"})})
		return evs, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "on the coordinator") {
		t.Fatalf("move reply %q", line)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	p := st.Agents[0].Park
	if p == nil || len(p.Continuation) != 1 {
		t.Fatalf("park %+v, want one queued continuation", p)
	}
	for _, want := range []string{branch, loopHead, pull} {
		if !strings.Contains(p.Continuation[0], want) {
			t.Fatalf("continuation %q does not name %s", p.Continuation[0], want)
		}
	}
	for _, ref := range []string{sshTestRepository + "#1711", sshTestRepository + "#7"} {
		if _, ok := st.LocalTaskFor(ref); !ok {
			t.Fatalf("%s carries no mark to stay on the coordinator: %+v", ref, st.LocalTasks)
		}
	}
	if rec, _ := executionRecord(st, r.Ref); rec == nil || !rec.Completed {
		t.Fatalf("execution record %+v, want it completed", rec)
	}
}

// TestExecutionParkReasonMaySayAnything: an SSH execution's park on a pull
// request is recorded on it whatever its reason says; --on alone says what
// a park waits on.
func TestExecutionParkReasonMaySayAnything(t *testing.T) {
	for _, waits := range []string{"the editor's held-still source", "merge hold lifting", "gate run completion"} {
		t.Run(waits, func(t *testing.T) {
			a, _ := executionTestApp(t)
			err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				_, evs, err := a.applyExecutionControl(context.Background(), st, st.Agents[0].Party, execution.ControlRequest{
					Operation: execution.ControlPark,
					Data:      mustExecutionJSON(t, executionPark{On: refOne, Waits: waits}),
				})
				return evs, err
			})
			if err != nil {
				t.Fatalf("park refused: %v", err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if p := st.Agents[0].Park; p == nil || p.On != refOne || p.Waits != waits {
				t.Fatalf("park %+v, want on %s waiting for %q", p, refOne, waits)
			}
		})
	}
}

func TestRemoteCompletionRequiresAndRecordsDeliveryEvidence(t *testing.T) {
	a, r := executionTestApp(t)
	stubNoChangeGitHub(t, map[github.PR]github.RefState{{Repo: sshTestRepository, N: 1711}: {Kind: github.RefIssue, State: github.Open}}, nil)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.StartDelivery("giantswarm/beekeeper#1711", r.Name, "", a.now)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	request := execution.ControlRequest{Ref: r.Ref, ID: "outcome-refused", Sequence: 1, Operation: execution.ControlReport,
		Data: mustExecutionJSON(t, executionReport{Finished: true, Text: completedFixtureTask, Problems: []string{ghNone}})}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, taskEndings) {
		t.Fatalf("unexplained completion accepted: %+v", result)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[0].Done || st.Agents[0].Task == "" {
		t.Fatal("refused report released task")
	}
	request.ID, request.Sequence = "outcome-proven", 2
	request.Data = mustExecutionJSON(t, executionReport{Finished: true, NoChange: "operation verified in the issue response", Text: completedFixtureTask, Problems: []string{ghNone}})
	// Successful receipts omit Error; decode into a fresh result so the
	// previous refusal cannot survive JSON decoding of the corrected report.
	result = execution.ControlResult{}
	if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil || result.Error != "" {
		t.Fatalf("no-change: %v %+v", err, result)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Agents[0].Done || st.Deliveries[0].Outcome != state.OutcomeNoChange || st.Deliveries[0].Evidence == "" {
		t.Fatalf("missing remote outcome: %+v", st.Deliveries)
	}
}

// stubNoChangeGitHub answers the coordinator's reads of issues and pull
// requests from states and records its gh writes, which fail with writeErr.
func stubNoChangeGitHub(t *testing.T, states map[github.PR]github.RefState, writeErr error) *[][]string {
	t.Helper()
	prevStates, prevGH := refStates, executionPublishGH
	t.Cleanup(func() { refStates, executionPublishGH = prevStates, prevGH })
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		out := map[github.PR]github.RefState{}
		for _, r := range refs {
			if s, ok := states[r]; ok {
				out[r] = s
			}
		}
		return out, nil
	}
	var writes [][]string
	executionPublishGH = func(_ context.Context, args ...string) ([]byte, error) {
		writes = append(writes, args)
		return nil, writeErr
	}
	return &writes
}

// TestRemoteNoChangeProofNeedsFullURLs: the no-change proof becomes the
// comment that closes the issue, so a bare reference in it refuses the
// report, as one in the report's text does, before anything is closed.
func TestRemoteNoChangeProofNeedsFullURLs(t *testing.T) {
	a, r := executionTestApp(t)
	writes := stubNoChangeGitHub(t, map[github.PR]github.RefState{{Repo: sshTestRepository, N: 1711}: {Kind: github.RefIssue, State: github.Open}}, nil)
	request := execution.ControlRequest{Ref: r.Ref, ID: "bare-proof", Sequence: 1, Operation: execution.ControlReport,
		Data: mustExecutionJSON(t, executionReport{Finished: true, NoChange: "fixed by #1712, merged", Text: completedFixtureTask, Problems: []string{ghNone}})}
	var result execution.ControlResult
	if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "#1712 without its full URL") || len(*writes) > 0 {
		t.Fatalf("bare reference in the proof: %+v, writes %q", result, *writes)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[0].Done || len(st.Records) != 1 {
		t.Fatalf("refused report ended the task: done %v, records %+v", st.Agents[0].Done, st.Records)
	}
}

// TestRemoteNoChangeClosesTheServedIssue: an execution host's worker that
// ends its task with --done --no-change has the coordinator close the open
// issue it served, with its proof as the comment, since the worker cannot
// and refill offers an open issue again. A failed close keeps the outcome
// and tells the guide; a closed issue and a pull request are left alone.
func TestRemoteNoChangeClosesTheServedIssue(t *testing.T) {
	const proof = "fixed on main by https://github.com/giantswarm/beekeeper/pull/1712, merged"
	const url = "https://github.com/giantswarm/beekeeper/issues/1711"
	issue := github.PR{Repo: sshTestRepository, N: 1711}
	open := github.RefState{Kind: github.RefIssue, State: github.Open}
	for _, tc := range []struct {
		name     string
		state    github.RefState
		writeErr error
		closes   bool
		problem  string
	}{
		{name: "open issue", state: open, closes: true},
		{name: "close fails", state: open, writeErr: errors.New("HTTP 403: Resource not accessible"), closes: true, problem: url + " stays open: remote-worker ended its task with no change, and closing the issue failed: HTTP 403"},
		{name: "closed issue", state: github.RefState{Kind: github.RefIssue, State: github.Closed}},
		{name: "pull request", state: github.RefState{Kind: github.RefPullRequest, State: github.Open}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, r := executionTestApp(t)
			writes := stubNoChangeGitHub(t, map[github.PR]github.RefState{issue: tc.state}, tc.writeErr)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.StartDelivery("giantswarm/beekeeper#1711", r.Name, "", a.now)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			request := execution.ControlRequest{Ref: r.Ref, ID: "no-change", Sequence: 1, Operation: execution.ControlReport,
				Data: mustExecutionJSON(t, executionReport{Finished: true, NoChange: proof, Text: completedFixtureTask, Problems: []string{ghNone}})}
			var result execution.ControlResult
			if err := coordinatorTestCall(t, a, r, "control", request, &result, false); err != nil || result.Error != "" {
				t.Fatalf("no-change report: %v %+v", err, result)
			}
			if !tc.closes {
				if len(*writes) > 0 {
					t.Fatalf("wrote to GitHub for a %s %s: %q", tc.state.State, tc.state.Kind, *writes)
				}
				return
			}
			if len(*writes) != 1 {
				t.Fatalf("closes: got %q, want one", *writes)
			}
			w := (*writes)[0]
			want := fmt.Sprintf("issue close %s --repo %s --comment", url, sshTestRepository)
			if len(w) != 7 || strings.Join(w[:6], " ") != want || !strings.HasPrefix(w[6], "No change needed: "+proof) {
				t.Fatalf("close: got %q, want %q and the proof as the comment", w, want)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if !st.Agents[0].Done || st.Deliveries[0].Outcome != state.OutcomeNoChange || len(st.Records) != 0 {
				t.Fatalf("outcome: done %v, deliveries %+v, records %+v", st.Agents[0].Done, st.Deliveries, st.Records)
			}
			reports := st.WorkerReports
			if len(reports) != 1 {
				t.Fatalf("reports: %+v", reports)
			}
			closed, err := a.store.Events(0, func(ev state.Event) bool { return ev.Verb == "agents.no-change-closed" })
			if err != nil {
				t.Fatal(err)
			}
			if tc.problem == "" {
				if len(reports[0].Problems) != 0 || len(closed) != 1 || !strings.HasPrefix(closed[0].Detail, url+": closed for remote-worker") {
					t.Fatalf("problems %q, closed events %+v", reports[0].Problems, closed)
				}
				return
			}
			if len(reports[0].Problems) != 1 || !strings.HasPrefix(reports[0].Problems[0], tc.problem) || len(closed) != 0 {
				t.Fatalf("problems %q, want %q; closed events %+v", reports[0].Problems, tc.problem, closed)
			}
		})
	}
}

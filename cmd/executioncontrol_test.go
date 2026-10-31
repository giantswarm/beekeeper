package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const sshTestCoordinator = "coordinator"

const sshTestExecutable = "/usr/local/bin/beekeeper"
const sshTestConfig = "/etc/beekeeper/config.yaml"
const sshTestSelector = "host-a"
const sshTestRepository = "giantswarm/beekeeper"
const sshTestTask = "implement the fixture task"

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
	a := &app{store: store, out: io.Discard, now: time.Now().UTC(), cfg: &config.Config{StateDir: root, Agents: config.Agents{Hosts: map[string]config.ExecutionHost{sshTestSelector: h}}, Guide: config.Guide{Person: "reviewer"}}}
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
	request.Data = mustExecutionJSON(t, executionReport{Finished: true, Text: "Completed the fixture task", Problems: []string{ghNone}})
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

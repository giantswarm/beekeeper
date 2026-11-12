package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/transcripts"
)

const (
	transcriptTestGrantCommand = "grant"
	transcriptTestGuideSession = "guide-sess-1"
	transcriptTestRPCWorker    = "w-rpc-1"

	transcriptTestWorkerBox    = "worker-box"
	transcriptTestWorker       = "w-bk-1"
	transcriptTestDevctlWorker = "w-devctl-1"
	transcriptTestRemoteWorker = "w-bk-remote"
	transcriptTestIssue        = "https://github.com/giantswarm/beekeeper/issues/2183"
	transcriptTestRPCSession   = "s-rpc-1"
	transcriptTestCodex        = "codex"
)

func newTranscriptTestApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	was := plat.Machine
	plat.Machine = tableMachine{was}
	t.Cleanup(func() { plat.Machine = was })
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := &app{
		cfg: &config.Config{
			StateDir: dir,
			Identity: config.Identity{Person: "alex", Team: "platform", Host: "coord-1"},
			Claude: config.Claude{
				ProjectsDir: filepath.Join(dir, "claude-projects"),
				SessionsDir: filepath.Join(dir, "claude-sessions"),
			},
			Omp: config.Omp{
				SessionsDir: filepath.Join(dir, "omp-sessions"),
			},
			Agy: config.Agy{
				BrainDir:         filepath.Join(dir, "agy-brain"),
				ConversationsDir: filepath.Join(dir, "agy-conversations"),
			},
			Codex: config.Codex{
				SessionsDir: filepath.Join(dir, "codex-sessions"),
			},
		},
		store: store,
		out:   &out,
		now:   now,
	}
	return a, &out
}

func TestTranscriptsMultiHarnessAccessGrantAndAskUserQuestion(t *testing.T) {
	a, out := newTranscriptTestApp(t)
	ctx := context.Background()

	// Write a secret value to the scan index to verify secret masking.
	const secretVal = "super-secret-token-value-987654321"
	ix, err := guard.OpenIndex(a.scanDir())
	if err != nil {
		t.Fatal(err)
	}
	ix.Add("op://vault/item/API_KEY", secretVal)
	if err := ix.Save(a.now); err != nil {
		t.Fatal(err)
	}

	// 1. Create a Claude guide session with an AskUserQuestion dialog turn and a secret.
	projDir := filepath.Join(a.cfg.Claude.ProjectsDir, "-workspace-beekeeper")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	guideTranscript := filepath.Join(projDir, "guide-sess-1.jsonl")
	guideLines := strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-03T09:10:00Z","message":{"content":"Please coordinate the release"}}`,
		`{"type":"assistant","timestamp":"2026-11-03T09:11:00Z","message":{"content":[{"type":"tool_use","id":"tu-ask-1","name":"AskUserQuestion","input":{"questions":[{"question":"Which retention window should transcripts use?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-11-03T09:12:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"tu-ask-1","content":"User has answered your questions: \"Which retention window should transcripts use?\"=\"Keep 30 days and 2 GiB\""}]},"toolUseResult":{"answers":{"Which retention window should transcripts use?":"Keep 30 days and 2 GiB"}}}`,
		`{"type":"assistant","timestamp":"2026-11-03T09:13:00Z","message":{"content":[{"type":"text","text":"Configured retention with token ` + secretVal + `"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(guideTranscript, []byte(guideLines), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Create a same-repo worker Claude transcript (giantswarm/beekeeper#2183).
	workerBKTranscript := filepath.Join(projDir, "worker-bk-1.jsonl")
	workerBKLines := strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-03T09:20:00Z","message":{"content":"Work https://github.com/giantswarm/beekeeper/issues/2183"}}`,
		`{"type":"assistant","timestamp":"2026-11-03T09:21:00Z","message":{"content":[{"type":"text","text":"Implementing transcript store for beekeeper."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(workerBKTranscript, []byte(workerBKLines), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Create another repository's worker transcript (giantswarm/devctl#400).
	workerDevctlTranscript := filepath.Join(projDir, "worker-devctl-1.jsonl")
	workerDevctlLines := strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-03T09:30:00Z","message":{"content":"Work https://github.com/giantswarm/devctl/issues/400"}}`,
		`{"type":"assistant","timestamp":"2026-11-03T09:31:00Z","message":{"content":[{"type":"text","text":"Fixed release promote flag in devctl."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(workerDevctlTranscript, []byte(workerDevctlLines), 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Populate state with the guide, local workers, and a remote SSH execution on agy.
	execRef := execution.Ref{Host: transcriptTestWorkerBox, Execution: "exec-agy-2183"}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		guideParty := state.Party{Session: transcriptTestGuideSession, Name: guideName}
		st.Guide = &state.Role{
			Holder: &state.Supervisor{Party: guideParty, Since: a.now.Add(-time.Hour)},
		}
		bkParty := state.Party{Session: "worker-bk-1", Name: transcriptTestWorker}
		devctlParty := state.Party{Session: "worker-devctl-1", Name: transcriptTestDevctlWorker}
		remoteParty := state.Party{Session: "agy-conv-1", Name: transcriptTestRemoteWorker}
		st.Agents = []state.Agent{
			{Party: bkParty, Task: transcriptTestIssue},
			{Party: devctlParty, Task: "https://github.com/giantswarm/devctl/issues/400"},
			{Party: remoteParty, Task: transcriptTestIssue, Execution: &execRef},
		}
		st.Starts = []state.Start{
			{Party: bkParty, At: a.now.Add(-40 * time.Minute)},
			{Party: devctlParty, At: a.now.Add(-30 * time.Minute)},
			{
				Party: remoteParty,
				At:    a.now.Add(-20 * time.Minute),
				Execution: &state.ExecutionRecord{
					Selector:   "remote-host",
					ReservedAt: a.now.Add(-20 * time.Minute),
					LastActive: a.now.Add(-5 * time.Minute),
					Phase:      execution.Applied,
					Completed:  true,
					Request: execution.StartRequest{
						Name:       transcriptTestRemoteWorker,
						Harness:    "agy",
						Repository: sshTestRepository,
						Prompt:     "Verify Agy remote transcript on https://github.com/giantswarm/beekeeper/issues/2183",
						ControlKey: "ctrl-secret-key",
					},
					Handle: execution.Handle{
						Ref:            execRef,
						ConversationID: "agy-conv-1",
						Workspace:      "/home/worker/workspace",
					},
				},
			},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Also append remote execution stream log lines for w-bk-remote.
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	_ = a.syncAllTranscripts(ctx, st, false) // queries read the stored transcripts; transcripts sync reports a refresh failure
	remoteMeta, prompts := a.executionTranscriptMeta(st, st.Starts[2], st.Starts[2].Execution)
	remoteLog := `{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-11-03T09:45:00Z","content":"Remote Agy worker finished verification."}` + "\n"
	if _, err := a.transcriptStore().AppendRemoteLog(remoteMeta, []byte(remoteLog), "120", prompts); err != nil {
		t.Fatal(err)
	}

	// Guide sees all 4 transcripts.
	a.as = guideName
	metas, err := a.listTranscriptsLocal(ctx, remoteTranscriptListReq{})
	if err != nil {
		t.Fatalf("guide list: %v", err)
	}
	if len(metas) != 4 {
		t.Fatalf("guide saw %d transcripts, want 4: %+v", len(metas), metas)
	}

	// Guide can search the AskUserQuestion dialog answer.
	hits, err := a.searchTranscriptsLocal(ctx, remoteTranscriptSearchReq{Query: "Keep 30 days and 2 GiB"})
	if err != nil {
		t.Fatalf("guide search AskUserQuestion: %v", err)
	}
	if len(hits) != 1 || hits[0].Turn.Role != transcripts.RoleUser || !strings.Contains(hits[0].Snippet, "Keep 30 days and 2 GiB") {
		t.Fatalf("unexpected AskUserQuestion search hits: %+v", hits)
	}

	// Secret value is redacted in stored transcripts and never returned by search or read.
	secretHits, err := a.searchTranscriptsLocal(ctx, remoteTranscriptSearchReq{Query: secretVal})
	if err != nil {
		t.Fatal(err)
	}
	if len(secretHits) != 0 {
		t.Fatalf("secret value was not redacted from search index: %+v", secretHits)
	}
	guideRead, err := a.readTranscriptLocal(ctx, remoteTranscriptReadReq{Target: transcriptTestGuideSession})
	if err != nil {
		t.Fatalf("guide read: %v", err)
	}
	for _, turn := range guideRead.Turns {
		if strings.Contains(turn.Text, secretVal) {
			t.Fatalf("unredacted secret in turn: %q", turn.Text)
		}
	}

	// Worker on giantswarm/beekeeper (w-bk-1) sees its own transcript and w-bk-remote's transcript,
	// but NOT the guide's transcript and NOT giantswarm/devctl's transcript.
	a.as = transcriptTestWorker
	workerMetas, err := a.listTranscriptsLocal(ctx, remoteTranscriptListReq{})
	if err != nil {
		t.Fatalf("worker list: %v", err)
	}
	if len(workerMetas) != 2 {
		t.Fatalf("worker saw %d transcripts, want 2 (same repo only): %+v", len(workerMetas), workerMetas)
	}
	for _, m := range workerMetas {
		if m.Repo != sshTestRepository || m.Role == guideRole.name {
			t.Fatalf("worker saw unauthorized transcript: %+v", m)
		}
	}

	// Worker can read the remote Agy worker's transcript on the same repository even after it ended.
	remoteRead, err := a.readTranscriptLocal(ctx, remoteTranscriptReadReq{Target: transcriptTestRemoteWorker})
	if err != nil {
		t.Fatalf("worker read same-repo remote transcript: %v", err)
	}
	if remoteRead.Meta.Running || remoteRead.Meta.Harness != "agy" || len(remoteRead.Turns) < 2 {
		t.Fatalf("unexpected remote transcript read: %+v", remoteRead)
	}

	// Worker is refused when reading the guide's transcript or another repository's transcript.
	if _, err := a.readTranscriptLocal(ctx, remoteTranscriptReadReq{Target: transcriptTestGuideSession}); Code(err) != ExitRefused {
		t.Fatalf("worker reading guide transcript: got %v, want ExitRefused", err)
	}
	if _, err := a.readTranscriptLocal(ctx, remoteTranscriptReadReq{Target: transcriptTestDevctlWorker}); Code(err) != ExitRefused {
		t.Fatalf("worker reading devctl transcript: got %v, want ExitRefused", err)
	}
	if _, err := a.searchTranscriptsLocal(ctx, remoteTranscriptSearchReq{Repo: devctlRepo, Query: "promote"}); Code(err) != ExitRefused {
		t.Fatalf("worker searching devctl repo: got %v, want ExitRefused", err)
	}

	// Worker cannot grant access to itself.
	cmd := a.transcriptsCmd()
	cmd.SetArgs([]string{transcriptTestGrantCommand, transcriptTestWorker, loop.LoopRepoFlag, devctlRepo})
	if err := cmd.Execute(); Code(err) != ExitRefused {
		t.Fatalf("worker self-grant: got %v, want ExitRefused", err)
	}

	// Worker requests access to giantswarm/devctl.
	out.Reset()
	cmd = a.transcriptsCmd()
	cmd.SetArgs([]string{"request", loop.LoopRepoFlag, devctlRepo, "--reason", "check release promote flag"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("worker request: %v", err)
	}
	if !strings.Contains(out.String(), "transcript access request #1 recorded") {
		t.Fatalf("unexpected request output: %q", out.String())
	}

	// Guide fulfills the open request with `beekeeper transcripts grant w-bk-1`.
	a.as = guideName
	out.Reset()
	cmd = a.transcriptsCmd()
	cmd.SetArgs([]string{transcriptTestGrantCommand, transcriptTestWorker, "--ttl", "1h"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("guide grant: %v", err)
	}
	if !strings.Contains(out.String(), `granted transcript access #2 to "w-bk-1" (repo giantswarm/devctl)`) {
		t.Fatalf("unexpected grant output: %q", out.String())
	}

	// Worker can now read and search giantswarm/devctl transcripts!
	a.as = transcriptTestWorker
	devctlRead, err := a.readTranscriptLocal(ctx, remoteTranscriptReadReq{Target: "https://github.com/giantswarm/devctl/issues/400"})
	if err != nil {
		t.Fatalf("worker read devctl after grant: %v", err)
	}
	if len(devctlRead.Turns) != 2 || !strings.Contains(devctlRead.Turns[1].Text, "release promote flag") {
		t.Fatalf("unexpected devctl turns: %+v", devctlRead.Turns)
	}

	// Once the grant expires, worker is refused again.
	a.now = a.now.Add(2 * time.Hour)
	if _, err := a.readTranscriptLocal(ctx, remoteTranscriptReadReq{Target: transcriptTestDevctlWorker}); Code(err) != ExitRefused {
		t.Fatalf("worker read devctl after grant expiry: got %v, want ExitRefused", err)
	}

	// TranscriptAccess includes <stateDir>/transcripts so the raw file guard protects the store.
	access := a.transcriptPolicy()
	wantRoot := filepath.Join(a.cfg.StateDir, "transcripts")
	if !slices.Contains(access.Roots, wantRoot) {
		t.Fatalf("TranscriptAccess.Roots %v lacks %q", access.Roots, wantRoot)
	}
}

func TestExecutionTranscriptRPC(t *testing.T) {
	a, _ := newTranscriptTestApp(t)
	ctx := context.Background()

	execRef := execution.Ref{
		Host:       transcriptTestWorkerBox,
		Execution:  "11111111-2222-3333-4444-555555555555",
		Generation: 1,
	}
	ctrlKey, err := execution.NewControlKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		p := state.Party{Session: transcriptTestRPCSession, Name: transcriptTestRPCWorker}
		st.Agents = []state.Agent{{
			Party:     p,
			Task:      transcriptTestIssue,
			Execution: &execRef,
		}}
		st.Starts = []state.Start{{
			Party: p,
			At:    a.now.Add(-15 * time.Minute),
			Execution: &state.ExecutionRecord{
				Selector:   "remote-host",
				ReservedAt: a.now.Add(-15 * time.Minute),
				LastActive: a.now.Add(-2 * time.Minute),
				Phase:      execution.Running,
				Request: execution.StartRequest{
					Name:       transcriptTestRPCWorker,
					Harness:    transcriptTestCodex,
					Repository: sshTestRepository,
					Prompt:     "Work https://github.com/giantswarm/beekeeper/issues/2183",
					ControlKey: ctrlKey,
				},
				Handle: execution.Handle{Ref: execRef, ConversationID: transcriptTestRPCSession},
			},
		}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Seed a same-repo transcript and a different-repo transcript in the store.
	store := a.transcriptStore()
	if _, err := store.Save(transcripts.Meta{
		Key:        transcripts.SafeKey(transcriptTestCodex, transcriptTestWorkerBox, string(execRef.Execution)),
		ID:         transcriptTestRPCSession,
		Execution:  string(execRef.Execution),
		Name:       transcriptTestRPCWorker,
		RosterName: transcriptTestRPCWorker,
		Role:       workerRole,
		Repo:       sshTestRepository,
		Ref:        "giantswarm/beekeeper#2183",
		Harness:    transcriptTestCodex,
		Host:       transcriptTestWorkerBox,
		Running:    true,
	}, []transcripts.Turn{{At: a.now, Role: transcripts.RoleAssistant, Text: "Codex remote turn searchable text"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(transcripts.Meta{
		Key:        transcripts.SafeKey(claudeComm, "coord-1", "other-repo-1"),
		ID:         "other-repo-1",
		Name:       "w-other",
		RosterName: "w-other",
		Role:       workerRole,
		Repo:       devctlRepo,
		Harness:    claudeComm,
		Host:       "coord-1",
	}, []transcripts.Turn{{At: a.now, Role: transcripts.RoleAssistant, Text: "Other repo secret context"}}); err != nil {
		t.Fatal(err)
	}

	endpoint := execution.Endpoint{SSH: "coord.example.com", Executable: sshTestExecutable, Config: "/etc/beekeeper/config.yaml"}
	runner := func(ctx context.Context, _ []string, stdin []byte) ([]byte, error) {
		var reply bytes.Buffer
		if err := a.serveExecutionCoordinator(ctx, transcriptTestWorkerBox, bytes.NewReader(stdin), &reply); err != nil {
			return nil, err
		}
		return reply.Bytes(), nil
	}

	var hits []transcripts.SearchHit
	if err := execution.CoordinatorCall(ctx, endpoint, transcriptTestWorkerBox, transcriptsSearchOp, execRef, remoteTranscriptSearchReq{Query: "searchable text"}, ctrlKey, &hits, runner); err != nil {
		t.Fatalf("CoordinatorCall transcripts.search: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "Codex remote turn searchable text") {
		t.Fatalf("unexpected RPC search hits: %#v", hits)
	}
}

func TestTranscriptsUnknownHarnessHasNoGuideAccess(t *testing.T) {
	const session = "unregistered-worker"
	a, _ := newTranscriptTestApp(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", session)
	st := &state.State{}
	caller, _ := a.resolveTranscriptCaller(st)
	if caller.IsGuide || caller.Role != workerRole {
		t.Fatalf("unregistered worker received guide access: %+v", caller)
	}
	if transcripts.Allowed(caller, transcripts.Meta{ID: transcriptTestGuideSession, Role: guideRole.name}, nil, a.now) {
		t.Fatal("unregistered worker can read guide transcript")
	}
	st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: session}}}
	caller, _ = a.resolveTranscriptCaller(st)
	if !caller.IsGuide {
		t.Fatal("current guide holder lost guide access")
	}
}

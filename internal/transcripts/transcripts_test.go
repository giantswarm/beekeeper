package transcripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testGuide         = "guide"
	testBeekeeperRepo = "giantswarm/beekeeper"
	testLocalHost     = "local"
	testWorker        = "worker"
	testDevctlRepo    = "giantswarm/devctl"
	testExecution     = "exec-1"
	testRosterWorker  = "worker-bk-1"
	testConv          = "conv"
	testClaude        = "claude"
)

func TestStoreHarnessesAndRedaction(t *testing.T) {
	dir := t.TempDir()
	store := Store{
		Root: filepath.Join(dir, "transcripts"),
		Redact: func(s string) string {
			return strings.ReplaceAll(s, "SUPER_SECRET_VALUE_12345", "[redacted: a1b2c3d4]")
		},
	}

	// 1. Claude transcript with AskUserQuestion answer and a secret value.
	claudeFile := filepath.Join(dir, "claude-s1.jsonl")
	claudeLines := strings.Join([]string{
		`{"type":"custom-title","customTitle":"Guide run 14","sessionId":"s1","cwd":"/work/beekeeper"}`,
		`{"type":"user","timestamp":"2026-10-05T09:00:00Z","message":{"role":"user","content":"Check https://github.com/giantswarm/beekeeper/issues/2183"}}`,
		`{"type":"assistant","timestamp":"2026-10-05T09:01:00Z","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"Asking for retention details."},{"type":"tool_use","id":"q241","name":"AskUserQuestion","input":{"questions":[{"question":"How should transcripts be retained?"}]}}]}}`,
		`{"type":"user","timestamp":"2026-10-05T09:02:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q241","content":"User has answered your questions: \"How should transcripts be retained?\"=\"Keep 30 days and mask SUPER_SECRET_VALUE_12345\"."}]},"toolUseResult":{"answers":{"How should transcripts be retained?":"Keep 30 days and mask SUPER_SECRET_VALUE_12345"}}}`,
		`{"type":"assistant","timestamp":"2026-10-05T09:03:00Z","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"Recorded answer for Note 241."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(claudeFile, []byte(claudeLines), 0o600); err != nil {
		t.Fatal(err)
	}
	mClaude, changed, err := store.IngestFile(Meta{
		Key:        SafeKey("claude", testLocalHost, "s1"),
		ID:         "s1",
		RosterName: testGuide,
		Role:       testGuide,
		Repo:       testBeekeeperRepo,
		Ref:        "giantswarm/beekeeper#2183",
		Harness:    "claude",
		Host:       testLocalHost,
		Running:    true,
	}, claudeFile)
	if err != nil || !changed {
		t.Fatalf("IngestFile claude: changed=%v err=%v", changed, err)
	}
	if mClaude.Name != "Guide run 14" || mClaude.Model != "claude-opus-4-6" {
		t.Errorf("claude meta = %+v", mClaude)
	}

	// Verify secret was never written to disk in raw form.
	rawTurns, err := os.ReadFile(store.turnsPath(mClaude.Key))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawTurns), "SUPER_SECRET_VALUE_12345") {
		t.Fatalf("secret leaked to stored transcript: %s", rawTurns)
	}
	if !strings.Contains(string(rawTurns), "[redacted: a1b2c3d4]") {
		t.Fatalf("expected redacted marker in stored transcript: %s", rawTurns)
	}

	// Verify session ending preserves metadata and turns.
	endedAt := time.Date(2026, 10, 5, 9, 10, 0, 0, time.UTC)
	mClaudeEnded, err := store.UpdateMeta(Meta{
		Key:     mClaude.Key,
		Running: false,
		EndedAt: endedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mClaudeEnded.RosterName != testGuide || mClaudeEnded.Repo != testBeekeeperRepo || !mClaudeEnded.EndedAt.Equal(endedAt) {
		t.Fatalf("ended meta lost fields: %+v", mClaudeEnded)
	}

	// 2. Agy transcript.
	agyFile := filepath.Join(dir, "agy-t1.jsonl")
	agyLines := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","created_at":"2026-10-05T10:00:00Z","content":"<USER_REQUEST>Fix https://github.com/giantswarm/beekeeper/issues/2183</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-10-05T10:01:00Z","content":"Inspecting transcript store.","tool_calls":[{"id":"c1","name":"run_command","args":{"CommandLine":"go test ./...","toolSummary":"Run unit tests"}}]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(agyFile, []byte(agyLines), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestFile(Meta{
		Key:        SafeKey("agy", testLocalHost, "agy-1"),
		ID:         "agy-1",
		RosterName: "worker-agy",
		Role:       testWorker,
		Repo:       testBeekeeperRepo,
		Harness:    "agy",
		Host:       testLocalHost,
	}, agyFile); err != nil {
		t.Fatal(err)
	}

	// 3. Codex transcript.
	codexFile := filepath.Join(dir, "codex-t1.jsonl")
	codexLines := strings.Join([]string{
		`{"timestamp":"2026-10-05T11:00:00Z","type":"session_meta","payload":{"id":"codex-1","cwd":"/work/devctl"}}`,
		`{"timestamp":"2026-10-05T11:00:01Z","type":"turn_context","payload":{"cwd":"/work/devctl","model":"gpt-5-codex"}}`,
		`{"timestamp":"2026-10-05T11:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Work https://github.com/giantswarm/devctl/issues/99"}]}}`,
		`{"timestamp":"2026-10-05T11:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Updated release promotion logic."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(codexFile, []byte(codexLines), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestFile(Meta{
		Key:        SafeKey("codex", testLocalHost, "codex-1"),
		ID:         "codex-1",
		RosterName: "worker-codex",
		Role:       testWorker,
		Repo:       testDevctlRepo,
		Ref:        "giantswarm/devctl#99",
		Harness:    "codex",
		Host:       testLocalHost,
	}, codexFile); err != nil {
		t.Fatal(err)
	}

	// 4. Omp transcript.
	ompFile := filepath.Join(dir, "omp-t1.jsonl")
	ompLines := strings.Join([]string{
		`{"type":"session","id":"omp-1","timestamp":"2026-10-05T12:00:00Z","cwd":"/work/beekeeper"}`,
		`{"type":"title_change","title":"Omp worker"}`,
		`{"type":"message","timestamp":"2026-10-05T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Check omp transcript parser"}]}}`,
		`{"type":"message","timestamp":"2026-10-05T12:00:04Z","message":{"role":"assistant","provider":"local","model":"qwen3","content":[{"type":"text","text":"Omp parser verified."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ompFile, []byte(ompLines), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestFile(Meta{
		Key:        SafeKey("omp", testLocalHost, "omp-1"),
		ID:         "omp-1",
		RosterName: "worker-omp",
		Role:       testWorker,
		Repo:       testBeekeeperRepo,
		Harness:    "omp",
		Host:       testLocalHost,
	}, ompFile); err != nil {
		t.Fatal(err)
	}

	// 5. Remote SSH execution stream log.
	remoteStream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"remote-thread-1"}`,
		`{"type":"item.completed","timestamp":"2026-10-05T13:00:05Z","item":{"id":"i1","type":"command_execution","command":"go test ./internal/transcripts/...","aggregated_output":"ok SUPER_SECRET_VALUE_12345"}}`,
		`{"type":"item.completed","timestamp":"2026-10-05T13:00:10Z","item":{"id":"i2","type":"agent_message","text":"Remote execution finished https://github.com/giantswarm/beekeeper/pull/2200"}}`,
	}, "\n") + "\n"
	if _, err := store.AppendRemoteLog(Meta{
		Key:        SafeKey("codex", "worker-host-1", testExecution),
		Execution:  testExecution,
		RosterName: "remote-worker",
		Role:       testWorker,
		Repo:       testBeekeeperRepo,
		Ref:        "giantswarm/beekeeper#2183",
		Harness:    "codex",
		Host:       "worker-host-1",
	}, []byte(remoteStream), "1024", []Turn{
		{At: time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC), Role: RoleUser, Text: "Work https://github.com/giantswarm/beekeeper/issues/2183 on remote host"},
	}); err != nil {
		t.Fatal(err)
	}

	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 5 {
		t.Fatalf("List len = %d, want 5", len(metas))
	}

	// Search for AskUserQuestion answer across sessions.
	hits, err := store.Search(metas, "Keep 30 days", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Meta.RosterName != testGuide {
		t.Fatalf("Search AskUserQuestion answer = %+v", hits)
	}

	// Search for remote execution turn.
	remoteHits, err := store.Search(metas, "Remote execution finished", SearchOptions{Host: "worker-host-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteHits) != 1 || remoteHits[0].Meta.Execution != testExecution {
		t.Fatalf("Search remote host = %+v", remoteHits)
	}

	// Resolve by roster name, session ID, and GitHub ref URL.
	for _, q := range []string{"remote-worker", testExecution, "https://github.com/giantswarm/beekeeper/pull/2200", "giantswarm/devctl#99"} {
		if _, err := Resolve(metas, q); err != nil {
			t.Errorf("Resolve(%q) failed: %v", q, err)
		}
	}
}

func TestAccessControl(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	metas := []Meta{
		{Key: "guide-1", ID: "g1", RosterName: testGuide, Role: testGuide, Repo: testBeekeeperRepo},
		{Key: "bk-1", ID: "w1", RosterName: testRosterWorker, Role: testWorker, Repo: testBeekeeperRepo},
		{Key: "bk-2", ID: "w2", RosterName: "worker-bk-2", Role: testWorker, Repo: testBeekeeperRepo},
		{Key: "dc-1", ID: "w3", RosterName: "worker-dc-1", Role: testWorker, Repo: testDevctlRepo},
	}

	// 1. Guide reads and searches all transcripts.
	guideCaller := Caller{
		Party:      state.Party{Session: "g1", Name: testGuide},
		RosterName: testGuide,
		Role:       testGuide,
		IsGuide:    true,
	}
	if got := Filter(metas, guideCaller, nil, now); len(got) != 4 {
		t.Fatalf("guide Filter = %d, want 4", len(got))
	}

	// 2. Worker on giantswarm/beekeeper reads its own and same-repo worker transcripts,
	// but NOT the guide's transcript or giantswarm/devctl's transcript by default.
	workerCaller := Caller{
		Party:      state.Party{Session: "w1", Name: testRosterWorker},
		RosterName: testRosterWorker,
		Role:       testWorker,
		Repo:       testBeekeeperRepo,
	}
	gotDefault := Filter(metas, workerCaller, nil, now)
	if len(gotDefault) != 2 || gotDefault[0].Key != "bk-1" || gotDefault[1].Key != "bk-2" {
		t.Fatalf("worker default Filter = %+v, want bk-1 and bk-2 only", gotDefault)
	}

	// 3. Active grant for giantswarm/devctl unlocks dc-1, and active session grant unlocks guide-1.
	grants := []state.TranscriptGrant{
		{
			ID:      1,
			To:      state.Party{Name: testRosterWorker},
			Repo:    testDevctlRepo,
			Expires: now.Add(time.Hour),
		},
		{
			ID:      2,
			To:      state.Party{Name: testRosterWorker},
			Session: testGuide,
			Expires: now.Add(time.Hour),
		},
	}
	if gotGranted := Filter(metas, workerCaller, grants, now); len(gotGranted) != 4 {
		t.Fatalf("worker granted Filter = %d, want 4", len(gotGranted))
	}

	// 4. Expired grants no longer authorize access.
	later := now.Add(2 * time.Hour)
	if gotExpired := Filter(metas, workerCaller, grants, later); len(gotExpired) != 2 {
		t.Fatalf("worker expired Filter = %d, want 2", len(gotExpired))
	}
}

func TestCodexNativeKeepsCommentary(t *testing.T) {
	lines := strings.Join([]string{
		`{"timestamp":"2026-10-05T11:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the release flow"}]}}`,
		`{"timestamp":"2026-10-05T11:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Keeping the old flag because the promotion depends on it."}]}}`,
		`{"timestamp":"2026-10-05T11:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Release flow fixed."}]}}`,
	}, "\n") + "\n"
	p := parseCodex(strings.NewReader(lines))
	if len(p.Turns) != 3 {
		t.Fatalf("turns = %+v, want user, commentary and final answer", p.Turns)
	}
	if c := p.Turns[1]; c.Role != RoleAssistant || c.Phase != "commentary" || !strings.Contains(c.Text, "Keeping the old flag") {
		t.Fatalf("commentary turn = %+v", c)
	}
	if f := p.Turns[2]; f.Phase != "final_answer" || f.Text != "Release flow fixed." {
		t.Fatalf("final turn = %+v", f)
	}
}

func TestDefaultAccessRequiresWorkerRole(t *testing.T) {
	caller := Caller{Party: state.Party{Session: testRosterWorker}, Repo: testBeekeeperRepo}
	for _, role := range []string{"", "operator", testGuide, testWorker} {
		meta := Meta{ID: "other", Repo: testBeekeeperRepo, Role: role}
		if got := Allowed(caller, meta, nil, time.Now()); got != (role == testWorker) {
			t.Errorf("role %q: allowed = %v", role, got)
		}
	}
}

func TestAgyResultsAndLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agy.jsonl")
	big := strings.Repeat("x", maxResultBytes+10)
	lines := strings.Join([]string{
		`{"type":"USER_INPUT","content":"<USER_REQUEST>brief <truncated 41433 bytes></USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","content":"looking","tool_calls":[{"name":"run_command","args":{"CommandLine":"ls"}},{"name":"view_file","args":{"Path":"a.txt"}}]}`,
		`{"type":"TOOL_RESPONSE","content":"a.txt"}`,
		`{"type":"GENERIC","content":"` + big + `"}`,
		`{"type":"PLANNER_RESPONSE","content":"","tool_calls":[{"name":"run_command","args":{"CommandLine":"pwd"}}]}`,
		`{"type":"PLANNER_RESPONSE","content":"done <truncated 7215 bytes>"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := ParseFile(harnessAgy, path)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, turn := range p.Turns {
		if turn.Role == RoleTool {
			results = append(results, turn.Result)
		}
	}
	if len(results) != 3 || results[0] != "a.txt" || !strings.HasSuffix(results[1], "… <10 bytes not stored>") || results[2] != "" {
		t.Fatalf("tool results = %q", results)
	}
	l := LossOf(p.Turns, true, false)
	if want := (Loss{HarnessBytes: 48648, HarnessCuts: 2, ClippedBytes: 10, Clipped: 1, NoResult: 1}); l != want {
		t.Fatalf("loss = %+v, want %+v", l, want)
	}
	for _, part := range []string{"48648 bytes", "1 tool results are clipped", "1 tool calls have no stored result"} {
		if !strings.Contains(l.Lost()+l.Unindexed(), part) {
			t.Errorf("%+v does not say %q", l, part)
		}
	}
	if LossOf(p.Turns, false, false).NoResult != 0 {
		t.Error("a read without results reports results as lost")
	}
}

func TestLossSeparatesWhatIsGoneFromWhatIsMissing(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "brief <truncated 5 bytes>"},
		{Role: RoleTool, Text: "empty", Answered: true},
		{Role: RoleTool, Text: "none"},
		{Role: RoleTool, Text: "older clip", Result: strings.Repeat("x", maxResultBytes) + "…"},
	}
	ended := LossOf(turns, true, false)
	if ended.NoResult != 1 || ended.Pending != 0 || ended.Clipped != 1 {
		t.Fatalf("ended = %+v: empty output is an answer, an older clip still counts", ended)
	}
	if strings.Contains(ended.Lost(), "tool") || !strings.Contains(ended.Lost(), "5 bytes") {
		t.Errorf("lost = %q: only the harness's cut is gone for good", ended.Lost())
	}
	if !strings.Contains(ended.Unindexed(), "may still hold") {
		t.Errorf("unindexed = %q", ended.Unindexed())
	}
	ended.SourceGone = true
	if !strings.Contains(ended.Unindexed(), "lost too") {
		t.Errorf("unindexed with its source gone = %q", ended.Unindexed())
	}
	running := LossOf(turns[:3], true, true)
	if running.NoResult != 0 || running.Pending != 1 || running.Unindexed() != "1 tool calls have no result yet" {
		t.Fatalf("running = %+v, %q", running, running.Unindexed())
	}
	if (Loss{}).Lost()+(Loss{}).Unindexed() != "" {
		t.Error("no loss says something")
	}
}

func TestIngestReadsAnOlderAgyIndexAgainOnce(t *testing.T) {
	dir := t.TempDir()
	store := Store{Root: filepath.Join(dir, "transcripts")}
	path := filepath.Join(dir, "agy.jsonl")
	lines := `{"type":"PLANNER_RESPONSE","content":"","tool_calls":[{"name":"run_command","args":{"CommandLine":"ls"}}]}` + "\n" + `{"type":"TOOL_RESPONSE","content":"a.txt"}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// As an older version left it: same source, no result, no parser version.
	key := SafeKey(harnessAgy, testLocalHost, testConv)
	old := Meta{Key: key, ID: testConv, Harness: harnessAgy, Host: testLocalHost, SourcePath: path, SourceMod: fi.ModTime(), SourceSize: fi.Size()}
	if _, err := store.Save(old, []Turn{{Role: RoleTool, Tool: "run_command", Text: "run_command: ls"}}); err != nil {
		t.Fatal(err)
	}
	m, changed, err := store.IngestFile(Meta{Key: key, ID: testConv, Harness: harnessAgy, Host: testLocalHost}, path)
	if err != nil || !changed || m.Parser != parserVersions[harnessAgy] {
		t.Fatalf("ingest = %+v, %v, %v", m, changed, err)
	}
	turns, err := store.ReadTurns(key, ReadOptions{Results: true})
	if err != nil || len(turns) != 1 || turns[0].Result != "a.txt" {
		t.Fatalf("turns = %+v, %v", turns, err)
	}
	if _, changed, err = store.IngestFile(Meta{Key: key, ID: testConv, Harness: harnessAgy, Host: testLocalHost}, path); err != nil || changed {
		t.Fatalf("an unchanged source was read again: %v, %v", changed, err)
	}
	// Harnesses whose parser did not change keep the cheap path.
	claudeFile := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(claudeFile, []byte(`{"type":"user","timestamp":"2026-10-05T09:00:00Z","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestFile(Meta{Key: "c", Harness: testClaude, Host: testLocalHost}, claudeFile); err != nil {
		t.Fatal(err)
	}
	if _, changed, err = store.IngestFile(Meta{Key: "c", Harness: testClaude, Host: testLocalHost}, claudeFile); err != nil || changed {
		t.Fatalf("a claude transcript was read again: %v, %v", changed, err)
	}
}

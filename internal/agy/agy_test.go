package agy

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testBusyConv   = "11111111-2222-4333-8444-555555555501"
	testIdleConv   = "11111111-2222-4333-8444-555555555502"
	testRepoDir    = "/work/beekeeper"
	testOpusModel  = "claude-opus-5-5-high"
	testArgonModel = "gemini-4-argon-medium"
)

type fakeProc struct {
	pid, ppid int
	args      []string
	cwd       string
	env       []string
	age       time.Duration
}

func procRoot(t *testing.T, now time.Time, ps ...fakeProc) *proc.Table {
	t.Helper()
	root := t.TempDir()
	boot := now.Add(-time.Hour).Truncate(time.Second)
	writeFile(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu 0\nbtime %d\n", boot.Unix()))
	for _, p := range ps {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		ticks := int64(now.Add(-p.age).Sub(boot) / (time.Second / 100))
		comm := filepath.Base(p.args[0])
		f := make([]string, 22)
		for i := range f {
			f[i] = "0"
		}
		f[0], f[1], f[19], f[21] = "S", strconv.Itoa(p.ppid), strconv.FormatInt(ticks, 10), "10"
		writeFile(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) %s\n", p.pid, comm, strings.Join(f, " ")))
		writeFile(t, filepath.Join(dir, "cmdline"), strings.Join(p.args, "\x00")+"\x00")
		writeFile(t, filepath.Join(dir, "environ"), strings.Join(p.env, "\x00")+"\x00")
		if p.cwd != "" {
			if err := os.Symlink(p.cwd, filepath.Join(dir, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	tab, err := proc.ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
}

func busyTranscriptContent(at time.Time) string {
	t0 := at.UTC().Format(time.RFC3339)
	t1 := at.Add(5 * time.Second).UTC().Format(time.RFC3339)
	t2 := at.Add(10 * time.Second).UTC().Format(time.RFC3339)
	return strings.Join([]string{
		fmt.Sprintf(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":%q,"content":"<USER_REQUEST>\nFix the flaky lease test\n</USER_REQUEST>\n<USER_SETTINGS_CHANGE>\nThe user changed setting `+"`Model Selection`"+` from None to Claude Opus 5.5 (High).\n</USER_SETTINGS_CHANGE>"}`, t0),
		fmt.Sprintf(`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"input_tokens":1000,"cache_read_tokens":200,"output_tokens":50,"content":"Reading the issue first.","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"gh issue view 999 --repo giantswarm/beekeeper\"","toolSummary":"\"Read issue 999\""}}]}`, t1),
		fmt.Sprintf(`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":%q,"content":"Created At: %s\nCommand exited with code 1.\nnot found"}`, t2, t2),
	}, "\n") + "\n"
}

func idleTranscriptContent(at time.Time) string {
	t0 := at.UTC().Format(time.RFC3339)
	t1 := at.Add(2 * time.Second).UTC().Format(time.RFC3339)
	return strings.Join([]string{
		fmt.Sprintf(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":%q,"content":"<USER_REQUEST>\nPing pong\n</USER_REQUEST>\n<USER_SETTINGS_CHANGE>\nThe user changed setting `+"`Model Selection`"+` from None to Gemini 4 Argon (Medium).\n</USER_SETTINGS_CHANGE>"}`, t0),
		fmt.Sprintf(`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":%q,"input_tokens":400,"cache_read_tokens":100,"output_tokens":20,"content":"pong"}`, t1),
	}, "\n") + "\n"
}

func TestDiscoverAndEnded(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	brainDir := t.TempDir()
	stateDir := t.TempDir()

	busyPath := TranscriptPath(brainDir, testBusyConv)
	writeFile(t, busyPath, busyTranscriptContent(now.Add(-4*time.Minute)))
	_ = os.Chtimes(busyPath, now.Add(-10*time.Second), now.Add(-10*time.Second))

	idlePath := TranscriptPath(brainDir, testIdleConv)
	writeFile(t, idlePath, idleTranscriptContent(now.Add(-30*time.Minute)))
	_ = os.Chtimes(idlePath, now.Add(-20*time.Minute), now.Add(-20*time.Minute))

	startID := "1234abcd-0000-4000-8000-000000000000"
	linkConversation(InboxPath(stateDir, startID), testBusyConv)

	tab := procRoot(t, now,
		fakeProc{
			pid: 100, ppid: 1,
			args: []string{Comm, "--input-format", "stream-json", "--output-format", "stream-json", "--model", testOpusModel},
			cwd:  testRepoDir,
			age:  5 * time.Minute,
			env:  []string{EnvAgent + "=" + startID, EnvName + "=agy worker"},
		},
		// subagent and helper commands are ignored
		fakeProc{pid: 101, ppid: 100, args: []string{Comm}, cwd: testRepoDir, age: time.Minute},
		fakeProc{pid: 200, ppid: 1, args: []string{Comm, "models"}, cwd: testRepoDir, age: time.Minute},
		fakeProc{pid: 201, ppid: 1, args: []string{Comm, "-p", "/usage"}, cwd: testRepoDir, age: time.Minute},
		// unprompted interactive agy
		fakeProc{pid: 300, ppid: 1, args: []string{"/usr/bin/agy", "--model", testArgonModel}, cwd: testRepoDir, age: time.Minute},
	)

	got := map[int]*claude.Session{}
	ss := Discover(brainDir, stateDir, tab, now)
	for _, s := range ss {
		got[s.PID] = s
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d sessions, want 2 (100, 300): %+v", len(got), got)
	}
	a := got[100]
	if a == nil {
		t.Fatal("beekeeper agy agent not discovered")
	}
	if a.Harness != Harness || a.ID != testBusyConv || a.HostID != HostPrefix+startID || a.Name != "agy worker" {
		t.Errorf("agent = harness %q id %q host %q name %q", a.Harness, a.ID, a.HostID, a.Name)
	}
	if a.State != StateBusy || a.Model != testOpusModel || a.Cwd != testRepoDir {
		t.Errorf("agent state %q model %q cwd %q", a.State, a.Model, a.Cwd)
	}
	if !a.Party().Is(state.Party{HostSession: HostPrefix + startID, Name: "agy worker"}) {
		t.Error("agent party does not match roster entry")
	}

	p := got[300]
	if p == nil {
		t.Fatal("interactive agy not discovered")
	}
	if p.Transcript != "" || p.Name != "agy in beekeeper" || p.State != StateIdle || p.Model != testArgonModel {
		t.Errorf("interactive session = transcript %q name %q state %q model %q", p.Transcript, p.Name, p.State, p.Model)
	}

	ended := Ended(brainDir, ss, now.Add(-24*time.Hour))
	if len(ended) != 1 || ended[0].ID != testIdleConv || ended[0].State != StateEnded || ended[0].Name != "Ping pong" || ended[0].Model != testArgonModel {
		t.Fatalf("ended = %+v, want idle conversation", ended)
	}
}

func TestRunsSession(t *testing.T) {
	for args, want := range map[string]bool{
		"agy":                               true,
		"agy --model gemini-4-argon-medium": true,
		"agy --input-format stream-json --output-format stream-json --model claude-opus-5-5-high": true,
		"agy --conversation 1234": true,
		"agy models":              false,
		"agy mcp list":            false,
		"agy remote-control status": false,
		"agy -p /usage":           false,
		"agy --print /usage":      false,
		"agy --version":           false,
		"agy --help":              false,
		"claude":                  false,
	} {
		f := strings.Fields(args)
		p := &proc.Process{Comm: filepath.Base(f[0]), Args: f}
		if got := runsSession(p); got != want {
			t.Errorf("runsSession(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestTailFollowAndReadTranscript(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	busyPath := TranscriptPath(brainDir, testBusyConv)
	writeFile(t, busyPath, busyTranscriptContent(now.Add(-10*time.Minute)))

	turns, err := Tail(busyPath, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[0].Text != "Fix the flaky lease test" || turns[1].Role != "assistant" || turns[1].Text != "Reading the issue first." {
		t.Fatalf("Tail = %+v", turns)
	}

	follow, err := Follow(busyPath, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(follow) != 3 || follow[2].Role != claude.RoleTool || follow[2].Text != "run_command: Read issue 999" {
		t.Fatalf("Follow = %+v", follow)
	}

	w, act := ReadTranscript(busyPath, now)
	if len(w.Refs) == 0 || w.Refs[0] != "giantswarm/beekeeper#999" {
		t.Errorf("refs = %v, want giantswarm/beekeeper#999", w.Refs)
	}
	c := act.LastHour
	if c.Turns != 1 || c.ToolCalls != 1 || c.ToolErrors != 1 || c.GitHubCalls != 1 {
		t.Errorf("last hour = %+v, want 1 turn, 1 tool call, 1 error, 1 github call", c)
	}
	if c.Tokens.Input != 1000 || c.Tokens.CacheRead != 200 || c.Tokens.Output != 50 || act.Context != 1250 {
		t.Errorf("tokens = %+v, context = %d", c.Tokens, act.Context)
	}
	if act.Model != testOpusModel {
		t.Errorf("model = %q, want %s", act.Model, testOpusModel)
	}
	act.Price(config.Metrics{Models: config.DefaultModels})
	if act.Total.CostUSD == nil || *act.Total.CostUSD <= 0 {
		t.Errorf("expected %s to be priced via DefaultModels, got %v (unknown: %v)", testOpusModel, act.Total.CostUSD, act.Total.CostUnknown)
	}
}

func TestSpendTranscripts(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	busyPath := TranscriptPath(brainDir, testBusyConv)
	writeFile(t, busyPath, busyTranscriptContent(now.Add(-10*time.Minute)))
	// transcript_full.jsonl alongside transcript.jsonl must not double-count
	writeFile(t, filepath.Join(filepath.Dir(busyPath), "transcript_full.jsonl"), busyTranscriptContent(now.Add(-10*time.Minute)))

	ts := SpendTranscripts(brainDir, now.Add(-time.Hour), now.Add(time.Hour))
	if len(ts) != 1 || len(ts[0].Requests) != 1 {
		t.Fatalf("SpendTranscripts = %+v, want 1 transcript with 1 request", ts)
	}
	req := ts[0].Requests[0]
	if req.Model != testOpusModel || req.Tokens.Input != 1000 || req.Tokens.CacheRead != 200 || req.Tokens.Output != 50 {
		t.Errorf("req = %+v", req)
	}
}

func TestProjectForDirAndParseModels(t *testing.T) {
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "b.json"), `{"id":"proj-b","projectResources":{"resources":[{"folderUri":"file:///work/other"}]}}`)
	writeFile(t, filepath.Join(projDir, "a.json"), `{"id":"proj-a","projectResources":{"resources":[{"gitFolder":{"folderUri":"file:///work/beekeeper/"}}]}}`)

	if got := ProjectForDir(projDir, "/work/beekeeper"); got != "proj-a" {
		t.Errorf("ProjectForDir = %q, want proj-a", got)
	}
	if got := ProjectForDir(projDir, "/work/missing"); got != "" {
		t.Errorf("ProjectForDir missing = %q, want empty", got)
	}

	raw := "\r⠋ Fetching available models...\rgemini-4-argon-medium     Gemini 4 Argon (Medium)\nclaude-opus-5-5-high      Claude Opus 5.5 (High)\n"
	models := ParseModelsOutput(raw)
	if len(models) != 2 || models[0] != "gemini-4-argon-medium" || models[1] != "claude-opus-5-5-high" {
		t.Errorf("ParseModelsOutput = %v", models)
	}
}

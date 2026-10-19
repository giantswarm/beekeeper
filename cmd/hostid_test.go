package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
)

// The desktop restarts a worker's CLI under a new session id and keeps its
// host id: every lookup of its start finds it by that host id.
const (
	firstCLI     = "cli-first"
	restartedCLI = "cli-restarted"
	restartHost  = "local_" + firstCLI
	restartName  = "test: restarted worker"
)

// restartedStart is the start of the worker the desktop restarted.
func restartedStart(dir, model string) state.Start {
	return state.Start{Party: state.Party{Session: firstCLI, HostSession: restartHost, Name: restartName},
		Mode: state.ModeBypass, Dir: dir, Model: model, At: time.Now().Add(-time.Hour)}
}

// restarted is the worker as it registers from its restarted CLI.
var restarted = state.Party{Session: restartedCLI, HostSession: restartHost, Name: restartName}

func seed(t *testing.T, a *app, f func(st *state.State)) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { f(st); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

// A hand-over of a restarted worker keeps the model and folder of its
// start and passes on its brief.
func TestReadHandoverFindsARestartedWorker(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, nil)
	seed(t, a, func(st *state.State) {
		st.Starts = []state.Start{restartedStart("/w", sonnetModel)}
		st.Agents = []state.Agent{{Party: restarted, Task: countTask}}
	})
	writeFile(t, filepath.Join(a.cfg.Claude.ProjectsDir, "-w", restartedCLI+".jsonl"),
		`{"type":"user","message":{"role":"user","content":"# Count\ncount a, b"}}`+"\n")
	h, err := a.readHandover(restartName)
	if err != nil {
		t.Fatal(err)
	}
	if h.model != sonnetModel || h.dir != "/w" || !strings.HasPrefix(h.brief, "# Count") {
		t.Errorf("hand-over of a restarted worker: model %q, dir %q, brief %q", h.model, h.dir, h.brief)
	}
}

// A wake of a worker registered from its restarted CLI keeps its start's
// bypass and model.
func TestResolveWakeFindsARestartedWorker(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Claude: config.Claude{ProjectsDir: filepath.Join(root, "projects"), DesktopDir: filepath.Join(root, "desk")}}
	writeFile(t, filepath.Join(cfg.Claude.ProjectsDir, "-w", restartedCLI+".jsonl"), `{"type":"user"}`+"\n")
	writeFile(t, filepath.Join(cfg.Claude.DesktopDir, "a", "b", restartHost+".json"),
		`{"sessionId":"`+restartHost+`","cliSessionId":"`+restartedCLI+`","cwd":"/w","permissionMode":"acceptEdits","title":"`+restartName+`","model":"claude-opus-5-5"}`)
	st := &state.State{Starts: []state.Start{restartedStart("/w", sonnetModel)}}
	got, err := resolveWake(cfg, st, state.Agent{Party: restarted})
	want := wakeTarget{name: restartName, id: restartedCLI, host: restartHost, dir: "/w", mode: state.ModeBypass, model: sonnetModel}
	if err != nil || got != want {
		t.Errorf("resolveWake = %+v, %v, want %+v", got, err, want)
	}
}

// A role's successor of a restarted holder starts in the folder of the
// holder's start.
func TestStartSuccessorFindsARestartedHolder(t *testing.T) {
	a, _ := stubApp(t)
	missing := filepath.Join(t.TempDir(), "the-start's-folder")
	holder := state.Party{Session: restartedCLI, HostSession: restartHost, Name: "Supervisor run 11"}
	seed(t, a, func(st *state.State) {
		*st = *handOverState()
		st.Supervisor.Party = holder
		st.Starts = []state.Start{restartedStart(missing, "")}
	})
	a.cfg.Supervisor.RelayTTL.Duration = 15 * time.Minute
	_, _, err := a.startSuccessor(context.Background(), supervisorRole, holder, holder, "")
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("startSuccessor of a restarted holder: %v, want the start's folder %s", err, missing)
	}
}

// A restarted worker's permission request gets the bypass allow; a
// session no start knows by either id gets none.
func TestPermissionRequestAllowsARestartedWorker(t *testing.T) {
	a, dir := permissionApp(t)
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = []state.Start{restartedStart(dir, "")}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ev := []byte(strings.Replace(string(permissionEvent(restartedCLI)), `"default"`, `"acceptEdits"`, 1))
	allow := string(guard.PermissionDecision(takeover.Allow, ""))
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", restartHost)
	if out := a.permissionRequest(context.Background(), ev); string(out) != allow {
		t.Errorf("a restarted worker got %s, want the allow", out)
	}
	// The person took the worker over under its first CLI id: the
	// restarted CLI's request is held for the screen, never allowed.
	tdir := takeover.Dir(dir)
	if err := takeover.Take(tdir, firstCLI, takeover.Flag{PID: os.Getpid(), By: tPerson}); err != nil {
		t.Fatal(err)
	}
	defer func(g, p time.Duration) { takeoverGiveUp, takeoverPoll = g, p }(takeoverGiveUp, takeoverPoll)
	takeoverGiveUp, takeoverPoll = 40*time.Millisecond, 5*time.Millisecond
	if out := a.permissionRequest(context.Background(), ev); out != nil {
		t.Errorf("a restarted worker taken over under its first CLI got %s, want none", out)
	}
	if err := takeover.Release(tdir, firstCLI); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "local_stranger")
	if out := a.permissionRequest(context.Background(), ev); out != nil {
		t.Errorf("an unknown session got %s, want none", out)
	}
}

// The model a start chose is found by the start's host id as well.
func TestChosenModelByTheHostID(t *testing.T) {
	a, _ := stubApp(t)
	seed(t, a, func(st *state.State) {
		st.Starts = []state.Start{{Party: state.Party{HostSession: restartHost}, Mode: state.ModeBypass, Model: sonnetModel}}
	})
	if got := a.chosenModel(firstCLI); got != sonnetModel {
		t.Errorf("chosenModel = %q, want %q", got, sonnetModel)
	}
}

// A restarted worker's idle desktop CLI may steward: beekeeper started it,
// and its roster row under the new CLI id is no hand-over.
func TestStewardsKnowsARestartedWorker(t *testing.T) {
	now := time.Now()
	st := &state.State{Starts: []state.Start{restartedStart("/w", "")}, Agents: []state.Agent{{Party: restarted}}}
	tbl := &proc.Table{ByPID: map[int]*proc.Process{7: {PID: 7, Args: desktopArgs}}}
	s := &claude.Session{ID: restartedCLI, HostID: restartHost, PID: 7, Name: restartName, LastActive: now.Add(-time.Hour)}
	if !stewards(st, tbl, s, "local_target", now) {
		t.Error("a restarted worker's idle CLI may not steward")
	}
}

// A restarted worker's desktop session is archivable as beekeeper's start.
func TestArchivableRecordKnowsARestartedWorker(t *testing.T) {
	root := t.TempDir()
	a := &app{cfg: &config.Config{Claude: config.Claude{DesktopDir: filepath.Join(root, "desk")}}}
	writeFile(t, filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", restartHost+".json"),
		`{"sessionId":"`+restartHost+`","cliSessionId":"`+restartedCLI+`","title":"`+restartName+`"}`)
	st := &state.State{Starts: []state.Start{restartedStart("/w", "")}}
	if host, why := a.archivableRecord(st, restarted); host != restartHost || why != "" {
		t.Errorf("archivableRecord = %q, %q, want %q", host, why, restartHost)
	}
}

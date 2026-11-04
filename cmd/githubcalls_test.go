package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestHookRecordsGitHubCalls(t *testing.T) {
	var out bytes.Buffer
	a, dir := scopeApp(t, &out)
	bin := t.TempDir()
	marker := filepath.Join(bin, "called")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o700); err != nil { //nolint:gosec // a test-owned executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	raw := func(cwd, command string) string {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", executionOmpSession: "finished-session", executionOmpCwd: cwd, "tool_name": tToolBash, "tool_input": map[string]string{"command": command}})
		return string(b)
	}
	// No gh executable or credentials are needed to record the call.
	runHook(t, a, &out, "pretooluse", raw(filepath.Join(dir, "desk"), "gh project item-list 1 --query narrow"))
	runHook(t, a, &out, "pretooluse", raw(filepath.Join(dir, "outside"), "gh pr list"))
	// Denied board-wide reads must not be attributed as spending.
	runHook(t, a, &out, "pretooluse", raw(filepath.Join(dir, "desk"), "gh project item-list 1"))
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(0, func(e state.Event) bool { return e.Verb == verbGitHubCall })
	if err != nil || len(events) != 1 {
		t.Fatalf("events %+v: %v", events, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("recording invoked gh: %v", err)
	}
	a.store = store
	a.json = true
	out.Reset()
	log := a.logCmd()
	log.SetArgs([]string{"--verb", verbGitHubCall})
	if err := log.Execute(); err != nil {
		t.Fatal(err)
	}
	var logged []state.Event
	if err := json.Unmarshal(out.Bytes(), &logged); err != nil || len(logged) != 1 || logged[0].By.Session != "finished-session" {
		t.Fatalf("log: %s (%v)", out.String(), err)
	}
	if events[0].By.Session != "finished-session" || !strings.Contains(events[0].Detail, `"heavy":true`) || strings.Contains(events[0].Detail, "narrow") {
		t.Fatal(events)
	}
}

func TestGitHubWindowCallers(t *testing.T) {
	a, _ := budgetApp(t)
	reset := a.now.Add(10 * time.Minute)
	add := func(session, class string, heavy bool, at time.Time) {
		detail, _ := json.Marshal(map[string]any{"class": class, "heavy": heavy})
		if err := a.store.Record(state.Event{At: at, By: state.Party{Session: session}, Verb: verbGitHubCall, Detail: string(detail)}); err != nil {
			t.Fatal(err)
		}
	}
	add("old", "gh project item-list", true, reset.Add(-time.Hour-time.Nanosecond))
	add("finished-run", "gh project item-list", true, reset.Add(-time.Hour))
	add("future", "gh project item-list", true, a.now.Add(time.Second))
	for range 3 {
		add("poller", "gh pr checks", false, a.now.Add(-time.Minute))
	}
	callers, err := githubWindowCallers(a, &github.GraphQL{Reset: reset})
	if err != nil || len(callers) != 2 || callers[0].Session != "finished-run" || callers[0].Heavy != 1 || callers[1].Calls != 3 {
		t.Fatalf("%+v %v", callers, err)
	}
	callers, err = githubWindowCallers(a, &github.GraphQL{})
	if err != nil || len(callers) != 3 {
		t.Fatalf("unknown reset: %+v %v", callers, err)
	}
}

func TestBudgetAndWatchNameFinishedGitHubCaller(t *testing.T) {
	refuse := true
	refusingGitHub(t, &refuse)
	a, out := budgetApp(t)
	if err := a.store.Record(state.Event{At: a.now.Add(-time.Minute), By: state.Party{Session: "finished-session"}, Verb: verbGitHubCall, Detail: `{"class":"gh project item-list","heavy":true}`}); err != nil {
		t.Fatal(err)
	}
	cmd := a.budgetCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "finished-session: gh project item-list (1 calls, 1 heavy)") {
		t.Fatal(out.String())
	}
	out.Reset()
	w := &watcher{app: a, last: map[string]time.Time{}}
	w.budget(context.Background(), a.now)
	if !strings.Contains(out.String(), "window: finished-session: gh project item-list") {
		t.Fatal(out.String())
	}
}

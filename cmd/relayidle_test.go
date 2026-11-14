package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

func TestIdleRelayDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		idle      time.Duration
		threshold config.Tokens
		subagent  bool
		want      bool
	}{
		{"idle hour above half", time.Hour, 200_000, false, true},
		{"under an hour", time.Hour - time.Second, 200_000, false, false},
		{"below half", time.Hour, 400_000, false, false},
		{"exactly half", time.Hour, 326_036, false, false},
		{"active subagent", time.Hour, 200_000, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: supA, Since: relayNow.Add(-3 * time.Hour)}}}
			s := &claude.Session{ID: supA.Session, HostID: supA.HostSession, Name: supA.Name, Transcript: supervisorTranscript, LastActive: relayNow.Add(-tc.idle)}
			if tc.subagent {
				s.LastSubagent = relayNow.Add(-time.Minute)
			}
			sessions := []*claude.Session{s}
			q := idleRoleReading(st.GuideRole(), sessions, relayNow, tc.threshold, nil)
			lines, _ := guideRole.FireRelayDue(st, q, nil, relayNow)
			if (len(lines) > 0) != tc.want {
				t.Fatalf("lines %q, want due %v", lines, tc.want)
			}
			if tc.want && !strings.Contains(lines[0], "after an idle hour") {
				t.Fatal(lines)
			}
			if q := idleRoleReading(st.GuideRole(), sessions, relayNow.Add(time.Minute), tc.threshold, nil); tc.want && q.Checked {
				t.Fatal("repeated idle due")
			}
			s.LastActive = time.Time{}
			if q := idleRoleReading(state.Role{Holder: st.Guide.Holder}, sessions, relayNow, tc.threshold, nil); q.Checked {
				t.Fatal("unknown activity counted as idle")
			}
		})
	}
}

func TestGuidePromptBudget(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Guide.Skill = guideName
	plat.Machine = tableMachine{plat.Machine}
	standing := strings.Repeat("A desk instruction.\n", 10_000)
	if err := os.WriteFile(a.standingPath(), []byte(standing), 0o600); err != nil {
		t.Fatal(err)
	}
	var queue []queueItem
	for i := range 40 {
		queue = append(queue, queueItem{Note: &state.Note{ID: i + 1, Kind: noteDecision, For: "Person", Text: fmt.Sprintf("decision %d: %s", i+1, strings.Repeat("detailed context ", 1000))}})
	}
	if err := a.printGuidePrompt(state.Role{}, queue, standing, nil, &state.State{}); err != nil {
		t.Fatal(err)
	}
	prompt := out.String()
	if len(prompt) > 25_000 {
		t.Fatalf("prompt %d bytes exceeds conservative 25k token budget", len(prompt))
	}
	for i := range 40 {
		if !strings.Contains(prompt, fmt.Sprintf("#%d ", i+1)) {
			t.Fatalf("missing decision %d", i+1)
		}
	}
	if strings.Contains(prompt, standing) || strings.Count(prompt, "/guide ") != 1 {
		t.Fatal("repeated instructions")
	}
	for _, s := range []string{a.standingPath(), "guide queue --full", "handover --full"} {
		if !strings.Contains(prompt, s) {
			t.Fatalf("missing retrieval path %q", s)
		}
	}
	t.Logf("40-decision prompt: %d bytes, <= %d byte-level tokens", len(prompt), len(prompt))
	full, err := runIn(a, a.guideHandoverCmd(), "--full")
	if err != nil || !strings.Contains(full, strings.TrimSpace(standing)) {
		t.Fatalf("full hand-over lost the standing instructions: %v", err)
	}
}

func TestGuidePromptReferencesInstructionFile(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Guide.Instructions = a.standingPath()
	instructions := strings.Repeat("Follow this instruction.\n", 10_000)
	if err := os.WriteFile(a.cfg.Guide.Instructions, []byte(instructions), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.printGuidePrompt(state.Role{}, nil, "", nil, &state.State{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), a.cfg.Guide.Instructions) || strings.Contains(out.String(), instructions) {
		t.Fatal("instruction file was not referenced")
	}
}

// The complete feed remains silent while idle, then couples a new decision
// with the relay advice before a 163k context is warmed again.
func TestGuideFeedRelaysOnANewLineAfterIdle(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Guide.Person = personTimo
	runLive(t, a, map[int]string{1: agentOne})
	projects := t.TempDir()
	a.cfg.Claude.ProjectsDir = projects
	transcript := filepath.Join(projects, "desk", "s1.jsonl")
	raw := []byte(`{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","message":{"id":"idle-fixture","usage":{"input_tokens":1,"cache_read_input_tokens":163017}}}` + "\n")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.guideFeed(t.Context()); err != nil {
		t.Fatal(err)
	}
	idle := a.now.Add(-time.Hour)
	if err := os.Chtimes(transcript, idle, idle); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(time.Second)
	if lines, err := a.guideFeed(t.Context()); err != nil || len(lines) != 0 {
		t.Fatalf("idle without news: %q, %v", lines, err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{{ID: 1, Kind: noteDecision, For: personTimo, Text: "choose the next step", By: agentC}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	lines, err := a.guideFeed(t.Context())
	text := strings.Join(lines, "\n")
	if err != nil || !strings.Contains(text, "GUIDE DECISION:") || !strings.Contains(text, "GUIDE RELAY DUE:") || !strings.Contains(text, "after an idle hour") {
		t.Fatalf("new line after idle: %q, %v", lines, err)
	}
	a.now = a.now.Add(time.Second)
	if lines, err := a.guideFeed(t.Context()); err != nil || len(lines) != 0 {
		t.Fatalf("repeated idle relay: %q, %v", lines, err)
	}
}

// A busy run keeps adding requests within a turn. No waiting-session or
// decision event is needed to announce the crossing or repeat the warning.
func TestGuideFeedRelayDueCrossesMidTurnAndRepeats(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, map[int]string{1: agentOne})
	a.cfg.Guide.RelayAt = 400_000
	a.guideDues = relayDues{}
	a.cfg.Claude.ProjectsDir = t.TempDir()
	transcript := filepath.Join(a.cfg.Claude.ProjectsDir, "desk", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	appendRequest := func(tokens int) {
		t.Helper()
		f, err := os.OpenFile(transcript, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // a test fixture
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(f, "{\"type\":\"assistant\",\"message\":{\"id\":\"request-%d\",\"usage\":{\"input_tokens\":%d}}}\n", tokens, tokens)
		closeErr := f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	poll := func(want bool) {
		t.Helper()
		lines, err := a.guideFeed(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, line := range lines {
			if strings.Contains(line, "GUIDE RELAY DUE:") {
				count++
			}
		}
		if (count == 1) != want || count > 1 {
			t.Fatalf("due=%v: %q", want, lines)
		}
	}
	appendRequest(399_000)
	poll(false)
	a.now = a.now.Add(time.Second)
	appendRequest(401_000)
	poll(true)
	first := a.now
	a.now = first.Add(watch.RelayDueRepeat - time.Second)
	poll(false)
	a.now = first.Add(watch.RelayDueRepeat)
	poll(true)
	a.now = a.now.Add(watch.RelayDueRepeat)
	poll(true)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Guide.RelayDue.Reported.Equal(first) {
		t.Fatal("repeat reset the relay grace")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Relay = &state.Relay{From: st.Guide.Holder.Party, To: supB, At: a.now, Expires: a.now.Add(time.Hour)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(watch.RelayDueRepeat)
	poll(false)
}

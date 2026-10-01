package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
	"github.com/giantswarm/beekeeper/internal/tui"
)

// Fixtures of the take-over and screen-message tests.
const (
	tTakenSession = "7a1b2c3d-0000-4000-8000-000000000002"
	tPerson       = "Grace"
	tBeeName      = "bee"
	tToolBash     = "Bash"
)

// permissionApp is an app whose configuration names a fresh state folder,
// as the hook loads it.
func permissionApp(t *testing.T) (*app, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &app{cfgPath: cfg}, dir
}

func permissionEvent(session string) []byte {
	return []byte(`{"hook_event_name":"PermissionRequest","session_id":"` + session +
		`","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"rm -r build"}}`)
}

// A session not taken over gets no decision without waiting; a taken-over
// one gets the screen's answer; a held request returns no decision at the
// give-up, and so does one whose screen is gone.
func TestPermissionRequestTakeOver(t *testing.T) {
	a, dir := permissionApp(t)
	tdir := takeover.Dir(dir)
	start := time.Now()
	if out := a.permissionRequest(context.Background(), permissionEvent(tTakenSession)); out != nil {
		t.Fatalf("a session not taken over got %s", out)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("a session not taken over waited %s", el)
	}

	if err := takeover.Take(tdir, tTakenSession, takeover.Flag{PID: os.Getpid(), By: tPerson}); err != nil {
		t.Fatal(err)
	}
	defer func(g, p time.Duration) { takeoverGiveUp, takeoverPoll = g, p }(takeoverGiveUp, takeoverPoll)
	takeoverGiveUp, takeoverPoll = 5*time.Second, 5*time.Millisecond
	go func() {
		for range 1000 {
			if rs, _ := takeover.Pending(tdir, tTakenSession); len(rs) == 1 {
				if !strings.Contains(string(rs[0].Input), "rm -r build") {
					t.Errorf("the held request lost its input: %s", rs[0].Input)
				}
				_ = takeover.Answer(tdir, tTakenSession, rs[0].ID, takeover.Decision{Behavior: takeover.Deny, Message: "not that"})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	out := a.permissionRequest(context.Background(), permissionEvent(tTakenSession))
	var got struct {
		Out struct {
			Event    string `json:"hookEventName"`
			Decision struct{ Behavior, Message string }
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.Out.Event != "PermissionRequest" ||
		got.Out.Decision.Behavior != takeover.Deny || got.Out.Decision.Message != "not that" {
		t.Fatalf("taken over: %s (%v), want the screen's deny", out, err)
	}

	takeoverGiveUp = 40 * time.Millisecond
	start = time.Now()
	if out := a.permissionRequest(context.Background(), permissionEvent(tTakenSession)); out != nil {
		t.Fatalf("an unanswered request got %s, want none at the give-up", out)
	}
	if el := time.Since(start); el < takeoverGiveUp || el > 2*time.Second {
		t.Errorf("the give-up came after %s, want %s", el, takeoverGiveUp)
	}
	if rs, _ := takeover.Pending(tdir, tTakenSession); len(rs) != 0 {
		t.Errorf("a handed-back request is still pending: %+v", rs)
	}

	if err := takeover.Take(tdir, tTakenSession, takeover.Flag{PID: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	takeoverGiveUp = 5 * time.Second
	start = time.Now()
	if out := a.permissionRequest(context.Background(), permissionEvent(tTakenSession)); out != nil || time.Since(start) > 500*time.Millisecond {
		t.Errorf("a flag whose screen is gone held the request (%s) or answered %s", time.Since(start), out)
	}
}

// The screen marks a session its running screen took over with the
// requests held for it, and its answer reaches the held request.
func TestCollectorApprovals(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir}
	cfg.Guide.Person = tPerson
	c := &collector{a: &app{cfg: cfg, store: store}}
	tdir := takeover.Dir(dir)
	if err := takeover.Take(tdir, tTakenSession, takeover.Flag{PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type held struct {
		d  takeover.Decision
		ok bool
	}
	done := make(chan held, 1)
	go func() {
		d, ok, _ := takeover.Hold(ctx, tdir, takeover.Request{ID: "r1", Session: tTakenSession, Tool: tToolBash,
			Input: json.RawMessage(`{"command":"rm -r build\nls","description":"Clean the build"}`), At: time.Now()}, func(int) bool { return true }, 5*time.Millisecond)
		done <- held{d, ok}
	}()
	var ss []tui.Session
	for range 1000 {
		ss = c.withApprovals([]tui.Session{{Name: tBeeName, ID: tTakenSession}, {Name: "wasp", ID: "sid-wasp"}, {Name: "no id"}})
		if len(ss[0].Approvals) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ss[0].TakenOver || len(ss[0].Approvals) != 1 || ss[1].TakenOver || ss[2].TakenOver {
		t.Fatalf("sessions = %+v, want bee taken over with one approval", ss)
	}
	ap := ss[0].Approvals[0]
	want := []string{"command: rm -r build", "  ls", "description: Clean the build"}
	if ap.Gist != "Bash: Clean the build" || strings.Join(ap.Detail, "|") != strings.Join(want, "|") {
		t.Errorf("approval = %+v, want the gist and %v", ap, want)
	}
	if err := c.Answer(context.Background(), tTakenSession, ap.ID, false); err != nil {
		t.Fatal(err)
	}
	if h := <-done; !h.ok || h.d.Behavior != takeover.Deny || !strings.Contains(h.d.Message, tPerson) {
		t.Errorf("the held request got %+v, want Grace's deny", h)
	}
	if err := c.Release(context.Background(), tTakenSession); err != nil {
		t.Fatal(err)
	}
	if ss := c.withApprovals([]tui.Session{{Name: tBeeName, ID: tTakenSession}}); ss[0].TakenOver {
		t.Error("a released session still shows taken over")
	}
}

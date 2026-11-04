package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
)

func TestRestartedTakeoverQueuesStayVisibleAndAnswerable(t *testing.T) {
	dir := t.TempDir()
	c := &collector{a: &app{cfg: &config.Config{StateDir: dir}}}
	st := &state.State{Starts: []state.Start{restartedStart(dir, "")}}
	v := &view{st: st, Sessions: []*sessionView{{Session: &claude.Session{ID: restartedCLI, HostID: restartHost, Name: restartName}}}}
	tdir := takeover.Dir(dir)
	at := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{firstCLI, restartedCLI} {
		if err := takeover.Take(tdir, id, takeover.Flag{PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		folder := filepath.Join(tdir, id)
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		r := takeover.Request{ID: id + "-request", Session: id, Tool: tToolBash, At: at.Add(time.Duration(1-i) * time.Minute)}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, r.ID+".request"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		ss := c.withApprovals(uiSessions(v, nil))
		if len(ss) != 1 || ss[0].ID != restartedCLI || ss[0].TakeoverAlias != firstCLI || !ss[0].TakenOver || len(ss[0].Approvals) != i+1 {
			t.Fatalf("restart pane = %+v", ss)
		}
		if i == 0 && ss[0].Approvals[0].Session != firstCLI {
			t.Fatal("original queue was not exposed")
		}
		if i == 1 && ss[0].Approvals[0].Session != restartedCLI {
			t.Fatal("combined queues are not oldest first")
		}
	}
	ss := c.withApprovals(uiSessions(v, nil))
	for _, ap := range ss[0].Approvals {
		if err := c.Answer(context.Background(), ap.Session, ap.ID, true); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(tdir, ap.Session, ap.ID+".answer"))
		var d takeover.Decision
		if err != nil || json.Unmarshal(b, &d) != nil || d.Behavior != takeover.Allow {
			t.Fatalf("answer for %s = %s, %v", ap.Session, b, err)
		}
	}
	// A stranger with another desktop host must not inherit the old queue.
	v.Sessions[0].HostID = "local_stranger"
	if x := uiSessions(v, nil)[0]; x.TakeoverAlias != "" {
		t.Fatalf("stranger inherited %q", x.TakeoverAlias)
	}
}

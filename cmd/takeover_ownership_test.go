package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
	"github.com/giantswarm/beekeeper/internal/tui"
)

func TestOldCollectorCleanupPreservesNewScreen(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	makeScreen := func() *collector { return &collector{a: &app{cfg: &config.Config{StateDir: dir}, store: store}} }
	a, b := makeScreen(), makeScreen()
	old := takeover.Flag{PID: os.Getpid(), By: "screen A", Since: time.Now().UTC()}
	next := old
	next.By, next.Since = "screen B", old.Since.Add(time.Second)
	if err := a.takeOwned(tTakenSession, old); err != nil {
		t.Fatal(err)
	}
	if err := b.Release(context.Background(), tTakenSession); err != nil {
		t.Fatal(err)
	}
	if err := b.takeOwned(tTakenSession, next); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(takeover.Dir(dir), tTakenSession)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	r := takeover.Request{ID: "held-by-B", Session: tTakenSession, Tool: tToolBash}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, r.ID+".request"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseOwned(context.Background(), tTakenSession); err != nil {
		t.Fatal(err)
	}
	if f, ok := takeover.Taken(takeover.Dir(dir), tTakenSession, proc.Alive); !ok || f.By != next.By {
		t.Fatalf("B's takeover = %+v, %v", f, ok)
	}
	ss := b.withApprovals([]tui.Session{{ID: tTakenSession}})
	if !ss[0].TakenOver || len(ss[0].Approvals) != 1 {
		t.Fatalf("B lost its held approval: %+v", ss)
	}
	if err := b.ReleaseOwned(context.Background(), tTakenSession); err != nil {
		t.Fatal(err)
	}
	if _, ok := takeover.Taken(takeover.Dir(dir), tTakenSession, proc.Alive); ok {
		t.Fatal("B could not clean up its own takeover")
	}
}

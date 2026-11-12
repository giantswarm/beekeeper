package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// Run by the dedicated CI job on branches and release tags. Keep the 1 GiB
// disk fixture out of ordinary unit-test matrices, while making it mandatory
// in every release pipeline.
func TestLargeHistoryStartupAndPass(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_LARGE_HISTORY") != "1" {
		t.Skip("dedicated large-history CI job")
	}
	r := newLoopRig(t)
	// Use the real FileStore, including its persisted history projections.
	store, err := state.Open(r.a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	r.a.store = store
	path := filepath.Join(store.Dir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // isolated fixture under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	line := fmt.Sprintf("{\"at\":\"2026-09-24T00:00:00Z\",\"verb\":\"watch.history-fixture\",\"detail\":%q}\n", strings.Repeat("history ", 300))
	const count = 450000
	var bytesWritten int64
	for i := range count {
		record := line
		if i%1000 == 0 {
			record = fmt.Sprintf("{\"at\":\"2026-09-24T00:00:00Z\",\"verb\":\"supervisor.stage\",\"detail\":\"[history-fixture %d] done\"}\n", i)
		}
		bytesWritten += int64(len(record))
		if _, err := w.WriteString(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if bytesWritten < 1<<30 {
		t.Fatal("large-history fixture fell below 1 GiB")
	}
	// Structural regression: a restarted reader must not decode the 1 GiB
	// prefix again, regardless of how fast the CI machine happens to be.
	decoded := 0
	keep := func(state.Event) bool { decoded++; return false }
	if _, err := store.FollowSnapshot("large-history-projection", "v1", keep); err != nil {
		t.Fatal(err)
	}
	if decoded < count {
		t.Fatalf("fixture was not fully scanned: %d events", decoded)
	}
	restarted, err := state.Open(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	decoded = 0
	if _, err := restarted.FollowSnapshot("large-history-projection", "v1", keep); err != nil || decoded != 0 {
		t.Fatalf("restart replayed %d historical events: %v", decoded, err)
	}
	oldList := loop.ReadOpenPulls
	t.Cleanup(func() { loop.ReadOpenPulls = oldList })
	loop.ReadOpenPulls = func(context.Context, func() ([]string, error), []string) ([]string, error) { return nil, nil }
	for _, label := range []string{"cold migration", "checkpoint restart"} {
		reopened, err := state.Open(store.Dir())
		if err != nil {
			t.Fatal(err)
		}
		r.a.store = reopened
		started := time.Now()
		l, err := r.a.newSupervisorLoop(false)
		if err != nil {
			t.Fatal(err)
		}
		startup := time.Since(started)
		if startup >= update.HeartbeatGrace {
			t.Fatalf("%s startup %s exceeds probation grace %s", label, startup, update.HeartbeatGrace)
		}
		l.Once = true
		l.Lifecycle = &loop.LoopLifecycle{Version: loop.NormalizeLoopVersion(loop.LoopRunningVersion()), Started: started}
		started = time.Now()
		err = l.Pass(t.Context())
		l.PassActions.Wait()
		l.MergeActions.Wait()
		l.TimerActs.Wait()
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(started)
		if elapsed >= 10*time.Minute {
			t.Fatalf("%s pass %s exceeds WIP freshness bound", label, elapsed)
		}
		var life loop.LoopLifecycle
		if found, err := reopened.ReadFile(loop.LoopLifecycleFile, &life); err != nil || !found || life.At.IsZero() {
			t.Fatalf("no first-pass health evidence: %+v %v", life, err)
		}
		t.Logf("%s: %d events, %d bytes; startup=%s pass=%s", label, count, bytesWritten, startup, elapsed)
	}
}

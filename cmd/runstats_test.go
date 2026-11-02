package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/runrecord"
	"github.com/giantswarm/beekeeper/internal/state"
)

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed executable and typed arguments; test fixtures
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}

// A run names what it builds: the worktree's repository and HEAD, so
// `run stats` counts per repository; outside such a worktree, nothing.
func TestWorktreeIdentity(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "remote", "add", "origin", "git@github.com:giantswarm/beekeeper.git")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "main.go")
	gitTest(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "x")
	t.Chdir(dir)
	a := &app{}
	repo, head := a.worktreeIdentity(t.Context())
	if repo != sshTestRepository || len(head) != 40 {
		t.Fatalf("repo=%q head=%q", repo, head)
	}
	// Not a GitHub origin: no repository, the HEAD stays.
	gitTest(t, dir, "remote", "set-url", "origin", "https://git.example/host/repo.git")
	repo, head2 := a.worktreeIdentity(t.Context())
	if repo != "" || head2 != head {
		t.Fatalf("repo=%q head=%q", repo, head2)
	}
	t.Chdir(t.TempDir())
	repo, head = a.worktreeIdentity(t.Context())
	if repo != "" || head != "" {
		t.Fatalf("outside a worktree: repo=%q head=%q", repo, head)
	}
}

// The local run's record shape keeps the facts between the scope and the
// slot: RunScope reads the scope and Repository the repository out of the
// same detail, whatever the run's class says after them.
func TestRunRecordLocalShape(t *testing.T) {
	d := "memcap-1-2.scope giantswarm/beekeeper@16a89d96 shared slot 1 max 12G: go test ./..."
	if guard.RunScope(d) != "memcap-1-2.scope" || runrecord.Repository(d) != "giantswarm/beekeeper" {
		t.Fatalf("scope %q repo %q", guard.RunScope(d), runrecord.Repository(d))
	}
	if runrecord.Remote(d) {
		t.Fatal("local run read as remote")
	}
}

// run stats counts a window's records per repository: local vs remote, the
// fallbacks, the minutes and the failures, an end attributed through the
// start that opened it.
func TestRunStats(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	evs := []state.Event{
		// a warm-cache remote build that failed: its tail is output text,
		// not a repository, and must not move the command.
		{At: now.Add(-3 * time.Hour), Verb: runrecord.Start, Detail: "remote:pod-a.3f2a9c1b giantswarm/beekeeper@16a89d96 ns build-workers cache warm: go test ./..."},
		{At: now.Add(-3*time.Hour + 120*time.Second), Verb: runrecord.End, Detail: "remote:pod-a.3f2a9c1b exit 1 after 2m0s, failed: --- FAIL; TestFoo x/y@deadbeef leaked: go test ./..."},
		// a remote attempt that fell back, and the local run then made
		{At: now.Add(-2 * time.Hour), Verb: runrecord.Fallback, Detail: "local other/repo@abcdef01 fell back (cluster unreachable): make lint"},
		{At: now.Add(-2 * time.Hour), Verb: runrecord.Start, Detail: "memcap-9-1.scope other/repo@abcdef01 slot 1 max 12G: make lint"},
		{At: now.Add(-2*time.Hour + 60*time.Second), Verb: runrecord.End, Detail: "memcap-9-1.scope exit 0 after 1m0s: make lint"},
		// an old release's record: no repository, counts under "-"
		{At: now.Add(-time.Hour), Verb: runrecord.End, Detail: "memcap-7-3.scope exit 0 after 30s: go build ./..."},
		// before the window: only the whole log's run counts it
		{At: now.Add(-48 * time.Hour), Verb: runrecord.Start, Detail: "remote:pod-b.11111111 giantswarm/beekeeper@16a89d96 ns build-workers cache cold: go vet ./..."},
		{At: now.Add(-48*time.Hour + time.Second), Verb: runrecord.End, Detail: "remote:pod-b.11111111 exit 1 after 1s: go vet ./..."},
	}
	if err := store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	runWith := func(asJSON bool, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		a := &app{store: store, out: &out, json: asJSON, now: now}
		c := a.runStatsCmd()
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatalf("stats %v: %v", args, err)
		}
		return out.String()
	}
	run := func(args ...string) string {
		t.Helper()
		return runWith(true, args...)
	}
	var got struct {
		Repositories []runStat `json:"repositories"`
		Total        runStat   `json:"total"`
	}
	if err := json.Unmarshal([]byte(run(sinceFlag, "3h")), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Repositories) != 3 {
		t.Fatalf("rows %v", got.Repositories)
	}
	bk, desk, none := got.Repositories[0], got.Repositories[1], got.Repositories[2]
	if bk.Repo != "giantswarm/beekeeper" || bk.Remote != 1 || bk.WarmCache != 1 || bk.Failed != 1 || bk.Minutes != 2 {
		t.Fatalf("beekeeper row %+v", bk)
	}
	if desk.Repo != "other/repo" || desk.Local != 1 || desk.Fallback != 1 || desk.Failed != 0 || desk.Minutes != 1 {
		t.Fatalf("desk row %+v", desk)
	}
	if none.Repo != "" || none.Local != 1 {
		t.Fatalf("old row %+v", none)
	}
	if got.Total.Local+got.Total.Remote != 3 || got.Total.Minutes != 3.5 || got.Total.Failed != 1 || got.Total.Fallback != 1 {
		t.Fatalf("total %+v", got.Total)
	}
	// the whole log: the run 48h back is counted too
	if err := json.Unmarshal([]byte(run()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Total.Remote != 2 || got.Total.WarmCache != 1 || got.Total.Failed != 2 {
		t.Fatalf("whole log %+v", got.Total)
	}
	text := runWith(false, sinceFlag, "1h")
	if !strings.Contains(strings.Join(strings.Fields(text), " "), "TOTAL 1 0 0 0 0% 0.5 0") {
		t.Fatalf("TOTAL row:\n%s", text)
	}
}

// The cache count follows the runs it measures: a warm run crossing the
// --since boundary counts where it completed, a run still going counts
// nowhere, and a cold run whose command merely names warm stays cold.
func TestRunStatsWindowAndCache(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	const since = "3h" // the window opens 09:00
	evs := []state.Event{
		// warm run crossing the boundary: start 08:55 (before), end 09:05
		{At: now.Add(-3*time.Hour - 5*time.Minute), Verb: runrecord.Start, Detail: "remote:pod-a.11111111 other/repo@01234567 ns build-workers cache warm: go test ./..."},
		{At: now.Add(-3*time.Hour + 5*time.Minute), Verb: runrecord.End, Detail: "remote:pod-a.11111111 exit 0 after 10m0s: go test ./..."},
		// warm run still going: no end, counts nowhere
		{At: now.Add(-time.Hour), Verb: runrecord.Start, Detail: "remote:pod-b.22222222 other/repo@01234567 ns build-workers cache warm: make build"},
		// cold run whose command names warm: output, not cache state
		{At: now.Add(-2 * time.Hour), Verb: runrecord.Start, Detail: "remote:pod-c.33333333 other/repo@01234567 ns build-workers cache cold: go test ./... -run warm"},
		{At: now.Add(-2*time.Hour + time.Minute), Verb: runrecord.End, Detail: "remote:pod-c.33333333 exit 0 after 1m0s: go test ./... -run warm"},
	}
	if err := store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{store: store, out: &out, json: true, now: now}
	c := a.runStatsCmd()
	c.SetArgs([]string{sinceFlag, since})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Repositories []runStat `json:"repositories"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Repositories) != 1 {
		t.Fatalf("rows %v", got.Repositories)
	}
	r := got.Repositories[0]
	if r.Repo != "other/repo" || r.Remote != 2 || r.WarmCache != 1 || r.Minutes != 11 {
		t.Fatalf("row %+v: want remote 2 with one warm (the crossing run), the cold -run warm one not warm, the unfinished one absent", r)
	}
}

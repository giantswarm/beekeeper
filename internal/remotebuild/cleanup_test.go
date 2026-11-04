package remotebuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

func TestSweepOwnershipAndRetries(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	dir := filepath.Join(state, "remote-build")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	busy := filepath.Join(t.TempDir(), "busy")
	foreign := filepath.Join(t.TempDir(), "foreign")
	names, records := map[string]string{}, map[string]string{}
	for _, root := range []string{live, missing, busy, foreign} {
		owner := host
		if root == foreign {
			owner += "-other"
		}
		name := builderName(owner, root)
		names[root] = name
		rec := builderRecord{root, owner, filepath.Join(state, "fixture-kubeconfig"), "fixture-context", "builders"}
		records[root] = filepath.Join(dir, rec.file(name))
		raw, _ := json.Marshal(rec)
		if err := os.WriteFile(records[root], raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock := flock.New(filepath.Join(dir, names[busy]+".lock"))
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	fail := true
	var calls []string
	runner := Runner{StateDir: state, Command: func(_ context.Context, _ io.Reader, _ io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if fail {
			return errors.New("offline")
		}
		return nil
	}}
	// A foreign record is ignored; it must never result in a cluster call.
	if err := runner.Sweep(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatal(calls)
	}
	if err := runner.Sweep(context.Background(), nil, false); err == nil {
		t.Fatal("expected offline failure")
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "delete job "+names[missing]) {
		t.Fatal(calls)
	}
	if _, err := os.Stat(records[missing]); err != nil {
		t.Fatal("lost retry record", err)
	}
	fail = false
	calls = nil
	_ = runner.Sweep(context.Background(), []string{live}, false)
	if len(calls) != 2 {
		t.Fatal(calls)
	}
	for _, call := range calls {
		if strings.Contains(call, names[busy]) || strings.Contains(call, names[foreign]) {
			t.Fatal(call)
		}
		if !strings.Contains(call, "--kubeconfig "+filepath.Join(state, "fixture-kubeconfig")+" --context fixture-context --namespace builders") {
			t.Fatal(call)
		}
	}
	for _, root := range []string{live, missing} {
		if _, err := os.Stat(records[root]); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(root, err)
		}
	}
}

// A worktree built through a second connection before the first builder idled
// out owns a Job in each cluster: both retire, and neither's outage costs the
// other its retirement or itself its retry.
func TestSweepRetiresEveryConnection(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	dir := filepath.Join(state, "remote-build")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	builder := builderName(host, root)
	old := Config{Kubeconfig: filepath.Join(state, "old-kubeconfig"), Context: "old-context", Namespace: "old-builders"}
	current := Config{Kubeconfig: filepath.Join(state, "new-kubeconfig"), Context: "new-context", Namespace: "new-builders"}
	offline := true
	var calls []string
	runner := Runner{StateDir: state, Command: func(_ context.Context, _ io.Reader, _ io.Writer, args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if offline && strings.Contains(call, "--context old-context") {
			return errors.New("offline")
		}
		return nil
	}}
	for _, config := range []Config{old, current, old, current} {
		runner.Config = config
		if err := runner.remember(root, host, builder); err != nil {
			t.Fatal(err)
		}
	}
	journals := func() []string {
		t.Helper()
		found, err := filepath.Glob(filepath.Join(dir, builder+".*.json"))
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	if got := journals(); len(got) != 2 {
		t.Fatalf("journals = %v, want one per connection", got)
	}
	deleted := func(c Config) string {
		return "--kubeconfig " + c.Kubeconfig + " --context " + c.Context + " --namespace " + c.Namespace +
			" --request-timeout=15s delete job " + builder + " --ignore-not-found=true --wait=false"
	}
	// The record, not the runner's configuration, says where each builder is.
	runner.Config = Config{}
	if err := runner.Sweep(context.Background(), []string{root}, false); err == nil {
		t.Fatal("expected the old connection's failure")
	}
	if want := []string{deleted(old), deleted(current)}; !slices.Equal(sorted(calls), sorted(want)) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	remaining := journals()
	if len(remaining) != 1 {
		t.Fatalf("journals = %v, want the old connection's alone", remaining)
	}
	raw, err := os.ReadFile(remaining[0]) //nolint:gosec // owned test fixture
	if err != nil {
		t.Fatal(err)
	}
	var kept builderRecord
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Context != old.Context {
		t.Fatalf("retained %+v, want the unavailable connection's record", kept)
	}
	offline = false
	calls = nil
	if err := runner.Sweep(context.Background(), []string{root}, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{deleted(old)}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if got := journals(); len(got) != 0 {
		t.Fatalf("journals = %v, want none", got)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

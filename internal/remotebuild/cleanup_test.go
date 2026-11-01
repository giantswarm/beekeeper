package remotebuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	names := map[string]string{}
	for _, root := range []string{live, missing, busy, foreign} {
		owner := host
		if root == foreign {
			owner += "-other"
		}
		name := builderName(owner, root)
		names[root] = name
		raw, _ := json.Marshal(builderRecord{root, owner, "fixture-kubeconfig", "fixture-context", "builders"})
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
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
	if _, err := os.Stat(filepath.Join(dir, names[missing]+".json")); err != nil {
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
		if !strings.Contains(call, "--kubeconfig fixture-kubeconfig --context fixture-context --namespace builders") {
			t.Fatal(call)
		}
	}
	for _, root := range []string{live, missing} {
		if _, err := os.Stat(filepath.Join(dir, names[root]+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(root, err)
		}
	}
}

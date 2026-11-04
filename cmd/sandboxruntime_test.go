package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func runtimeBroker(t *testing.T, state string) *app {
	t.Helper()
	return &app{cfg: &config.Config{StateDir: state, Sandbox: config.Sandbox{Devctl: probeHolds}}, out: &bytes.Buffer{}}
}

func startRuntimeBroker(t *testing.T, a *app) {
	t.Helper()
	a.out = io.Discard
	ctx, cancel := context.WithCancel(context.Background())
	serve, err := a.listenEgress(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve() }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("broker did not stop")
		}
	})
}

func runtimeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // a path of this test
		if err != nil {
			return err
		}
		files[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestBrokerRuntimeIsolation(t *testing.T) {
	run, stateHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("XDG_STATE_HOME", stateHome)
	live := runtimeBroker(t, filepath.Join(stateHome, "beekeeper"))
	startRuntimeBroker(t, live)
	liveDir := filepath.Join(run, "beekeeper")
	// Old token files belonging to the live broker must survive a scratch start.
	tokenDir := filepath.Join(liveDir, "github")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hosts.yml", "git-credential"} {
		if err := os.WriteFile(filepath.Join(tokenDir, name), []byte("live marker"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := runtimeFiles(t, liveDir)
	scratch := runtimeBroker(t, filepath.Join(t.TempDir(), "scratch-proof"))
	startRuntimeBroker(t, scratch)
	scratchDir := scratch.sandboxPolicy().Egress
	if !strings.HasPrefix(scratchDir, filepath.Join(liveDir, "brokers", "scratch-proof-")) {
		t.Fatalf("scratch files in %s", scratchDir)
	}
	for path, want := range before {
		got, err := os.ReadFile(path) //nolint:gosec // a path of this test
		if err != nil || string(got) != want {
			t.Errorf("live file changed: %s: %v", path, err)
		}
	}
	scratchCA, err := os.ReadFile(filepath.Join(scratchDir, sandbox.EgressCA)) //nolint:gosec // a path of this test
	if err != nil {
		t.Fatal(err)
	}
	if string(scratchCA) == before[filepath.Join(liveDir, "egress", sandbox.EgressCA)] {
		t.Fatal("brokers share a CA")
	}
	// Even on a different port, a second owner must fail before changing files.
	duplicate := runtimeBroker(t, live.cfg.StateDir)
	if _, err := duplicate.listenEgress(context.Background()); err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("duplicate owner: %v", err)
	}
	for path, want := range before {
		got, err := os.ReadFile(path) //nolint:gosec // a path of this test
		if err != nil || string(got) != want {
			t.Errorf("refused broker changed %s: %v", path, err)
		}
	}
	// Status identifies the same files that the sandbox policy exposes.
	for _, broker := range []*app{live, scratch} {
		a := runtimeBroker(t, broker.cfg.StateDir)
		command, _, err := a.sandboxCmd().Find([]string{"status"})
		if err != nil {
			t.Fatal(err)
		}
		if err := command.RunE(command, nil); err != nil {
			t.Fatal(err)
		}
		out := a.out.(*bytes.Buffer).String()
		if !strings.Contains(out, "broker state: "+a.cfg.StateDir) || !strings.Contains(out, "egress files: "+a.sandboxPolicy().Egress) {
			t.Fatalf("status: %s", out)
		}
	}
}

func TestBrokerRuntimePortFailurePreservesFiles(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	a := runtimeBroker(t, t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Sandbox.ProxyPort, _ = strconv.Atoi(port)
	dir := a.sandboxPolicy().Egress
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sandbox.EgressCA)
	if err := os.WriteFile(path, []byte("existing CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.listenEgress(context.Background()); err == nil {
		t.Fatal("occupied port accepted")
	}
	got, err := os.ReadFile(path) //nolint:gosec // a path of this test
	if err != nil || string(got) != "existing CA" {
		t.Fatalf("CA changed: %q, %v", got, err)
	}
	// Startup failure must release ownership.
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	startRuntimeBroker(t, a)
}

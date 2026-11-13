//go:build unix

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestGatedChildSignalExit(t *testing.T) {
	for _, route := range []string{litDirect, "owned", litWatchStop} {
		for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
			t.Run(route+"/"+signalName(signal), func(t *testing.T) {
				stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = stderr.Close() }()
				// The child really dies by a signal, rather than returning a chosen code.
				argv := []string{"sh", "-c", "sleep 0.05; kill -" + signalName(signal)[3:] + " $$"}
				var rc int
				if route == "direct" {
					original := os.Stderr
					os.Stderr = stderr
					defer func() { os.Stderr = original }()
					rc = runChild(argv, io.Discard)
				} else {
					var current atomic.Pointer[proc.Child]
					var stop atomic.Value
					var gone atomic.Bool
					if route == "watch stop" {
						stop.Store(signal)
						argv = []string{sleepComm, "30"}
					}
					rc = runSpec("", gate.ChildSpec{Argv: argv, Env: os.Environ()}, stderr, stderr, &current, &stop, &gone)
				}
				if want := 128 + int(signal); rc != want {
					t.Fatalf("exit %d, want %d", rc, want)
				}
				raw, err := os.ReadFile(stderr.Name())
				if err != nil {
					t.Fatal(err)
				}
				prefix := gate.GatePrefix + "ended by " + signalName(signal) + " after "
				if !strings.HasPrefix(string(raw), prefix) {
					t.Fatalf("missing signal diagnostic: %q", raw)
				}
				elapsed, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(string(raw), prefix)))
				if err != nil || elapsed < 0 {
					t.Fatalf("invalid elapsed time in %q: %v", raw, err)
				}
				if route != "watch stop" && elapsed < 50*time.Millisecond {
					t.Fatalf("elapsed time excludes child runtime: %s", elapsed)
				}
			})
		}
	}
}

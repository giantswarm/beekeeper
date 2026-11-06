package guard

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestProcessRun(t *testing.T) {
	run := []string{selfCmd, "run", "--max=2G", "--"}
	gate := []string{"beekeeper", litGate, "--"}
	for _, tc := range []struct{ script, want string }{
		{"echo waiting; tail -f output", "echo waiting; tail -f output"},
		{"devctl pr wait o/r 1 && go test ./...", "beekeeper gate -- devctl pr wait o/r 1 && beekeeper run --max=2G -- go test ./..."},
		{"go build ./...; tail -f output", "beekeeper run --max=2G -- go build ./...; tail -f output"},
		{"devctl pr wait o/r 1 >output & echo waiting; tail output", "beekeeper gate -- devctl pr wait o/r 1 >output & echo waiting; tail output"},
		{"env FOO=bar devctl pr wait o/r 1", "env FOO=bar beekeeper gate -- devctl pr wait o/r 1"},
		{"FOO=bar go test ./... && echo done", "FOO=bar beekeeper run --max=2G -- go test ./... && echo done"},
		{`echo "$(go test ./...)"`, `echo "$(beekeeper run --max=2G -- go test ./...)"`},
		{`echo '; go test ./...'`, `echo '; go test ./...'`},
		{"false && go test ./...", "false && beekeeper run --max=2G -- go test ./..."},
	} {
		t.Run(tc.script, func(t *testing.T) {
			got, reason := ProcessRun([]string{"sh", "-c", tc.script}, run, gate)
			if len(got) != 3 || got[2] != tc.want || reason == "" {
				t.Fatalf("got %q (%s), want %q", got, reason, tc.want)
			}
		})
	}
	for _, script := range []string{"custom-build; tail output", "sh -c 'go test ./...'", "f() { go build; }; f", "echo $(custom-build)", "echo '"} {
		if got, _ := ProcessRun([]string{"sh", "-c", script}, run, gate); got != nil {
			t.Errorf("unsupported shell %q planned as %q", script, got)
		}
	}
	for _, verb := range []string{"pr wait", "pr merge", "release wait", "release promote", "rollout wait"} {
		argv := append([]string{"devctl"}, strings.Fields(verb)...)
		argv = append(argv, "o/r", "1")
		got, reason := ProcessRun(argv, run, gate)
		if strings.Join(got, " ") != "beekeeper gate -- "+strings.Join(argv, " ") || !strings.Contains(reason, "no build slot") {
			t.Fatalf("got %q (%s)", got, reason)
		}
	}
}

// The wrapper records slot ownership around its child. A wait before the
// build and the shell's remaining work must both observe a free slot.
func TestProcessRunSlotFollowsChild(t *testing.T) {
	dir := t.TempDir()
	// The executable helper below uses Run and the real flock-based slot.
	wrapper := os.Args[0]
	// A fake compiler verifies it owns the slot, then returns a chosen exit.
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\ntest -e \"$STATE/1.holder\" || exit 92\nexit \"${BUILD_EXIT:-0}\"\n"), 0700); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	t.Setenv("STATE", dir)
	t.Setenv("PROCESS_RUN_HELPER", "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, exit := range []string{"0", "7"} {
		t.Setenv("BUILD_EXIT", exit)
		script := `devctl pr wait o/r 1 && go test ./...; rc=$?; devctl pr wait o/r 1 || exit 93; echo after; exit "$rc"`
		argv, _ := ProcessRun([]string{"sh", "-c", script}, []string{wrapper, "-test.run=^TestProcessRunHelper$", "--", "run"}, []string{wrapper, "-test.run=^TestProcessRunHelper$", "--", "gate"})
		if argv == nil {
			t.Fatal("script was not split")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // a test fixture
		cancel()
		rc := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				rc = e.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		want := 0
		if exit == "7" {
			want = 7
		}
		if rc != want || string(output) != "after\n" {
			t.Fatalf("exit %d output %q, want %d", rc, output, want)
		}
	}
	events, err := os.ReadFile(filepath.Join(dir, "events")) //nolint:gosec // a test fixture
	if err != nil || string(events) != "gate\nstart\nend\ngate\ngate\nstart\nend\ngate\n" {
		t.Fatalf("events %q: %v", events, err)
	}
}

// TestProcessRunHelper is a subprocess so Run can exercise real slot locks
// without sharing platform globals with the parent test.
func TestProcessRunHelper(t *testing.T) {
	if os.Getenv("PROCESS_RUN_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(90)
	}
	dir := os.Getenv("STATE")
	events, err := os.OpenFile(filepath.Join(dir, "events"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600) //nolint:gosec // a test fixture
	if err != nil {
		os.Exit(94)
	}
	if args[1] == "gate" {
		lock := flock.New(filepath.Join(dir, "1.lock"))
		ok, err := lock.TryLock()
		if !ok || err != nil {
			os.Exit(91)
		}
		_ = lock.Unlock()
		_, _ = events.WriteString("gate\n")
		os.Exit(0)
	}
	plat.Capper = &fakeCapper{}
	_, _ = events.WriteString("start\n")
	rc := Run(Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 1, Stderr: io.Discard}, args[2:])
	_, _ = events.WriteString("end\n")
	os.Exit(rc)
}

func TestProcessRunOutputBesideHeldSlot(t *testing.T) {
	dir := t.TempDir()
	holdSlot(t, dir, 1, "other")
	outputFile := filepath.Join(dir, "output")
	if err := os.WriteFile(outputFile, []byte("wait output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := "echo waiting; tail " + ShellQuote(outputFile)
	argv, _ := ProcessRun([]string{"sh", "-c", script}, []string{"must-not-run"}, []string{"must-not-gate"})
	if argv == nil {
		t.Fatal("output-only shell was not classified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // a test fixture
	if err != nil || string(output) != "waiting\nwait output\n" {
		t.Fatalf("output %q: %v", output, err)
	}
}

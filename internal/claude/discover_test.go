package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// The process trees in testdata/proc were captured on a lab machine
// (stat, cmdline, the CLAUDE* environment keys, values only for the
// identity keys), with neutral paths and fake ids:
//
//   - children: a desktop session (CLI 2927993 under the desktop app), a
//     `claude -p` its tool shell runs (3060617) and a `claude -p` it started
//     and left to the user manager (3060623), both inheriting its ids.
//   - background: a `claude --bg` worker started through systemd-run: the
//     daemon (3055535), the worker's terminal host (3055586) and CLI
//     (3055597), a spare terminal host (3055581) and spare CLI (3055600).
const (
	desktopPID  = 2927993
	desktopHost = "local_aaaaaaaa-0000-4000-8000-000000000002"
	desktopID   = "aaaaaaaa-0000-4000-8000-000000000001"
	desktopName = "test: desktop session"
	inTreePID   = 3060617
	detachedPID = 3060623
	detachedID  = "cccccccc-0000-4000-8000-000000000003"
	workerPID   = 3055597
	workerID    = "bbbbbbbb-0000-4000-8000-000000000004"
)

func discoverAt(t *testing.T, root string) map[int]*Session {
	t.Helper()
	tab, err := proc.ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Claude.ProjectsDir = t.TempDir()
	cfg.Claude.DesktopDir = filepath.Join("testdata", "desktop")
	out := map[int]*Session{}
	for _, s := range Discover(cfg, tab, time.Now()) {
		out[s.PID] = s
	}
	return out
}

// copyTree copies a captured process tree into a temporary directory,
// without the processes skip names.
func copyTree(t *testing.T, src string, skip ...int) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	for _, pid := range skip {
		if err := os.RemoveAll(filepath.Join(dst, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestDiscoverKeepsTheDesktopSessionWhenItStartsChildren(t *testing.T) {
	root := filepath.Join("testdata", "proc", "children")
	got := discoverAt(t, root)
	if len(got) != 2 {
		t.Fatalf("sessions = %v, want the desktop session and its detached child", got)
	}
	d := got[desktopPID]
	if d == nil || d.HostID != desktopHost || d.ID != desktopID || d.Name != desktopName || d.Parent != "" {
		t.Fatalf("desktop session = %+v", d)
	}
	if !slices.ContainsFunc(d.Commands, func(c Command) bool { return c.PID == inTreePID }) {
		t.Errorf("the claude -p its tool shell runs is not its command: %+v", d.Commands)
	}
	c := got[detachedPID]
	if c == nil || c.ID != detachedID || c.HostID != "" || c.Parent != desktopID || c.Name != "test: beekeeper#27 detached child" {
		t.Fatalf("detached child = %+v", c)
	}
	// watch reports a restart when a key's PID changes: the children leave
	// the desktop session's key and PID as they were without them.
	alone := discoverAt(t, copyTree(t, root, inTreePID, detachedPID))
	if a := alone[desktopPID]; a == nil || a.Key() != d.Key() || len(alone) != 1 {
		t.Errorf("without the children: %v", alone)
	}
}

func TestDiscoverListsAChildStartedUnderItsOwnID(t *testing.T) {
	root := copyTree(t, filepath.Join("testdata", "proc", "children"), detachedPID)
	args := "/opt/claude-code/bin/claude\x00-p\x00--session-id\x00" + detachedID + "\x00-n\x00test: in-tree child\x00ok\x00"
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(inTreePID), "cmdline"), []byte(args), 0o600); err != nil {
		t.Fatal(err)
	}
	got := discoverAt(t, root)
	c := got[inTreePID]
	if c == nil || c.ID != detachedID || c.Parent != desktopID || c.Name != "test: in-tree child" {
		t.Fatalf("in-tree child = %+v", c)
	}
	if d := got[desktopPID]; d == nil || d.Key() != desktopHost {
		t.Fatalf("desktop session = %+v", d)
	}
}

func TestDiscoverFindsTheBackgroundWorkerNotItsDaemon(t *testing.T) {
	got := discoverAt(t, filepath.Join("testdata", "proc", "background"))
	w := got[workerPID]
	if len(got) != 1 || w == nil {
		t.Fatalf("sessions = %v, want the worker alone", got)
	}
	if w.ID != workerID || w.Name != "test: beekeeper#27 bg worker" || w.HostID != "" || w.Parent != "" || w.Key() != workerID {
		t.Errorf("worker = %+v", w)
	}
}

func TestIsCLI(t *testing.T) {
	// The arguments are separated by "|".
	for args, want := range map[string]bool{
		"claude|--output-format|stream-json":   true,
		"claude|-p|ok":                         true,
		"claude":                               true,
		"claude|stop the daemon":               true, // a prompt, not a subcommand
		"claude|--session-id|x|-n|test":        true,
		"claude|daemon|run|--origin|transient": false,
		"claude|bg-pty-host|--bg-pty-host|s|--|claude|--session-id|x": false,
		"claude bg-pty-host|--bg-pty-host|s|--|claude|--session-id|x": false,
		"claude bg-spare|--bg-spare|s":                                false,
		"claude|--bg|-n|worker|prompt":                                false,
		"claude|stop|bbbbbbbb":                                        false,
		"claude|mcp|serve":                                            false,
	} {
		if got := isCLI(&proc.Process{Comm: "claude", Args: strings.Split(args, "|")}); got != want {
			t.Errorf("isCLI(%q) = %v", args, got)
		}
	}
	if isCLI(&proc.Process{Comm: "claude-desktop", Args: []string{"claude-desktop"}}) {
		t.Error("the desktop app is a CLI")
	}
}

func TestOwnID(t *testing.T) {
	for args, want := range map[string]string{
		"claude --session-id s1 -p ok":       "s1",
		"claude --resume s2":                 "s2",
		"claude --resume=s3":                 "s3",
		"claude -r s4":                       "s4",
		"claude -r --model haiku":            "",
		"claude -p ok":                       "",
		"claude --session-id s5 --resume s6": "s5",
	} {
		if got := ownID(strings.Fields(args)); got != want {
			t.Errorf("ownID(%q) = %q, want %q", args, got, want)
		}
	}
}

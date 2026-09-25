package claude

import (
	"fmt"
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
//   - spare: two `claude --bg` workers started one after the other through
//     the same daemon (349150): worker a in a fresh CLI (349203), worker b
//     in the spare the daemon handed it (349202, `claude bg-spare`), and
//     the next spare (350263), which no session has claimed.
//   - woken: worker a stopped and woken with `claude --bg --resume <id>`
//     through a new daemon (399538): its CLI (399599) resumes the
//     transcript's path; the daemon's spare (399593) is unclaimed.
//
// testdata/sessions holds the records the CLIs of spare and woken kept in
// ~/.claude/sessions, reduced to the keys beekeeper reads and a few more.
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
	freshPID    = 349203
	claimedPID  = 349202
	wokenPID    = 399599
	workerA     = "dddddddd-0000-4000-8000-000000000005"
	workerB     = "eeeeeeee-0000-4000-8000-000000000006"
	workerAName = "test: beekeeper#35 worker a"
	workerBName = "test: beekeeper#35 worker b"
)

// discoverAt discovers the sessions of a captured process tree with the CLI
// records in sessions ("" for none).
func discoverAt(t *testing.T, root, sessions string) map[int]*Session {
	t.Helper()
	tab, err := proc.ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Claude.ProjectsDir = t.TempDir()
	cfg.Claude.SessionsDir = sessions
	if sessions == "" {
		cfg.Claude.SessionsDir = t.TempDir()
	}
	cfg.Claude.DesktopDir = filepath.Join("testdata", "desktop")
	out := map[int]*Session{}
	for _, s := range Discover(cfg, tab, time.Now()) {
		out[s.PID] = s
	}
	return out
}

// copyTree copies a captured process tree or record directory into a
// temporary directory, without the processes skip names.
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
	got := discoverAt(t, root, "")
	if len(got) != 2 {
		t.Fatalf("sessions = %v, want the desktop session and its detached child", got)
	}
	d := got[desktopPID]
	if d == nil || d.HostID != desktopHost || d.ID != desktopID || d.Name != desktopName || d.Parent != "" || d.Aside() != "test" || d.Background {
		t.Fatalf("desktop session = %+v", d)
	}
	if !slices.ContainsFunc(d.Commands, func(c Command) bool { return c.PID == inTreePID }) {
		t.Errorf("the claude -p its tool shell runs is not its command: %+v", d.Commands)
	}
	c := got[detachedPID]
	if c == nil || c.ID != detachedID || c.HostID != "" || c.Parent != desktopID || c.Name != "test: beekeeper#27 detached child" || c.Background {
		t.Fatalf("detached child = %+v", c)
	}
	// watch reports a restart when a key's PID changes: the children leave
	// the desktop session's key and PID as they were without them.
	alone := discoverAt(t, copyTree(t, root, inTreePID, detachedPID), "")
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
	got := discoverAt(t, root, "")
	c := got[inTreePID]
	if c == nil || c.ID != detachedID || c.Parent != desktopID || c.Name != "test: in-tree child" {
		t.Fatalf("in-tree child = %+v", c)
	}
	if d := got[desktopPID]; d == nil || d.Key() != desktopHost {
		t.Fatalf("desktop session = %+v", d)
	}
}

func TestDiscoverFindsTheBackgroundWorkerNotItsDaemon(t *testing.T) {
	got := discoverAt(t, filepath.Join("testdata", "proc", "background"), "")
	w := got[workerPID]
	if len(got) != 1 || w == nil {
		t.Fatalf("sessions = %v, want the worker alone", got)
	}
	if w.ID != workerID || w.Name != "test: beekeeper#27 bg worker" || w.HostID != "" || w.Parent != "" || w.Key() != workerID || !w.Background {
		t.Errorf("worker = %+v", w)
	}
}

func TestDiscoverFindsAWorkerInAClaimedSpare(t *testing.T) {
	root := filepath.Join("testdata", "proc", "spare")
	records := filepath.Join("testdata", "sessions", "spare")
	got := discoverAt(t, root, records)
	if len(got) != 2 {
		t.Fatalf("sessions = %v, want both workers, neither the daemon nor the unclaimed spare", got)
	}
	for pid, want := range map[int][2]string{freshPID: {workerA, workerAName}, claimedPID: {workerB, workerBName}} {
		if w := got[pid]; w == nil || w.ID != want[0] || w.Name != want[1] || w.Key() != want[0] || w.HostID != "" || w.Parent != "" || !w.Background {
			t.Errorf("worker %d = %+v", pid, w)
		}
	}
	// A record a dead process left behind does not name a new process
	// under its PID.
	stale := copyTree(t, records)
	rec := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"name":"test: dead worker","procStart":"5390000","kind":"bg"}`, claimedPID, workerB)
	if err := os.WriteFile(filepath.Join(stale, strconv.Itoa(claimedPID)+".json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := discoverAt(t, root, stale); len(got) != 1 || got[freshPID] == nil {
		t.Errorf("with a stale record: %v, want worker a alone", got)
	}
}

func TestDiscoverFindsAWokenWorkerUnderItsID(t *testing.T) {
	root := filepath.Join("testdata", "proc", "woken")
	// Its record names it, and so does its transcript's path before the
	// CLI has written the record.
	for from, records := range map[string]string{"record": filepath.Join("testdata", "sessions", "woken"), "arguments": ""} {
		got := discoverAt(t, root, records)
		w := got[wokenPID]
		if len(got) != 1 || w == nil || w.ID != workerA || w.Name != workerAName || w.Key() != workerA || !w.Background {
			t.Errorf("from its %s: sessions = %v, worker = %+v", from, got, w)
		}
	}
}

func TestDiscoverTakesTheSessionFromTheRecord(t *testing.T) {
	// A resumed CLI goes on under a new session id; its arguments keep
	// the one it resumed.
	root := filepath.Join("testdata", "proc", "children")
	tab, err := proc.ReadAt(root)
	if err != nil {
		t.Fatal(err)
	}
	records := t.TempDir()
	rec := fmt.Sprintf(`{"pid":%d,"sessionId":"ffffffff-0000-4000-8000-000000000007","name":"test: renamed","procStart":"%d","kind":"interactive"}`, desktopPID, tab.ByPID[desktopPID].StartTicks)
	if err := os.WriteFile(filepath.Join(records, strconv.Itoa(desktopPID)+".json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	d := discoverAt(t, root, records)[desktopPID]
	// The desktop title still names it, and its host id keys it.
	if d == nil || d.ID != "ffffffff-0000-4000-8000-000000000007" || d.Name != desktopName || d.Key() != desktopHost {
		t.Fatalf("desktop session = %+v", d)
	}
}

// claudeProcess is a claude process with the arguments args, separated by "|".
func claudeProcess(args string) *proc.Process {
	return &proc.Process{Comm: "claude", Args: strings.Split(args, "|")}
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
		"claude|--bg-spare|s":                                         false,
		"claude|--bg|-n|worker|prompt":                                false,
		"claude|stop|bbbbbbbb":                                        false,
		"claude|mcp|serve":                                            false,
	} {
		if got := isCLI(claudeProcess(args)); got != want {
			t.Errorf("isCLI(%q) = %v", args, got)
		}
	}
	if isCLI(&proc.Process{Comm: "claude-desktop", Args: []string{"claude-desktop"}}) {
		t.Error("the desktop app is a CLI")
	}
}

func TestIsSpare(t *testing.T) {
	for args, want := range map[string]bool{
		"claude bg-spare|--bg-spare|s":                              true,
		"claude|--bg-spare|s":                                       true,
		"claude bg-pty-host|--bg-pty-host|s|--|claude|--bg-spare|s": false,
		"claude|--session-id|x":                                     false,
	} {
		if got := isSpare(claudeProcess(args)); got != want {
			t.Errorf("isSpare(%q) = %v", args, got)
		}
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
		"claude --resume /home/user/.claude/projects/-home-user-work/s7.jsonl -n w": "s7",
	} {
		if got := ownID(strings.Fields(args)); got != want {
			t.Errorf("ownID(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestRecordNameIsTheBackgroundWorkersTitle(t *testing.T) {
	tab, err := proc.ReadAt(filepath.Join("testdata", "proc", "spare"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Claude.SessionsDir = filepath.Join("testdata", "sessions", "spare")
	for id, want := range map[string]string{workerA: workerAName, workerB: workerBName, desktopID: ""} {
		if got := RecordName(cfg, tab, id); got != want {
			t.Errorf("RecordName(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestStoppedWaitingSkipsArchivedAndTestSessions(t *testing.T) {
	cfg := &config.Config{}
	cfg.Claude.DesktopDir = filepath.Join("testdata", "desktop-waiting")
	titles := func(rs []*Record) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Title)
		}
		return out
	}
	// The archived #60 test (no title), the "test: …" run, the completed
	// session and the stale summary stay out.
	want := []string{"Land the follow-ups", "Guide Timo through his decisions"}
	if got := titles(StoppedWaiting(cfg, nil)); !slices.Equal(got, want) {
		t.Fatalf("stopped and waiting = %q, want %q", got, want)
	}
	running := []*Session{{HostID: "local_bbbbbbbb-0000-4000-8000-000000000013"}}
	if got := titles(StoppedWaiting(cfg, running)); !slices.Equal(got, want[1:]) {
		t.Fatalf("with the first one running = %q, want %q", got, want[1:])
	}
	if got := StoppedRecords(cfg, nil, time.Time{}); len(got) != 5 || slices.ContainsFunc(got, func(r *Record) bool { return r.IsArchived }) {
		t.Fatalf("stopped records = %q, want every unarchived one", titles(got))
	}
}

func TestAsideMarksArchivedAndTestSessions(t *testing.T) {
	for _, c := range []struct {
		s    Session
		want string
	}{
		{Session{Name: "c-32", Archived: true}, "archived"},
		{Session{Name: "test: beekeeper#60 permission hook"}, "test"},
		{Session{Name: "Test the rollout"}, ""},
		{Session{Name: "Land the follow-ups"}, ""},
	} {
		if got := c.s.Aside(); got != c.want {
			t.Errorf("%q archived=%v: aside %q, want %q", c.s.Name, c.s.Archived, got, c.want)
		}
	}
}

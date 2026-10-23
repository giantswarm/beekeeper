package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestRecordStart(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	live := func(p state.Party) bool { return p.Session == "r" }
	start := func(id, name string, at time.Time) state.Start {
		return state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: name}, Mode: state.ModeBypass, Dir: "/w", At: at}
	}
	old := start("old", "test: gone", now.Add(-startsKept-time.Hour))
	kept := start("kept", "test: kept", now.Add(-startsKept+time.Hour))

	// A start is recorded with its mode, the old ones forgotten, and the
	// agent registered busy with the brief.
	st := &state.State{Starts: []state.Start{old, kept}}
	reg, err := recordStart(st, start("n", workerName, now), "Brief: count lines", "", live)
	if err != nil || reg.task != "Brief: count lines" {
		t.Fatalf("registration = %+v, err = %v", reg, err)
	}
	if ids := []string{st.Starts[0].Session, st.Starts[len(st.Starts)-1].Session}; len(st.Starts) != 2 || !slices.Equal(ids, []string{"kept", "n"}) {
		t.Errorf("starts = %+v", st.Starts)
	}
	if s, ok := st.BypassStart("n"); !ok || s.Name != workerName {
		t.Errorf("BypassStart(n) = %+v, %v", s, ok)
	}
	if ag := st.Agents[0]; len(st.Agents) != 1 || ag.Session != "n" || ag.HostSession != "local_n" || ag.Task != "Brief: count lines" || !ag.AssignedAt.Equal(now) {
		t.Errorf("roster = %+v", st.Agents)
	}

	// A start under the name of a stopped session takes over its open
	// task; a running session's name is refused and nothing is recorded.
	st = &state.State{Agents: []state.Agent{
		{Party: state.Party{Session: "a", Name: workerName}, Task: staleTask, AssignedAt: now.Add(-time.Hour)},
		{Party: state.Party{Session: "r", Name: "test: busy"}, Task: "x"},
	}}
	if reg, err := recordStart(st, start("m", workerName, now), "Brief: other", "", live); err != nil || reg.task != staleTask || st.Agents[1].Task != staleTask {
		t.Errorf("take over: %+v, %v, roster %+v", reg, err, st.Agents)
	}
	if _, err := recordStart(st, start("z", "test: busy", now), "Brief", "", live); err == nil || len(st.Starts) != 1 {
		t.Errorf("a running session's name: err = %v, starts = %+v", err, st.Starts)
	}
}

func TestAgentsStartServesTheBriefsIssueFromTheStart(t *testing.T) {
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	const served = "giantswarm/beekeeper#1276"
	by := state.Party{Session: "sv", Name: claimSupervisor}
	brief := "# FIX beekeeper 1276 (opus, builds)\n\nIssue: https://github.com/giantswarm/beekeeper/issues/1276 — read it only.\nFollow-up to https://github.com/giantswarm/beekeeper/issues/1259."
	if got := briefIssue(brief); got != served {
		t.Fatalf("briefIssue = %q", got)
	}
	if got := briefIssue("# Sweep\nno link here, beekeeper#12"); got != "" {
		t.Errorf("briefIssue of a brief without a URL = %q", got)
	}

	worker := state.Party{Session: "w", Name: "FIX beekeeper 1276"}
	s := state.Start{Party: worker, Mode: state.ModeBypass, Dir: "/w", By: by, At: now}
	live := func(p state.Party) bool { return p.Is(worker) }
	st := &state.State{}
	if _, err := recordStart(st, s, briefTask(brief), briefIssue(brief), live); err != nil {
		t.Fatal(err)
	}
	if len(st.Records) != 1 || !st.Records[0].Session.Is(worker) || st.Records[0].Issue != served || !st.Records[0].By.Is(by) {
		t.Fatalf("records = %+v", st.Records)
	}
	// board next sees it served with no sessions serve call.
	cands := []board.Candidate{{Item: board.Item{Ref: served}}, {Item: board.Item{Ref: "giantswarm/beekeeper#1277"}}}
	res := nextFree(st, cands, pickScope{me: by, alive: live, listed: now.Add(-time.Second)})
	if res.Pick == nil || res.Pick.Ref != cands[1].Ref || len(res.Skipped) != 1 || res.Skipped[0].Skip != `served by "FIX beekeeper 1276"` {
		t.Errorf("pick %v, skipped %+v", res.Pick, res.Skipped)
	}

	// A start taking over a stopped entry's open task keeps that task and
	// records nothing for the brief.
	st = &state.State{Agents: []state.Agent{{Party: state.Party{Session: "stopped", Name: worker.Name}, Task: staleTask}}}
	if _, err := recordStart(st, s, briefTask(brief), briefIssue(brief), live); err != nil || len(st.Records) != 0 {
		t.Errorf("take over: records %+v, %v", st.Records, err)
	}
}

func TestBypassStartNeedsTheRecordedMode(t *testing.T) {
	st := &state.State{Starts: []state.Start{{Party: state.Party{Session: "d"}, Mode: "default"}, {Party: state.Party{Session: "b"}, Mode: state.ModeBypass}}}
	for id, want := range map[string]bool{"b": true, "d": false, "x": false, "": false} {
		if _, ok := st.BypassStart(id); ok != want {
			t.Errorf("BypassStart(%q) = %v", id, ok)
		}
	}
}

// A worker whose CLI the desktop restarted runs under a new session id and
// its start's host id: BypassStartOf finds it by either, BypassStart by the
// session id only.
func TestBypassStartOfMatchesTheHostSession(t *testing.T) {
	const firstCLI, restartedCLI, firstHost, askingHost = "cli-1", "cli-2", "local_first", "local_asking"
	st := &state.State{Starts: []state.Start{
		{Party: state.Party{Session: firstCLI, HostSession: firstHost, Name: "W"}, Mode: state.ModeBypass},
		{Party: state.Party{Session: "asking", HostSession: askingHost}, Mode: keyDefault},
	}}
	for name, tc := range map[string]struct {
		p    state.Party
		want bool
	}{
		"its first CLI":                  {state.Party{Session: firstCLI}, true},
		"a restarted CLI under its host": {state.Party{Session: restartedCLI, HostSession: firstHost}, true},
		"an unknown session":             {state.Party{Session: "stranger", HostSession: "local_stranger"}, false},
		"a start not in bypass":          {state.Party{Session: "asking-restarted", HostSession: askingHost}, false},
		"a name alone":                   {state.Party{Name: "W"}, false},
		"nothing":                        {state.Party{}, false},
	} {
		if _, ok := st.BypassStartOf(tc.p); ok != tc.want {
			t.Errorf("%s: BypassStartOf(%+v) = %v, want %v", name, tc.p, ok, tc.want)
		}
	}
	if _, ok := st.BypassStart(restartedCLI); ok {
		t.Error("BypassStart matched a session id no start recorded")
	}
}

// Every headless turn of a start, the first (headlessStartArgv) and a wake
// (wakeArgv), runs in bypass with the CLI's own Chrome connection and no
// narrowed tool set: browse's narrowing is for the desktop turns.
func TestHeadlessTurnFlags(t *testing.T) {
	turns := map[string][]string{
		"first turn": headlessStartArgv("claude", "id", "n", "", "b"),
		"wake turn":  wakeArgv("claude", wakeTarget{id: "id", mode: state.ModeBypass}, "m"),
	}
	for turn, argv := range turns {
		flags := argv[:slices.Index(argv, "--")]
		if i := slices.Index(flags, permissionModeFlag); i < 0 || flags[i+1] != state.ModeBypass {
			t.Errorf("%s argv = %q, want %s %s", turn, argv, permissionModeFlag, state.ModeBypass)
		}
		if !slices.Contains(flags, chromeFlag) {
			t.Errorf("%s argv = %q, want %s", turn, argv, chromeFlag)
		}
		for _, narrowing := range []string{toolsFlag, allowedToolsFlag, strictMCPConfigFlag} {
			if slices.Contains(flags, narrowing) {
				t.Errorf("%s argv = %q, want no %s", turn, argv, narrowing)
			}
		}
	}
}

func TestAgentArgvAndBriefTask(t *testing.T) {
	got := agentArgv("/usr/bin/claude", "id-1", "test: w", "haiku", "-starts with a dash")
	want := []string{"/usr/bin/claude", "-p", "--session-id", "id-1", permissionModeFlag, "bypassPermissions", "-n", "test: w", "--model", "haiku", "--", "-starts with a dash"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q", got)
	}
	if got := agentArgv("claude", "id", "n", "", "b"); slices.Contains(got, "--model") {
		t.Errorf("argv without a model = %q", got)
	}
	if got := briefTask("# Brief: bk-permhook\n\nbody"); got != "Brief: bk-permhook" {
		t.Errorf("briefTask = %q", got)
	}
}

func TestAwaitFocus(t *testing.T) {
	log := filepath.Join(t.TempDir(), "main.log")
	focus := func(id string) {
		f, err := os.OpenFile(filepath.Clean(log), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck // test
		if _, err := fmt.Fprintf(f, "2026-09-25 13:14:04 [info] [CCD] LocalSessions.setFocusedSession: sessionId=%s\n", id); err != nil {
			t.Fatal(err)
		}
	}
	focus("local_prev")
	if awaitFocus(t.Context(), log, "local_new", 300*time.Millisecond) {
		t.Error("awaitFocus while the desktop shows another session: true")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		focus("null")
		focus("local_new")
	}()
	if !awaitFocus(t.Context(), log, "local_new", 5*time.Second) {
		t.Error("awaitFocus after the import showed the session: false")
	}
}

func TestAwaitReply(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-home-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id.jsonl")
	write := func(lines string) {
		if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	running := func() bool { return false }
	write(`{"type":"user","message":{"role":"user","content":"the brief"}}` + "\n")
	if err := awaitReply(t.Context(), projects, "id", running, 0, 700*time.Millisecond); err == nil {
		t.Error("awaitReply before the first reply: no error")
	}
	if err := awaitReply(t.Context(), projects, "id", func() bool { return true }, 0, 5*time.Second); err == nil || !strings.Contains(err.Error(), "ended before its first reply") {
		t.Errorf("awaitReply after the unit ended without a reply = %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		write(`{"type":"user","message":{"role":"user","content":"the brief"}}
{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}
`)
	}()
	start := time.Now()
	if err := awaitReply(t.Context(), projects, "id", running, 500*time.Millisecond, 5*time.Second); err != nil {
		t.Errorf("awaitReply after the first reply = %v", err)
	}
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Errorf("awaitReply returned %s after the start, before the transcript was quiet", took)
	}
}

func TestResumesNamesTheDesktopsCLIOnly(t *testing.T) {
	const id = "25a32485-e71e-462b-8d77-ddbb65220943"
	for args, want := range map[string]bool{
		"claude --output-format stream-json --resume=" + id + " --model default": true,
		"claude --resume " + id: true,
		"claude -p --session-id " + id + " --permission-mode bypassPermissions": false,
		"claude --resume=" + id[:8]:           false,
		"claude --resume /p/" + id + ".jsonl": false,
	} {
		if got := resumes(strings.Fields(args), id); got != want {
			t.Errorf("resumes(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestReopensOnlyAStartTheRosterHolds(t *testing.T) {
	st := &state.State{
		Starts: []state.Start{{Party: state.Party{Session: "on"}}, {Party: state.Party{Session: "handed"}}},
		Agents: []state.Agent{{Party: state.Party{Session: "on", Name: "test: on"}}, {Party: state.Party{Session: "desk", HostSession: "local_d", Name: "test: desk"}}},
	}
	for id, want := range map[string]string{"on": "test: on", "handed": "", "desk": "", "none": "", "local_d": "test: desk", "local_x": ""} {
		if got, ok := reopens(st, id); got != want || ok != (want != "") {
			t.Errorf("reopens(%q) = %q, %v, want %q", id, got, ok, want)
		}
	}
}

// A first turn that grew the transcript past the desktop import's window
// leaves the title -n wrote at its start out of reach: titleTranscript puts
// the name in the window's last custom-title line, and the reply's model
// stays readable.
func TestTitleTranscriptReachesTheImportWindow(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-home-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := titleTranscript(projects, "id", "Guide run 2"); err == nil {
		t.Error("titling a session without a transcript: no error")
	}
	path := filepath.Join(dir, "id.jsonl")
	turn := `{"type":"custom-title","customTitle":"Guide run 2","sessionId":"id"}` + "\n" +
		strings.Repeat(`{"type":"attachment","content":"`+strings.Repeat("x", 1000)+`"}`+"\n", importTitleWindow/1000+10) +
		`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001"}}` + "\n"
	if err := os.WriteFile(path, []byte(turn), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := titleTranscript(projects, "id", `Guide run 2: Timo's "open" decisions`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own transcript
	if err != nil {
		t.Fatal(err)
	}
	window := raw[len(raw)-importTitleWindow:]
	lines := strings.Split(strings.TrimSpace(string(window)), "\n")
	var title struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &title); err != nil || title.Type != "custom-title" ||
		title.CustomTitle != `Guide run 2: Timo's "open" decisions` || title.SessionID != "id" {
		t.Errorf("the import window's last line = %q (%v)", lines[len(lines)-1], err)
	}
	if model, err := claude.Model(path); err != nil || model != "claude-haiku-4-5-20251001" {
		t.Errorf("the titled transcript's model = %q, %v", model, err)
	}
}

// The import runs with the first turn's unit frozen and the unit is thawed
// after it, also when the import failed; a unit that ended is not frozen.
func TestWhileFrozenThawsAfterTheImport(t *testing.T) {
	needsPlatform(t)
	unit := "beekeeper-test-freeze-" + uuid.NewString()[:8]
	if out, err := userCommand("systemd-run", "--user", "--collect", "--quiet", "--unit="+unit, "--", "sleep", "60"); err != nil {
		t.Skipf("no systemd user manager: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = userCommand("systemctl", "--user", "stop", unit) })
	ctx := context.Background()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		out, _ := userCommand("systemctl", "--user", "show", "-p", "FreezerState", "--value", unit)
		return strings.TrimSpace(string(out))
	}
	boom := errors.New("import failed")
	for _, want := range []error{nil, boom} {
		var during string
		err := whileFrozen(ctx, store, []string{unit}, func() error { during = state(); return want })
		if !errors.Is(err, want) || (want == nil && err != nil) {
			t.Errorf("whileFrozen returned %v, want %v", err, want)
		}
		if during != "frozen" {
			t.Errorf("the unit during the import is %q, want frozen", during)
		}
		if after := state(); after != "running" {
			t.Errorf("the unit after the import (%v) is %q, want running", want, after)
		}
	}
	_, _ = userCommand("systemctl", "--user", "stop", unit)
	called := false
	if err := whileFrozen(ctx, store, []string{unit}, func() error { called = true; return nil }); err != nil || !called {
		t.Errorf("an ended unit: whileFrozen = %v, fn called %v", err, called)
	}
}

// userCommand runs one systemd command of the test on its own unit.
func userCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput() //nolint:gosec // the test's own unit
}

func TestTitleLine(t *testing.T) {
	for _, c := range []struct{ title, want string }{
		{"Board pull 3", `the desktop titled it "Board pull 3"`},
		{"", `the desktop recorded no title: the sidebar shows it untitled, not as "Board pull 3", until the session retitles itself after its first turn`},
		{"General coding session", `the desktop titled it "General coding session", not "Board pull 3", until the session retitles itself after its first turn`},
	} {
		if got := titleLine("Board pull 3", c.title); got != c.want {
			t.Errorf("titleLine(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

// A claude:// link waits while the desktop's window has the focus, and goes
// once it lost it; a window that keeps it, or a compositor that cannot be
// asked, holds the link past the wait.
func TestDeskAwaitsTheFocus(t *testing.T) {
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	for name, tc := range map[string]struct {
		active func(n int) (bool, error)
		want   error
	}{
		"away at once":          {func(int) (bool, error) { return false, nil }, nil},
		"leaves after a moment": {func(n int) (bool, error) { return n < 2, nil }, nil},
		"keeps the focus":       {func(int) (bool, error) { return true, nil }, errDesktopInUse},
		"cannot be asked":       {func(int) (bool, error) { return false, errors.New("hyprctl: no socket") }, errDesktopInUse},
	} {
		t.Run(name, func(t *testing.T) {
			var n int
			desktopWindowActive = func(context.Context) (bool, error) { n++; return tc.active(n - 1) }
			if got := (desk{quiet: -1}).await(t.Context(), 3*awayPoll, nil); !errors.Is(got, tc.want) {
				t.Errorf("await = %v after %d asks, want %v", got, n, tc.want)
			}
		})
	}
}

// A start issued while the person types imports only once their input has
// been quiet for desktop.typingQuiet, the window long without the focus.
func TestDeskAwaitsQuietInput(t *testing.T) {
	saved, savedInput := desktopWindowActive, desktopInput
	t.Cleanup(func() { desktopWindowActive, desktopInput = saved, savedInput })
	desktopWindowActive = func(context.Context) (bool, error) { return false, nil }
	typedAt := time.Now()
	desktopInput = func(context.Context) (func() time.Time, error) { return func() time.Time { return typedAt }, nil }
	a := &app{cfg: &config.Config{Desktop: config.Desktop{TypingQuiet: config.Duration{Duration: 2 * awayPoll}}}}
	d, err := a.watchDesk(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	d.locked = nil // the test machine's own screen lock does not count
	start := time.Now()
	if err := d.await(t.Context(), awayPoll, nil); !errors.Is(err, errTyping) {
		t.Fatalf("await while typing = %v, want errTyping", err)
	}
	if err := d.await(t.Context(), 4*awayPoll, nil); err != nil {
		t.Fatalf("await once input went quiet = %v", err)
	}
	if quiet := time.Since(start); quiet < 2*awayPoll {
		t.Errorf("the link went %s after the last input, before desktop.typingQuiet", quiet)
	}
	// Input that cannot be read stops a start before anything runs.
	desktopInput = func(context.Context) (func() time.Time, error) { return nil, errors.New("permission denied") }
	if _, err := a.watchDesk(t.Context()); err == nil || !strings.Contains(err.Error(), "desktop.typingQuiet: -1s") {
		t.Errorf("unreadable input: %v", err)
	}
	a.cfg.Desktop.TypingQuiet.Duration = -1
	if _, err := a.watchDesk(t.Context()); err != nil {
		t.Errorf("a negative desktop.typingQuiet still watches the input: %v", err)
	}
}

// A showBriefly while the desktop's window keeps the focus opens no link.
func TestShowBrieflyHeldByFocus(t *testing.T) {
	saved := desktopWindowActive
	t.Cleanup(func() { desktopWindowActive = saved })
	desktopWindowActive = func(context.Context) (bool, error) { return true, nil }
	a := &app{}
	if _, err := a.showBriefly(t.Context(), desk{quiet: -1}, "claude://nowhere", "local_x", "", true, awayPoll, nil); !errors.Is(err, errDesktopInUse) {
		t.Errorf("showBriefly = %v, want errDesktopInUse", err)
	}
}

// A show sends the desktop's window back to the session it showed before,
// unless that is a relieved role run's or a hand-over's predecessor's: the
// desktop warms the CLI of the session its window shows, which the watch
// stops as revived, so a window sent back there would revive it on every
// start, reopen and wake (the doctor's archive asks among them).
func TestShowBrieflyLeavesASupersededSession(t *testing.T) {
	taken := time.Now().Add(-time.Hour)
	guide38 := state.Party{Session: "guide-38", HostSession: "local_guide-38", Name: guideRole.runName(38)}
	guide39 := state.Party{Session: "guide-39", HostSession: "local_guide-39", Name: guideRole.runName(39)}
	handedOff := state.Party{Session: "ws-old", HostSession: "local_ws-old", Name: "WS 1 workspaces"}
	followUp := state.Party{Session: "ws-new", HostSession: "local_ws-new", Name: "WS 1 workspaces"}
	person := state.Party{Session: "own-1", HostSession: "local_own-1", Name: "a session of its own"}
	for _, tc := range []struct {
		name, prev string
		back       bool
	}{
		{"a relieved guide run", guide38.HostSession, false},
		{"a hand-over's predecessor", handedOff.HostSession, false},
		{"the role's holder", guide39.HostSession, true},
		{"a session beekeeper did not start", person.HostSession, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			a.cfg.Claude.DesktopLog = filepath.Join(t.TempDir(), "main.log")
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Starts = []state.Start{
					{Party: guide38, At: taken.Add(-time.Hour)}, {Party: guide39, At: taken.Add(-time.Minute)},
					{Party: handedOff, At: taken.Add(-time.Hour)}, {Party: followUp, At: taken},
				}
				st.Agents = []state.Agent{{Party: followUp, Task: "workspaces"}}
				guideRole.set(st, state.Role{
					Holder:   &state.Supervisor{Party: guide39, Since: taken},
					Relieved: []state.Relief{{Party: guide38, By: guide39, At: taken.Add(-time.Minute), Taken: taken}},
				})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			saved := desktopWindowActive
			t.Cleanup(func() { desktopWindowActive = saved })
			desktopWindowActive = func(context.Context) (bool, error) { return false, nil }
			plat.Machine = tableMachine{plat.Machine}
			o := &focusOpener{log: a.cfg.Claude.DesktopLog}
			plat.Opener = o
			o.focus(t, tc.prev)
			const host = "local_new-worker"
			prev, err := a.showBriefly(t.Context(), desk{quiet: -1}, resumeURL("new-worker"), host, "", true, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{resumeURL("new-worker")}
			if tc.back {
				want = append(want, continueURL(tc.prev))
			}
			if !slices.Equal(o.opened, want) {
				t.Errorf("opened %v, want %v", o.opened, want)
			}
			if back := prev != ""; back != tc.back {
				t.Errorf("showBriefly went back to %q, want back %v", prev, tc.back)
			}
		})
	}
}

// focusOpener is a running desktop whose window shows the session of each
// link it opens, as its log's focus line says.
type focusOpener struct {
	recordingOpener
	log string
}

func (o *focusOpener) Open(ctx context.Context, url string, running bool) error {
	_, id, _ := strings.Cut(url, "session=")
	if !strings.HasPrefix(id, "local_") {
		id = "local_" + id
	}
	if err := o.write(id); err != nil {
		return err
	}
	return o.recordingOpener.Open(ctx, url, running)
}

func (o *focusOpener) focus(t *testing.T, host string) {
	t.Helper()
	if err := o.write(host); err != nil {
		t.Fatal(err)
	}
}

func (o *focusOpener) write(host string) error {
	f, err := os.OpenFile(filepath.Clean(o.log), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // test
	_, err = fmt.Fprintf(f, "x [CCD] LocalSessions.setFocusedSession: sessionId=%s\n", host)
	return err
}

// A reopen right after its start's import waits for the start's switch back
// before it records where to return to.
func TestAwaitFocusOff(t *testing.T) {
	log := filepath.Join(t.TempDir(), "main.log")
	write := func(id string) {
		f, err := os.OpenFile(filepath.Clean(log), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck // test
		if _, err := fmt.Fprintf(f, "x [CCD] LocalSessions.setFocusedSession: sessionId=%s\n", id); err != nil {
			t.Fatal(err)
		}
	}
	write("local_other")
	if got := awaitFocusOff(t.Context(), log, "local_new", time.Second); got != "local_other" {
		t.Errorf("window on another session: awaitFocusOff = %q", got)
	}
	write("local_new")
	go func() { time.Sleep(300 * time.Millisecond); write("local_person") }()
	if got := awaitFocusOff(t.Context(), log, "local_new", 3*time.Second); got != "local_person" {
		t.Errorf("switch back landing: awaitFocusOff = %q, want local_person", got)
	}
	write("local_new")
	if got := awaitFocusOff(t.Context(), log, "local_new", 300*time.Millisecond); got != "local_new" {
		t.Errorf("window staying on host: awaitFocusOff = %q", got)
	}
	if got := awaitFocusOff(t.Context(), filepath.Join(t.TempDir(), "none.log"), "local_new", time.Second); got != "" {
		t.Errorf("unreadable log: awaitFocusOff = %q", got)
	}
}

// start says whether the imported session's browser waits on a person: only
// skip_all_permission_checks spares it the desktop's site requests.
func TestBrowserLine(t *testing.T) {
	for chrome, asks := range map[string]bool{"": true, "ask": true, "follow_a_plan": true, claude.ChromeSkipAll: false} {
		line := browserLine(chrome)
		if got := strings.Contains(line, "waits on a person's site request"); got != asks {
			t.Errorf("browserLine(%q) = %q, asks = %v, want %v", chrome, line, got, asks)
		}
	}
}

// A start that delivers no turn of its task fails loudly: a non-zero exit
// naming the reason, an agents.start line, and the roster saying so until a
// delivered wake clears it.
func TestUndeliveredTaskIsLoud(t *testing.T) {
	a, _ := stubApp(t)
	p := rotationParty
	rosterAgent(t, a, state.Agent{Party: p, Task: rotationTask})
	why := errors.New("importing it into the desktop: the desktop runs its cap of 28 CLIs")
	err := a.undelivered(state.Party{Name: "the starter"}, p, why)
	if err == nil || !errors.Is(err, why) || !strings.Contains(err.Error(), "task not delivered") {
		t.Fatalf("error %v, want a task-not-delivered error wrapping the reason", err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.start" })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, rotationName+": task not delivered: "+why.Error()) {
		t.Errorf("log %+v", evs)
	}
	st, _ := a.store.Read()
	views := a.agentViews(st, nil)
	if len(views) != 1 || views[0].Reachable != "task not delivered: "+why.Error() {
		t.Errorf("roster %+v", views)
	}
	a.markDelivered(p)
	st, _ = a.store.Read()
	if views := a.agentViews(st, nil); views[0].Reachable != "not running" {
		t.Errorf("after a delivered wake: %q", views[0].Reachable)
	}
}

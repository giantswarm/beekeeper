package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestCommandHead(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{testShell, "-c", "cd /x &&\n  go test ./...  "}, "cd /x && go test ./..."},
		{[]string{"/usr/bin/bash", "-c", "make test", "arg0"}, "make test"},
		{[]string{"python3", "-c", "bytearray(200<<20)"}, "python3 -c bytearray(200<<20)"},
		{[]string{"go", "test", strings.Repeat("x", 200)}, "go test " + strings.Repeat("x", 91) + "…"},
		// too long: the heavy command leads, not the checkout before it
		{[]string{testShell, "-c", slotIncident}, "… go test -race -timeout 25m -run '^TestSlackProof$' -count=150 -v ./internal/lab/ 2>&1 | /usr/bin/…"},
		{[]string{testShell, "-c", "git -C " + strings.Repeat("x", 100) + " switch -q main\ngo build ./..."}, "… go build ./..."},
		// too long with no heavy command: the start, clipped
		{[]string{testShell, "-c", "git -C " + strings.Repeat("x", 100) + " checkout -q -- ."}, "git -C " + strings.Repeat("x", 92) + "…"},
	} {
		if got := CommandHead(c.argv); got != c.want {
			t.Errorf("CommandHead(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

// slotIncident is a compound command whose start (an assignment, a git
// checkout, a worktree removal) fills a slot record's width; the go test
// after it is what holds the slot.
const slotIncident = `S=/tmp/claude-1000/-home-teemow-projects-klaus-lab/5df9a204-e414-4e77-b24b-b146480a8572/scratchpad/amp; ` +
	`git -C "$S" checkout -q -- . && git -C ~/projects/giantswarm/agentlab worktree remove "$S"; ` +
	`cd ~/projects/worktrees/agentlab/fix-slackproof-flake && go test -race -timeout 25m -run '^TestSlackProof$' -count=150 -v ./internal/lab/ 2>&1 | /usr/bin/grep -E "^panic|^(--- |ok|FAIL)" | sort | uniq -c`

func TestHolderNamesTheHeavyCommand(t *testing.T) {
	o := Options{SlotDir: t.TempDir(), Max: "12G"}
	writeHolder(o, 1, []string{testShell, "-c", slotIncident})
	b, err := os.ReadFile(filepath.Join(o.SlotDir, "1.holder"))
	if err != nil {
		t.Fatal(err)
	}
	var h holderRecord
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.Cmd, "… go test -race -timeout 25m -run '^TestSlackProof$' -count=150") || strings.Contains(h.Cmd, "checkout") ||
		strings.Contains(h.Cmd, `"`) || len([]rune(h.Cmd)) > 140 {
		t.Errorf("holder cmd %q: want the go test first, no checkout, no double quote, at most 140 characters", h.Cmd)
	}
}

func TestRunEventDetail(t *testing.T) {
	d := "memcap-2516344-492870.scope slot 1 max 12G: go test ./... -run 'A: B'"
	if RunScope(d) != "memcap-2516344-492870.scope" || RunCommand(d) != "go test ./... -run 'A: B'" {
		t.Errorf("scope %q, command %q", RunScope(d), RunCommand(d))
	}
}

// trueCmd is the command the slot tests run.
var trueCmd = []string{"true"}

// fakeCapper runs the command uncapped and records the slots it was asked
// to cap and to run in.
type fakeCapper struct {
	capped, ran []string
	// down makes it unavailable: no user systemd, no broker.
	down bool
}

func (f *fakeCapper) Available() bool                     { return !f.down }
func (*fakeCapper) Capped() bool                          { return false }
func (*fakeCapper) Adopt(int, string, platform.Cap) error { return nil }
func (f *fakeCapper) CapSlot(c platform.Cap) error {
	f.capped = append(f.capped, c.Slice)
	return nil
}
func (f *fakeCapper) Command(_ string, c platform.Cap, argv []string) (*exec.Cmd, error) {
	f.ran = append(f.ran, c.Slice)
	return exec.Command(argv[0], argv[1:]...), nil //nolint:gosec // the test's own command
}

// holdSlot holds slot n of dir for session as a running beekeeper run does.
func holdSlot(t *testing.T, dir string, n int, session string) {
	t.Helper()
	l := flock.New(filepath.Join(dir, strconv.Itoa(n)+".lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock slot %d: %v", n, err)
	}
	t.Cleanup(func() { _ = l.Unlock() })
	rec := fmt.Sprintf(`{"pid":1,"session":%q,"cwd":"/","cmd":"agentlab backstage-test","max":"12G","since":"2026-10-04T14:00:00Z"}`, session)
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(n)+".holder"), []byte(rec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunSharesItsSessionsSlot(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 2, Stderr: io.Discard}
	holdSlot(t, dir, 2, "parent")
	holdSlot(t, dir, 1, "other")

	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	start := time.Now()
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("a run inside its session's slot: exit %d, want 0", rc)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("a run inside its session's slot waited %s", time.Since(start))
	}
	if len(f.capped) != 0 || !slices.Equal(f.ran, []string{o.SlotSlice(2)}) {
		t.Errorf("capped slots %v, ran in %v: want none capped, ran in the shared slot 2", f.capped, f.ran)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "2.holder")) //nolint:gosec // the test's own temp dir
	if !strings.Contains(string(raw), `"session":"parent"`) {
		t.Errorf("the shared slot's holder record changed: %s", raw)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "outside")
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Errorf("a run outside a slot with every slot held: exit %d, want %d", rc, ExitBusy)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if rc := Run(o, trueCmd); rc != ExitBusy {
		t.Errorf("a run of no session with every slot held: exit %d, want %d", rc, ExitBusy)
	}
}

func TestRunTakesAndCapsAFreeSlot(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	holdSlot(t, dir, 1, "other")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "fresh")
	o := Options{Max: "1M", Swap: "0", SlotDir: dir, Slots: 2, Stderr: io.Discard}
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	if !slices.Equal(f.capped, []string{o.SlotSlice(2)}) || !slices.Equal(f.ran, []string{o.SlotSlice(2)}) {
		t.Errorf("capped slots %v, ran in %v: want slot 2 capped and run in", f.capped, f.ran)
	}
	if _, err := os.Stat(filepath.Join(dir, "2.holder")); !os.IsNotExist(err) {
		t.Errorf("slot 2's holder record outlived the run: %v", err)
	}
}

func TestRunJoinsTheSlotItsSessionTakesDuringTheWait(t *testing.T) {
	f := &fakeCapper{}
	saved := plat.Capper
	plat.Capper = f
	t.Cleanup(func() { plat.Capper = saved })
	dir := t.TempDir()
	l := flock.New(filepath.Join(dir, "1.lock"))
	if ok, err := l.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v", err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	go func() {
		time.Sleep(time.Second)
		_ = os.WriteFile(filepath.Join(dir, "1.holder"), []byte(`{"pid":1,"session":"parent"}`+"\n"), 0o600)
	}()
	t.Cleanup(func() { _ = l.Unlock() })
	o := Options{Max: "1M", Swap: "0", Wait: 20 * time.Second, SlotDir: dir, Slots: 1, Stderr: io.Discard}
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("exit %d, want 0", rc)
	}
	if len(f.capped) != 0 || !slices.Equal(f.ran, []string{o.SlotSlice(1)}) {
		t.Errorf("capped slots %v, ran in %v: want none capped, ran in the shared slot 1", f.capped, f.ran)
	}
}

func TestSlotSliceIsTheSlotDirectorysOwn(t *testing.T) {
	machine, scratch := Options{SlotDir: "/home/u/.local/state/memcap/slots"}, Options{SlotDir: "/tmp/test/slots"}
	a, b := machine.SlotSlice(1), scratch.SlotSlice(1)
	if a == b || strings.Count(a, "-") != 1 || !strings.HasPrefix(a, "memcap-slot1_") || !strings.HasSuffix(a, ".slice") {
		t.Errorf("SlotSlice: %q and %q", a, b)
	}
	if (Options{SlotDir: "/tmp/test/slots/"}).SlotSlice(1) != b || scratch.SlotSlice(2) == b {
		t.Error("SlotSlice: want one slice per clean directory and slot")
	}
	if got := (Options{SlotDir: t.TempDir(), Test: true}).SlotSlice(1); got != "memcap-slot1_test.slice" {
		t.Errorf("a test's SlotSlice = %q", got)
	}
}

func TestRunInTheSandboxGoesThroughTheBroker(t *testing.T) {
	host := &fakeCapper{down: true}
	saved := plat.Capper
	plat.Capper = host
	t.Cleanup(func() { plat.Capper = saved })
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sandboxed")
	broker := &fakeCapper{}
	o := Options{Max: "1M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Stderr: io.Discard, Sandbox: broker}
	if rc := Run(o, trueCmd); rc != 0 {
		t.Fatalf("a sandboxed run with a broker: exit %d, want 0", rc)
	}
	if !slices.Equal(broker.capped, []string{o.SlotSlice(1)}) || !slices.Equal(broker.ran, []string{o.SlotSlice(1)}) || len(host.ran) != 0 {
		t.Errorf("broker capped %v, ran %v; host ran %v", broker.capped, broker.ran, host.ran)
	}

	var stderr strings.Builder
	o.Sandbox, o.Stderr = &fakeCapper{down: true}, &stderr
	if rc := Run(o, []string{"false-is-never-run"}); rc != 1 || !strings.Contains(stderr.String(), "refused: the agent sandbox") {
		t.Errorf("a sandboxed run without a broker: exit %d, %q; want 1 and the refusal", rc, stderr.String())
	}
}

func TestResolveWait(t *testing.T) {
	for _, c := range []struct {
		bg, env, flag string
		flagSet       bool
		wait, from    string
		background    bool
	}{
		{wait: ForegroundWait, from: WaitFromForeground},
		{bg: "1", wait: BackgroundWait, from: WaitFromBackground, background: true},
		{bg: "1", env: "5m", wait: "5m", from: WaitFromEnv, background: true},
		{env: "5m", wait: "5m", from: WaitFromEnv},
		{bg: "1", env: "5m", flag: "2m", flagSet: true, wait: "2m", from: WaitFromFlag, background: true},
	} {
		t.Setenv(BackgroundEnv, c.bg)
		t.Setenv("MEMCAP_WAIT", c.env)
		wait, from, bg := ResolveWait(c.flag, c.flagSet)
		if wait != c.wait || from != c.from || bg != c.background {
			t.Errorf("%+v: got %s, %s, %v", c, wait, from, bg)
		}
	}
}

func TestWaitText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour: "120m", 60 * time.Minute: "60m", 8 * time.Minute: "8m", 10 * time.Minute: "10m", time.Second: "1s", 90 * time.Second: "1m30s",
	} {
		if got := (Options{Wait: d}).waitText(); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
	if got := (Options{Wait: time.Hour, WaitFrom: WaitFromBackground}).waitText(); got != "60m (the background default)" {
		t.Errorf("with its source: %q", got)
	}
}

// Package guard keeps builds from taking the machine down. Run takes one of
// the machine's build slots and runs a command in a memory-capped systemd
// scope; Hook is the PreToolUse hook that routes build, test, lint and lab
// commands through it and refuses a third kind lab.
//
// The slots are memcap's: flock files <slotDir>/<i>.lock with a <i>.holder
// record in memcap's format, so beekeeper and the memcap shell wrapper share
// them. A slot is a slice too, capped like one run: a session's runs share
// the slot it holds, and its cap, instead of taking a second one.
package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// Exit codes Run returns besides the command's own.
const (
	// ExitBusy: no slot, or not enough memory for the cap, within the wait.
	ExitBusy = 75
	// ExitNotFound: the command could not be started.
	ExitNotFound = 127
)

// LogPrefix starts every line Run writes, the cap's victim line included.
const LogPrefix = "beekeeper run: "

// The wait for a slot when neither --wait nor MEMCAP_WAIT sets one: a
// foreground call's fits within the Bash tool's 10 minutes, a background
// call's, which no tool limit stops, is longer. The PreToolUse hook exports
// BackgroundEnv=1 into every background Bash call, so a run it reaches
// directly, after a cd or export chain or through a script gets the
// background wait.
const (
	ForegroundWait = "8m"
	BackgroundWait = "60m"
	BackgroundEnv  = "MEMCAP_BACKGROUND"
)

// Where a run's wait came from (Options.WaitFrom).
const (
	WaitFromForeground = "the foreground default"
	WaitFromBackground = "the background default"
	WaitFromEnv        = "MEMCAP_WAIT"
	WaitFromFlag       = "--wait"
)

// plat caps the runs and reads the machine.
var plat = platform.Current(platform.Options{})

// The event log verbs of a capped run. Their detail is
// "<scope> <facts>: <command>": the scope's unit name exactly as the kernel
// prints it in an OOM kill's memcg path, then the slot and the cap (start) or
// the exit code, the duration and the cap's victims (end), then CommandHead.
const (
	VerbStart = "run.start"
	VerbEnd   = "run.end"
)

// A capped run's scope is <ScopePrefix><pid>-<n>.scope in memcap.slice. A
// test's run (Options.Test) takes TestScopePrefix instead, so that the cap
// kill it provokes on purpose reads as a test kill, never as a build's.
const (
	ScopePrefix     = "memcap-"
	TestScopePrefix = "memcap-test-"
)

// SlotSlice is the slice of the run's slot n under memcap.slice (systemd
// nests a slice by the dashes in its name, so the rest has none), named by
// the slot directory: a scratch state's slot never shares a cap with the
// machine's slot of the same number. A test's run (Test) takes one slice
// per slot number for every test, so that the temporary slot directories of
// tests leave no slice behind each.
func (o Options) SlotSlice(n int) string {
	if o.Test {
		return fmt.Sprintf("memcap-slot%d_test.slice", n)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(o.SlotDir)))
	return fmt.Sprintf("memcap-slot%d_%08x.slice", n, h.Sum32())
}

// IsTestScope reports whether a scope's unit name is a test run's.
func IsTestScope(unit string) bool {
	return strings.HasPrefix(unit, TestScopePrefix)
}

// RunScope is the scope a run event names.
func RunScope(detail string) string {
	scope, _, _ := strings.Cut(detail, " ")
	return scope
}

// RunCommand is the command a run event names.
func RunCommand(detail string) string {
	_, cmd, _ := strings.Cut(detail, ": ")
	return cmd
}

// CommandHead is the command a run's events name, at most 100 characters
// (commandHead).
func CommandHead(argv []string) string { return commandHead(argv, 100) }

// commandHead is the command a run names: a shell's -c script rather than the
// shell (the hook wraps every build in zsh -c), on one line, at most n
// characters. A command too long for that leads with the heavy command that
// holds the slot ("… go test …"), never with what runs before it (a cd, an
// assignment, a git checkout).
func commandHead(argv []string, n int) string {
	cmd := strings.Join(argv, " ")
	if len(argv) >= 3 && argv[1] == "-c" && slices.Contains([]string{"sh", "bash", "zsh"}, filepath.Base(argv[0])) {
		cmd = argv[2]
	}
	s := oneLine(cmd)
	if len([]rune(s)) <= n {
		return s
	}
	if m := commandMatch(heavy, cmd, keepHeavy); m != nil && m[2] > 0 {
		s = "… " + oneLine(cmd[m[2]:])
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// oneLine is s with every run of whitespace a single space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Options configure one capped run.
type Options struct {
	// Max is the scope's MemoryMax, a systemd size ("12G").
	Max string
	// Swap is the scope's MemorySwapMax; "0": a build does not swap.
	Swap string
	// CPUQuota and CPUWeight are memcap.slice's CPU budget, which every
	// slot shares (platform.Cap).
	CPUQuota  string
	CPUWeight int
	// Wait bounds the wait for a slot and for the memory the cap needs;
	// WaitFrom names where it came from (WaitFromForeground, …), for the
	// lines that state it.
	Wait     time.Duration
	WaitFrom string
	// Background: the run is part of a background Bash call (BackgroundEnv).
	Background bool
	SlotDir    string
	Slots      int
	Stderr     io.Writer
	// Record appends a run event (VerbStart, VerbEnd) to the event log; nil
	// records nothing. It must not fail or block the run: it swallows its
	// own errors and bounds its wait.
	Record func(verb, detail string)
	// Test names the scope TestScopePrefix…: a test's run, whose cap kill
	// snapshot and watch report as a test kill.
	Test bool
	// Sandbox caps the run when the agent sandbox keeps the service
	// manager out of reach (the host's broker); nil outside the sandbox.
	Sandbox platform.Capper
	// GoCache is the Go build cache the command compiles with
	// (gocache.Dir); "" for none. The run holds its use lock shared for the
	// length of the command, so no trim removes an entry the compile reads.
	GoCache string
	// TmpDir holds each run's own temporary directory, which GOTMPDIR and
	// TMPDIR name for the command (runTmp), off the tmpfs /tmp whose files
	// take RAM; "" leaves the caller's. The run removes it after the
	// command.
	TmpDir string
}

// cap is the run's cap in the slice of slot n.
func (o Options) cap(n int) platform.Cap {
	return platform.Cap{Max: o.Max, Swap: o.Swap, Slice: o.SlotSlice(n), CPUQuota: o.CPUQuota, CPUWeight: o.CPUWeight}
}

func (o Options) record(verb, detail string) {
	if o.Record != nil {
		o.Record(verb, detail)
	}
}

var sizeRe = regexp.MustCompile(`^(\d+)([KkMmGgTt]?)$`)

// ParseSize parses a systemd size with base-1024 suffixes into KiB.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q: want a number with an optional K, M, G or T suffix", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	switch strings.ToUpper(m[2]) {
	case "":
		return n / 1024, nil
	case "K":
		return n, nil
	case "M":
		return n << 10, nil
	case "G":
		return n << 20, nil
	default:
		return n << 30, nil
	}
}

// ResolveWait returns a run's wait, where it came from (WaitFrom…) and
// whether the run is part of a background Bash call: flag when set, else
// $MEMCAP_WAIT, else BackgroundWait with BackgroundEnv=1, else
// ForegroundWait.
func ResolveWait(flag string, flagSet bool) (wait, from string, background bool) {
	background = os.Getenv(BackgroundEnv) == "1"
	switch env := os.Getenv("MEMCAP_WAIT"); {
	case flagSet:
		return flag, WaitFromFlag, background
	case env != "":
		return env, WaitFromEnv, background
	case background:
		return BackgroundWait, WaitFromBackground, background
	default:
		return ForegroundWait, WaitFromForeground, background
	}
}

// waitText states the wait applied and, when known, where it came from:
// "60m (the background default)"; whole minutes in minutes, as the defaults
// are written.
func (o Options) waitText() string {
	s := o.Wait.String()
	if o.Wait > 0 && o.Wait%time.Minute == 0 {
		s = strconv.Itoa(int(o.Wait/time.Minute)) + "m"
	}
	if o.WaitFrom == "" {
		return s
	}
	return s + " (" + o.WaitFrom + ")"
}

// ParseWait parses a duration ("8m", "90s", "1h"); a bare number is seconds.
func ParseWait(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid wait %q: want a duration such as 90s, 8m or 1h", s)
	}
	return d, nil
}

// Run runs argv in a capped scope and returns its exit code: the command's,
// 128+signal when a signal ended it, ExitBusy when the wait ran out.
// Without a user systemd, or inside a capped scope already, it execs argv.
// In the agent sandbox (o.Sandbox), which keeps the user systemd out of
// reach, the host's broker caps it; without a broker it refuses, 1.
// A run of a session that holds a slot already joins that slot at once: its
// scope goes into the slot's slice and shares the slot's cap.
func Run(o Options, argv []string) int {
	logf := func(format string, a ...any) { _, _ = fmt.Fprintf(o.Stderr, LogPrefix+format+"\n", a...) }
	capper := plat.Capper
	switch {
	case capper.Available():
	case o.Sandbox == nil:
		logf("no user systemd here, running uncapped: %s", argv[0])
		return execve(argv, logf)
	case !o.Sandbox.Available():
		logf("refused: the agent sandbox keeps the user systemd out of reach and no sandbox broker answers; " +
			"the host runs it as beekeeper-sandbox.service (beekeeper install), an uncapped build would put the machine at risk")
		return 1
	default:
		capper = o.Sandbox
	}
	if capper.Capped() {
		return execve(argv, logf)
	}
	needKiB, err := ParseSize(o.Max)
	if err != nil {
		logf("%v", err)
		return 2
	}
	if err := os.MkdirAll(o.SlotDir, 0o750); err != nil {
		logf("%v", err)
		return 1
	}

	lock, slot := waitForSlot(o, needKiB, sessionID(), argv, logf)
	shared := lock == nil
	switch {
	case slot == 0:
		return ExitBusy
	case shared:
		logf("joining slot %d, which this session holds: the command shares the slot's cap", slot)
	default:
		defer func() { _ = lock.Unlock() }()
		holder := filepath.Join(o.SlotDir, strconv.Itoa(slot)+".holder")
		defer func() { _ = os.Remove(holder) }()
		if err := capper.CapSlot(o.cap(slot)); err != nil {
			logf("%v", err)
			return 1
		}
	}

	env, cleanup := runTmp(o.TmpDir, logf)
	defer cleanup()
	if o.GoCache != "" {
		release, ok := holdGoCache(o, logf)
		if !ok {
			return ExitBusy
		}
		defer release()
		env = append(env, gocache.HeldEnv+"="+o.GoCache)
	}

	prefix := ScopePrefix
	if o.Test {
		prefix = TestScopePrefix
	}
	unit := fmt.Sprintf("%s%d-%06d", prefix, os.Getpid(), time.Now().Nanosecond()%1000000)
	head := CommandHead(argv)
	how := "slot"
	if shared {
		how = "shared slot"
	}
	o.record(VerbStart, fmt.Sprintf("%s.scope %s %d max %s: %s", unit, how, slot, o.Max, head))
	start := time.Now()
	rc := scope(capper, unit, o, slot, argv, env, logf)
	end := fmt.Sprintf("%s.scope exit %d after %s", unit, rc, time.Since(start).Round(time.Second))
	if left := reap(capper, unit, slot, logf); left != "" {
		end += ", " + left
	}
	if rc != 0 {
		if kills := victims(unit, start, rc); len(kills) > 0 {
			what, advice, detail := killReport(o, unit, slot, shared, kills)
			logf("%s (exit %d) — %s", what, rc, advice)
			end += ", " + detail
		}
	}
	o.record(VerbEnd, end+": "+head)
	return rc
}

// holdGoCache takes the Go build cache's use lock shared for the command,
// waiting up to o.Wait for a trim or a clear under way; false when the wait
// ran out. A
// lock that cannot be taken at all (the cache unwritable) leaves the
// command to run without it, which the line says.
func holdGoCache(o Options, logf func(string, ...any)) (release func(), ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), max(o.Wait, time.Second))
	defer cancel()
	t0 := time.Now()
	release, err := gocache.Use(ctx, o.GoCache)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		logf("the Go build cache %s stayed under a trim or a clear for %s; nothing ran", o.GoCache, o.waitText())
		return nil, false
	case err != nil:
		logf("running without the Go build cache's use lock: %v", err)
		return func() {}, true
	}
	if d := time.Since(t0); d >= time.Second {
		logf("the Go build cache's trim or clear ended after %ds", int(d.Seconds()))
	}
	return release, true
}

// waitForSlot takes a free slot in its turn of the slot queue and waits
// until MemAvailable covers the cap, both within o.Wait: one line when the
// wait starts, one when it ends. A slot the session holds, before or during
// the wait, is shared at once: the slot without a lock. No slot within the
// wait: 0, and the log names the command as not run.
func waitForSlot(o Options, needKiB int64, session string, argv []string, logf func(string, ...any)) (*flock.Flock, int) {
	deadline := time.Now().Add(o.Wait)
	t0 := time.Now()
	waited := false
	var lock *flock.Flock
	slot := 0
	// A queue that cannot be joined leaves the run to take a slot as it
	// frees, as before the queue.
	t, _ := enqueue(o.SlotDir, queuedRun(argv))
	defer func() { t.leave() }()
	for {
		var reason string
		if lock == nil {
			if n := sessionSlot(o, session); n > 0 {
				return nil, n
			}
			turn, ahead := t.turn(o.Slots)
			var occupied string
			switch {
			case !turn:
				reason = fmt.Sprintf("its turn in the build slot queue:\n%s%s", holders(o), ahead)
			default:
				if lock, slot, occupied = acquire(o); lock != nil {
					writeHolder(o, slot, argv)
					t.leave()
					t = nil
				} else {
					reason = fmt.Sprintf("a build slot (all %d held):\n%s%s", o.Slots, holders(o), occupied)
				}
			}
		}
		if lock != nil {
			m, err := plat.Machine.Mem()
			if err != nil || int64(m.AvailableMiB)<<10 >= needKiB {
				break
			}
			reason = fmt.Sprintf("memory: %d MiB available, the cap needs %d", m.AvailableMiB, needKiB>>10)
		}
		now := time.Now()
		if !now.Before(deadline) {
			logf("gave up after %s waiting for %s", o.waitText(), reason)
			notRun := fmt.Sprintf("not run: %s — the machine is busy", CommandHead(argv))
			if o.Background {
				logf("%s; try again later — do not poll", notRun)
			} else {
				logf("%s; run this command with run_in_background (the wait is then %s) or try again later — do not poll", notRun, BackgroundWait)
			}
			if lock != nil {
				_ = os.Remove(filepath.Join(o.SlotDir, strconv.Itoa(slot)+".holder"))
				_ = lock.Unlock()
			}
			return nil, 0
		}
		if !waited {
			logf("waiting up to %s for %s", o.waitText(), reason)
			waited = true
		}
		time.Sleep(min(deadline.Sub(now), slotPoll))
	}
	if waited {
		logf("slot %d after %ds", slot, int(time.Since(t0).Seconds()))
	}
	return lock, slot
}

// acquire takes the first free slot. A slot whose lock is free while its
// slice still runs scopes (a run that joined the slot outlives the run that
// held it) is not free: capping the slice for this run would set another
// run's cap, below that run's memory when this run's cap is smaller, and
// the kernel would kill at the slice's cap. Those slots are named in
// occupied, one indented line each.
func acquire(o Options) (lock *flock.Flock, slot int, occupied string) {
	var b strings.Builder
	for i := 1; i <= o.Slots; i++ {
		l := flock.New(filepath.Join(o.SlotDir, strconv.Itoa(i)+".lock"))
		if ok, err := l.TryLock(); err != nil || !ok {
			continue
		}
		if s := plat.Machine.SlotSlice(o.SlotSlice(i)); s != nil && len(s.Scopes) > 0 {
			_ = l.Unlock()
			fmt.Fprintf(&b, "  slot %d: free, but its slice still runs %s (%d MiB)\n", i, strings.Join(s.Scopes, ", "), s.CurrentMiB)
			continue
		}
		return l, i, ""
	}
	return nil, 0, b.String()
}

// holders lists the held slots' records, one indented line each.
func holders(o Options) string {
	var b strings.Builder
	for _, s := range machine.ReadSlots(o.SlotDir, o.Slots) {
		if !s.Free {
			fmt.Fprintf(&b, "  slot %d: %s\n", s.N, s.Holder)
		}
	}
	return b.String()
}

// sessionSlot is the slot a run of the session holds, 0 for none or no
// session.
func sessionSlot(o Options, session string) int {
	if session == "" {
		return 0
	}
	for _, s := range machine.ReadSlots(o.SlotDir, o.Slots) {
		var h holderRecord
		if !s.Free && json.Unmarshal([]byte(s.Holder), &h) == nil && h.Session == session {
			return s.N
		}
	}
	return 0
}

// sessionID is the Claude Code session running this process, "" outside one.
func sessionID() string {
	if s := os.Getenv("CLAUDE_CODE_SESSION_ID"); s != "" {
		return s
	}
	return os.Getenv("CLAUDE_SESSION_ID")
}

// holderRecord is memcap's <i>.holder, field for field.
type holderRecord struct {
	PID     int    `json:"pid"`
	Session string `json:"session"`
	CWD     string `json:"cwd"`
	Cmd     string `json:"cmd"`
	Max     string `json:"max"`
	Since   string `json:"since"`
}

func writeHolder(o Options, slot int, argv []string) {
	cmd := strings.ReplaceAll(commandHead(argv, 140), `"`, "'")
	cwd, _ := os.Getwd()
	f, err := os.OpenFile(filepath.Join(o.SlotDir, strconv.Itoa(slot)+".holder"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(holderRecord{PID: os.Getpid(), Session: sessionID(), CWD: cwd, Cmd: cmd, Max: o.Max,
		Since: time.Now().UTC().Format("2006-01-02T15:04:05Z")})
}

// scope runs argv in the capped scope unit, in the slot's slice, with env
// over this process's environment.
func scope(capper platform.Capper, unit string, o Options, slot int, argv, env []string, logf func(string, ...any)) int {
	c, err := capper.Command(unit, o.cap(slot), argv)
	if err != nil {
		logf("%v", err)
		return ExitNotFound
	}
	if len(env) > 0 {
		if c.Env == nil {
			c.Env = os.Environ()
		}
		c.Env = append(c.Env, env...)
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := c.Start(); err != nil {
		logf("%v", err)
		return ExitNotFound
	}
	go func() {
		for s := range sig {
			_ = c.Process.Signal(s)
		}
	}()
	err = c.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		logf("%v", err)
		return 1
	}
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return c.ProcessState.ExitCode()
}

// reap stops the run's scope once the command has exited and left
// processes in it (a test's kube-apiserver): a scope that runs on keeps the
// slot's slice busy, and no run takes the slot while it does. It runs
// before the slot is released. Its result is the end event's detail, ""
// when the scope ended with its command.
func reap(capper platform.Capper, unit string, slot int, logf func(string, ...any)) string {
	// The processes' names, read while they run.
	var t *proc.Table
	if s := plat.Machine.MemcapScope(unit + ".scope"); s != nil && len(plat.Machine.ScopePIDs(s.Path)) > 0 {
		t, _ = plat.Machine.Processes()
	}
	left, err := capper.Reap(unit)
	if len(left) == 0 {
		if err != nil {
			logf("stopping %s.scope: %v", unit, err)
		}
		return ""
	}
	procs := ProcList(left, t)
	if err != nil {
		logf("the command exited and left %s in %s.scope; stopping it failed: %v — slot %d stays busy until they end or the watch stops the scope",
			procs, unit, err, slot)
		return "left " + procs + ", stop failed"
	}
	logf("the command exited and left %s in %s.scope: stopped them, they hold slot %d no longer", procs, unit, slot)
	return "stopped what it left: " + procs
}

// maxProcs is how many processes ProcList names before it counts the rest.
const maxProcs = 5

// ProcList names the processes, "4711 (kube-apiserver), 4712 (etcd)" by the
// table t (nil: their ids only), at most maxProcs of them and the count of
// the rest.
func ProcList(pids []int, t *proc.Table) string {
	var v []string
	for _, pid := range pids[:min(len(pids), maxProcs)] {
		if p := t.Process(pid); p != nil {
			v = append(v, fmt.Sprintf("%d (%s)", pid, p.Comm))
		} else {
			v = append(v, strconv.Itoa(pid))
		}
	}
	if len(pids) > maxProcs {
		v = append(v, fmt.Sprintf("%d more", len(pids)-maxProcs))
	}
	return strings.Join(v, ", ")
}

// killReport is what the kernel killed in the run's scope, the advice that
// follows it, and the end event's detail. The slot slice of a run that
// holds its slot is capped at the run's own cap, so a kill at either is the
// command's memory. A run that joined its session's slot shares the slice's cap with the
// slot's other runs. systemd-run killed before it ran the command is never
// the command's memory: other runs filled the slot's slice.
func killReport(o Options, unit string, slot int, shared bool, kills []machine.OOMKill) (what, advice, detail string) {
	var v []string
	atSlice, started := false, false
	for _, k := range kills {
		v = append(v, fmt.Sprintf("%d (%s) %d MiB", k.PID, k.Task, k.AnonMiB))
		atSlice = atSlice || strings.HasSuffix(k.Memcg, "/"+o.SlotSlice(slot))
		started = started || k.Task != "systemd-run"
	}
	names := strings.Join(v, "; ")
	switch {
	case !started:
		return fmt.Sprintf("slot %d's cap (%s) killed systemd-run before the command started: %s", slot, o.SlotSlice(slot), names),
			"other runs in the slot's slice filled it, not this command",
			fmt.Sprintf("slot %d's cap killed systemd-run before the command started: %s", slot, names)
	case shared && atSlice:
		return fmt.Sprintf("slot %d's shared cap (%s) killed: %s", slot, o.SlotSlice(slot), names),
			"the slot's runs together filled it",
			fmt.Sprintf("slot %d's shared cap killed %s", slot, names)
	default:
		return fmt.Sprintf("the %s cap killed: %s", o.Max, names),
			"bound the parallelism, do not raise --max",
			fmt.Sprintf("the %s cap killed %s", o.Max, names)
	}
}

// victims are the processes the kernel killed in the run's scope, at its
// own cap or its slot slice's. The kernel log can trail the exit of the
// killed process by a moment.
func victims(unit string, since time.Time, rc int) []machine.OOMKill {
	tries := 1
	if rc == 128+int(syscall.SIGKILL) {
		tries = 4
	}
	for i := range tries {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		kills, err := plat.Machine.OOMKills(ctx, since)
		cancel()
		if err != nil {
			return nil
		}
		var out []machine.OOMKill
		for _, k := range kills {
			if strings.HasSuffix(k.RunMemcg(), "/"+unit+".scope") {
				out = append(out, k)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// execve replaces this process with argv, as the shell's exec does.
func execve(argv []string, logf func(string, ...any)) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		logf("%v", err)
		return ExitNotFound
	}
	err = syscall.Exec(path, argv, os.Environ()) //nolint:gosec // running the caller's command is the purpose
	logf("%v", err)
	return ExitNotFound
}

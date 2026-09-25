// Package guard keeps builds from taking the machine down. Run takes one of
// the machine's build slots and runs a command in a memory-capped systemd
// scope; Hook is the PreToolUse hook that routes build, test, lint and lab
// commands through it and refuses a third kind lab.
//
// The slots are memcap's: flock files <slotDir>/<i>.lock with a <i>.holder
// record in memcap's format, so beekeeper and the memcap shell wrapper share
// them.
package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/giantswarm/beekeeper/internal/machine"
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

// The event log verbs of a capped run. Their detail is
// "<scope> <facts>: <command>": the scope's unit name exactly as the kernel
// prints it in an OOM kill's memcg path, then the slot and the cap (start) or
// the exit code, the duration and the cap's victims (end), then CommandHead.
const (
	VerbStart = "run.start"
	VerbEnd   = "run.end"
)

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

// CommandHead is the command a run's events name: a shell's -c script
// rather than the shell (the hook wraps every build in zsh -c), on one line,
// at most 100 characters.
func CommandHead(argv []string) string {
	cmd := argv
	if len(argv) >= 3 && argv[1] == "-c" && slices.Contains([]string{"sh", "bash", "zsh"}, filepath.Base(argv[0])) {
		cmd = argv[2:3]
	}
	s := strings.Join(strings.Fields(strings.Join(cmd, " ")), " ")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:99]) + "…"
	}
	return s
}

// Options configure one capped run.
type Options struct {
	// Max is the scope's MemoryMax, a systemd size ("12G").
	Max string
	// Swap is the scope's MemorySwapMax; "0": a build does not swap.
	Swap string
	// Wait bounds the wait for a slot and for the memory the cap needs.
	Wait    time.Duration
	SlotDir string
	Slots   int
	Stderr  io.Writer
	// Record appends a run event (VerbStart, VerbEnd) to the event log; nil
	// records nothing. It must not fail or block the run: it swallows its
	// own errors and bounds its wait.
	Record func(verb, detail string)
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

// UserSystemd reports whether a user service manager can start scopes.
// "degraded" (one failed unit somewhere) is a desktop's normal state.
func UserSystemd() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	out, _ := exec.Command("systemctl", "--user", "is-system-running").Output()
	switch strings.TrimSpace(string(out)) {
	case "running", "degraded", "starting", "maintenance":
		return true
	}
	return false
}

// inMemcapScope: a Makefile or script that runs a capped command again runs
// inside the outer scope, which already holds the slot and the cap.
func inMemcapScope() bool {
	raw, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && strings.Contains(string(raw), "/memcap.slice/")
}

// Run runs argv in a capped scope and returns its exit code: the command's,
// 128+signal when a signal ended it, ExitBusy when the wait ran out.
// Without a user systemd, or inside a capped scope already, it execs argv.
func Run(o Options, argv []string) int {
	logf := func(format string, a ...any) { _, _ = fmt.Fprintf(o.Stderr, LogPrefix+format+"\n", a...) }
	if !UserSystemd() {
		logf("no user systemd here, running uncapped: %s", argv[0])
		return execve(argv, logf)
	}
	if inMemcapScope() {
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

	lock, slot := waitForSlot(o, needKiB, argv, logf)
	if lock == nil {
		return ExitBusy
	}
	defer func() { _ = lock.Unlock() }()
	holder := filepath.Join(o.SlotDir, strconv.Itoa(slot)+".holder")
	defer func() { _ = os.Remove(holder) }()

	unit := fmt.Sprintf("memcap-%d-%06d", os.Getpid(), time.Now().Nanosecond()%1000000)
	head := CommandHead(argv)
	o.record(VerbStart, fmt.Sprintf("%s.scope slot %d max %s: %s", unit, slot, o.Max, head))
	start := time.Now()
	rc := scope(unit, o, argv, logf)
	end := fmt.Sprintf("%s.scope exit %d after %s", unit, rc, time.Since(start).Round(time.Second))
	if rc != 0 {
		if v := victims(unit, start, rc); len(v) > 0 {
			logf("the %s cap killed: %s (exit %d) — bound the parallelism, do not raise --max", o.Max, strings.Join(v, "; "), rc)
			end += fmt.Sprintf(", the %s cap killed %s", o.Max, strings.Join(v, "; "))
		}
	}
	o.record(VerbEnd, end+": "+head)
	return rc
}

// waitForSlot takes a free slot and waits until MemAvailable covers the cap,
// both within o.Wait: one line when the wait starts, one when it ends.
func waitForSlot(o Options, needKiB int64, argv []string, logf func(string, ...any)) (*flock.Flock, int) {
	deadline := time.Now().Add(o.Wait)
	t0 := time.Now()
	waited := false
	var lock *flock.Flock
	slot := 0
	for {
		if lock == nil {
			if lock, slot = acquire(o.SlotDir, o.Slots); lock != nil {
				writeHolder(o, slot, argv)
			}
		}
		var reason string
		if lock != nil {
			m, err := machine.ReadMem()
			if err != nil || int64(m.AvailableMiB)<<10 >= needKiB {
				break
			}
			reason = fmt.Sprintf("memory: %d MiB available, the cap needs %d", m.AvailableMiB, needKiB>>10)
		} else {
			reason = fmt.Sprintf("a build slot (all %d held):\n%s", o.Slots, holders(o))
		}
		now := time.Now()
		if !now.Before(deadline) {
			logf("gave up after %s waiting for %s", o.Wait, reason)
			logf("the machine is busy; run this command with run_in_background (the wait is then 60m) or try again later — do not poll")
			if lock != nil {
				_ = os.Remove(filepath.Join(o.SlotDir, strconv.Itoa(slot)+".holder"))
				_ = lock.Unlock()
			}
			return nil, 0
		}
		if !waited {
			logf("waiting (up to %s) for %s", o.Wait, reason)
			waited = true
		}
		time.Sleep(min(deadline.Sub(now), 5*time.Second))
	}
	if waited {
		logf("slot %d after %ds", slot, int(time.Since(t0).Seconds()))
	}
	return lock, slot
}

func acquire(dir string, n int) (*flock.Flock, int) {
	for i := 1; i <= n; i++ {
		l := flock.New(filepath.Join(dir, strconv.Itoa(i)+".lock"))
		if ok, err := l.TryLock(); err == nil && ok {
			return l, i
		}
	}
	return nil, 0
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
	cmd := strings.NewReplacer(`"`, "'", "\n", " ").Replace(strings.Join(argv, " "))
	if r := []rune(cmd); len(r) > 140 {
		cmd = string(r[:140])
	}
	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if session == "" {
		session = os.Getenv("CLAUDE_SESSION_ID")
	}
	cwd, _ := os.Getwd()
	f, err := os.OpenFile(filepath.Join(o.SlotDir, strconv.Itoa(slot)+".holder"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(holderRecord{PID: os.Getpid(), Session: session, CWD: cwd, Cmd: cmd, Max: o.Max,
		Since: time.Now().UTC().Format("2006-01-02T15:04:05Z")})
}

// scope runs argv in a transient scope under memcap.slice. systemd-run's own
// ${VAR} expansion is off (default-on for --scope since systemd 258): the
// argument list reaches the command verbatim, so a wrapped `zsh -c` keeps
// ${=files}, ${(f)x}, ${pipestatus[1]} and $$.
func scope(unit string, o Options, argv []string, logf func(string, ...any)) int {
	args := append([]string{"--user", "--scope", "--quiet", "--expand-environment=no", "--unit=" + unit,
		"--slice=memcap.slice", "-p", "MemoryMax=" + o.Max, "-p", "MemorySwapMax=" + o.Swap,
		"-p", "OOMPolicy=continue", "--"}, argv...)
	c := exec.Command("systemd-run", args...) //nolint:gosec // running the caller's command is the purpose
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
	err := c.Wait()
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

// victims names the processes the kernel killed at the scope's cap. The
// kernel log can trail the exit of the killed process by a moment.
func victims(unit string, since time.Time, rc int) []string {
	tries := 1
	if rc == 128+int(syscall.SIGKILL) {
		tries = 4
	}
	for i := range tries {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		kills, err := machine.OOMKills(ctx, since)
		cancel()
		if err != nil {
			return nil
		}
		var out []string
		for _, k := range kills {
			if strings.HasSuffix(k.Memcg, "/"+unit+".scope") {
				out = append(out, fmt.Sprintf("%d (%s) %d MiB", k.PID, k.Task, k.AnonMiB))
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

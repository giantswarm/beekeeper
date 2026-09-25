package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// hourBudget is what the replayed hour may print, in bytes. The hour
// printed 6977 bytes under v0.15.5 and 5862 now; raise this only for a line that carries new
// information.
const hourBudget = 5900

var (
	hourLine   = regexp.MustCompile(`^(\d\d:\d\d:\d\d) (.*)$`)
	alertLine  = regexp.MustCompile(`^ALERT (NEW|RESOLVED) (\S+) (\S+) (\S+) (\S+) (\S+) since (\S+ )?(\d\d:\d\d)Z(?: \[(.*)\])?$`)
	lineStamp  = regexp.MustCompile(`^\d\d:\d\d:\d\d `)
	sessionsRe = regexp.MustCompile(`^SESSIONS (started|ended|restarted)(?: \([^)]*\))?: (.*)$`)
)

// A real hour of a supervisor's watch (v0.15.5, anonymised in
// testdata/watch-hour.txt) replayed through this watch: the sessions
// through its session tracking, the alerts through their line and its
// brackets, the rest as they came. Each fact is said exactly once, and the
// hour stays under hourBudget.
func TestWatchReplayedHourSaysEachFactOnceInBudget(t *testing.T) {
	raw, err := os.ReadFile("testdata/watch-hour.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := &watcher{app: &app{out: &out}, last: map[string]time.Time{}, sessions: map[string]*claude.Session{}}
	live := map[string]*claude.Session{}
	pid := 0
	session := func(name string) *claude.Session {
		pid++
		return &claude.Session{Name: name, PID: pid, ID: name}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// the sessions the hour ends or restarts before it starts them ran before it
	seen := map[string]bool{}
	for _, l := range lines {
		if m := sessionsRe.FindStringSubmatch(hourLine.FindStringSubmatch(l)[2]); m != nil {
			for _, n := range names(m[2]) {
				if !seen[n] && m[1] != "started" {
					w.sessions[n], live[n] = session(n), nil
				}
				seen[n] = true
			}
		}
	}
	for n, s := range w.sessions {
		live[n] = s
	}
	now := time.Date(2026, 9, 25, 4, 0, 0, 0, time.UTC)
	for i := 0; i < len(lines); {
		at := hourLine.FindStringSubmatch(lines[i])[1]
		var poll, alertsNow []string
		var hints []string
		for ; i < len(lines) && strings.HasPrefix(lines[i], at); i++ {
			poll = append(poll, hourLine.FindStringSubmatch(lines[i])[2])
		}
		changed := false
		for _, l := range poll {
			if m := sessionsRe.FindStringSubmatch(l); m != nil {
				for _, n := range names(m[2]) {
					switch m[1] {
					case "started", "restarted":
						live[n] = session(n)
					case "ended":
						delete(live, n)
					}
				}
				changed = true
				continue
			}
			if m := alertLine.FindStringSubmatch(l); m != nil {
				day := "09-25 "
				if m[7] != "" {
					day = m[7]
				}
				since, err := time.Parse("01-02 15:04Z", day+m[8]+"Z")
				if err != nil {
					t.Fatal(err)
				}
				since = since.AddDate(2026, 0, 0)
				alertsNow = append(alertsNow, alerts.Line(m[1], m[2], m[3], m[4], m[5], m[6], since.Format(time.RFC3339), now))
				if m[9] != "" && !slices.Contains(hints, m[9]) {
					hints = append(hints, m[9])
				}
				continue
			}
			w.emitNow("event", "%s", l)
		}
		if changed {
			sessions := make([]*claude.Session, 0, len(live))
			for _, s := range live {
				sessions = append(sessions, s)
			}
			w.sessionChanges(sessions)
		}
		for _, l := range decorate(alertsNow, nil, func() []string { return hints }) {
			w.emitNow("alerts", "%s", l)
		}
	}

	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	facts := map[string]int{}
	for _, l := range got {
		facts[lineStamp.ReplaceAllString(l, "")]++
	}
	if len(got) != len(lines) {
		t.Errorf("the replayed hour printed %d lines for its %d facts:\n%s", len(got), len(lines), out.String())
	}
	before := len(raw)
	t.Logf("replayed hour: %d lines; %d bytes before, %d after (%d%%)", len(got), before, out.Len(), 100*out.Len()/before)
	if out.Len() > hourBudget {
		t.Errorf("the replayed hour printed %d bytes, over its budget of %d: a line grew without new information", out.Len(), hourBudget)
	}
}

func names(list string) []string {
	var out []string
	for _, n := range strings.Split(list, ", ") {
		out = append(out, strings.Trim(n, `"`))
	}
	return out
}

// A lasting condition is one line when it starts and one ENDED line when
// it ends, however many polls it lasts.
func TestWatchSaysAConditionsStartAndEndOnce(t *testing.T) {
	var out bytes.Buffer
	cfg := &config.Config{}
	cfg.Watch.Repeat.Duration = 10 * time.Minute
	w := &watcher{app: &app{out: &out, cfg: cfg}, last: map[string]time.Time{}}
	for range 40 {
		w.check("avail", true, "LOW RAM: %d MiB available, swap %d MiB", 900, 12000)
	}
	w.check("avail", false, "LOW RAM: %d MiB available, swap %d MiB", 9000, 12000)
	w.check("avail", false, "LOW RAM: %d MiB available, swap %d MiB", 9000, 12000)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "LOW RAM: 900 MiB available, swap 12000 MiB") ||
		!strings.Contains(lines[1], "ENDED LOW RAM (since ") {
		t.Errorf("a lasting condition printed:\n%s", out.String())
	}
}

// A second read with nothing changed is one "no change" line; a moving
// figure is no change, --full prints everything.
func TestSecondReadSaysNoChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\nlanes: [{name: ap, repositories: [giantswarm/klaus]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: cfg, store: store, now: time.Now(), out: &out, as: "Agent one"}
	full := func() { out.WriteString("the whole text\n") }
	read := func(age string, all bool) string {
		out.Reset()
		facts := []fact{{Key: "a", Sig: "a|x", Line: "a runs x, " + age}}
		facts = append(facts, textFacts("lane", "ap: free\nb: waiting, "+age+" left\n")...)
		if err := a.delta("test", all, facts, full); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := read("5m", false); got != "the whole text\n" {
		t.Errorf("a first read printed %q", got)
	}
	if got := read("6m", false); !strings.HasPrefix(got, "no change since ") || strings.Count(got, "\n") != 1 {
		t.Errorf("a second read printed %q", got)
	}
	if got := read("7m", true); got != "the whole text\n" {
		t.Errorf("--full printed %q", got)
	}
	for _, c := range []func() error{
		func() error { return a.agentList(false) },
		func() error {
			st, err := store.Read()
			if err != nil {
				return err
			}
			views := a.laneViews(st)
			return a.delta("lanes", false, textFacts("lane", a.capture(func() { a.printLanes(views) })), func() { a.printLanes(views) })
		},
	} {
		for i := range 2 {
			out.Reset()
			if err := c(); err != nil {
				t.Fatal(err)
			}
			if i == 1 && !strings.HasPrefix(out.String(), "no change since ") {
				t.Errorf("a second read printed %q", out.String())
			}
		}
	}
}

package free

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// cli is a live CLI of the session id, untouched for idle.
func cli(pid int, id, name string, idle time.Duration) *claude.Session {
	return &claude.Session{PID: pid, ID: id, Name: name, Cwd: "/work", MemMiB: pid, LastActive: now.Add(-idle)}
}

func TestStaleCLIOffTheRoster(t *testing.T) {
	r, out := newRun(t, Options{CLIStale: 12 * time.Hour})
	r.Sessions = []*claude.Session{cli(700, "s-old", "Old chat", 13*time.Hour), cli(800, "s-new", "Fresh chat", 11*time.Hour)}
	r.Desk = &state.State{}
	r.Do()
	for _, s := range []string{
		`pid 700        700 MiB  idle  13.0 h  "Old chat" (s-old)  roster -  role -  (/work)   <- stale, its exit returns 700 MiB`,
		"1 stale CLIs (sessions untouched for 12 h, no role, not busy or parked) hold 700 MiB",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out.String(), `"Fresh chat" (s-new)  roster -  role -  (/work)   <-`) {
		t.Errorf("a session untouched for 11 h is not stale:\n%s", out)
	}
}

func TestRolesBusyAndParkedNeverStale(t *testing.T) {
	r, out := newRun(t, Options{Summary: true, CLIStale: 12 * time.Hour})
	const task = "a task"
	long := 100 * time.Hour
	parked := cli(5, "s-parked", "Parked", long)
	parked.Waiting = &claude.Waiting{}
	r.Sessions = []*claude.Session{
		cli(1, "s-sup", "Supervisor run 9", long),
		cli(2, "s-guide", "Guide run 3", long),
		cli(3, "s-spare", "Supervisor run 10", long),
		cli(4, "s-busy", "Agent busy", long),
		parked,
		cli(6, "s-idle", "Agent idle", 13*time.Hour),
		cli(7, "s-done", "Agent done", 13*time.Hour),
		cli(8, "s-gspare", "Guide run 4", long),
	}
	r.Desk = &state.State{
		Supervisor: &state.Supervisor{Party: state.Party{Session: "s-sup"}},
		Relay:      &state.Relay{To: state.Party{Session: "s-spare"}, Expires: now.Add(time.Minute)},
		Guide: &state.Role{
			Holder: &state.Supervisor{Party: state.Party{Session: "s-guide"}},
			Relay:  &state.Relay{To: state.Party{Session: "s-gspare"}, Expires: now.Add(time.Minute)},
		},
		Agents: []state.Agent{
			{Party: state.Party{Session: "s-busy", Name: "Agent thirty"}, Task: task},
			{Party: state.Party{Session: "s-parked"}, Task: task},
			{Party: state.Party{Session: "s-idle"}},
			{Party: state.Party{Session: "s-done"}, Task: task, Done: true},
		},
	}
	r.Do()
	r.Summary = false
	report := &strings.Builder{}
	r.Out = report
	r.clis()
	if s := `"Agent busy" (s-busy)  roster busy as "Agent thirty"  role -`; !strings.Contains(report.String(), s) {
		t.Errorf("report lacks %q:\n%s", s, report)
	}
	if strings.Count(report.String(), "<- stale") != 2 {
		t.Errorf("only the idle and the done roster agents are stale:\n%s", report)
	}
	want := strings.Join([]string{
		"cli\t1\t1\t6000\t/work\ts-sup\tSupervisor run 9\t-\tsupervisor\t100\t-",
		"cli\t2\t2\t6000\t/work\ts-guide\tGuide run 3\t-\tguide\t100\t-",
		"cli\t3\t3\t6000\t/work\ts-spare\tSupervisor run 10\t-\tspare\t100\t-",
		"cli\t4\t4\t6000\t/work\ts-busy\tAgent busy\tbusy\t-\t100\t-",
		"cli\t5\t5\t6000\t/work\ts-parked\tParked\tparked\t-\t100\t-",
		"cli\t6\t6\t780\t/work\ts-idle\tAgent idle\tidle\t-\t13\tstale",
		"cli\t7\t7\t780\t/work\ts-done\tAgent done\tidle\t-\t13\tstale",
		"cli\t8\t8\t6000\t/work\ts-gspare\tGuide run 4\t-\tspare\t100\t-",
	}, "\n") + "\n"
	if out.String() != want {
		t.Errorf("summary:\n%s\nwant:\n%s", out, want)
	}
}

func TestExpiredRelayIsNoSpare(t *testing.T) {
	r, out := newRun(t, Options{Summary: true, CLIStale: 12 * time.Hour})
	r.Sessions = []*claude.Session{cli(3, "s-spare", "Supervisor run 10", 20*time.Hour)}
	r.Desk = &state.State{Relay: &state.Relay{To: state.Party{Session: "s-spare"}, Expires: now.Add(-time.Minute)}}
	r.Do()
	if want := "cli\t3\t3\t1200\t/work\ts-spare\tSupervisor run 10\t-\t-\t20\tstale\n"; out.String() != want {
		t.Errorf("summary:\n%s\nwant:\n%s", out, want)
	}
}

func TestApplyStopsNoCLI(t *testing.T) {
	r, out := newRun(t, Options{Apply: true, CLIStale: 12 * time.Hour})
	r.Sessions = []*claude.Session{cli(700, "s-old", "Old chat", 13*time.Hour)}
	r.kill = func(pids []int) { t.Errorf("free --apply killed %v", pids) }
	r.Do()
	if !strings.Contains(out.String(), "<- stale, its exit returns 700 MiB") {
		t.Errorf("the stale CLI is reported:\n%s", out)
	}
}

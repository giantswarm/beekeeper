package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestRelayKeepsRunNameAfterDesktopCLIRestarts(t *testing.T) {
	for _, title := range []string{mailGuide, claimSupervisor} {
		t.Run(title, func(t *testing.T) {
			rl := guideRole
			rl.Title = title
			out := state.Party{Session: "outgoing-run", Name: rl.RunName(79)}
			next := state.Party{Session: "next-run", HostSession: "local_next-run", Name: rl.RunName(80)}
			st := &state.State{}
			rl.Set(st, state.Role{Holder: &state.Supervisor{Party: out, Since: relayNow.Add(-time.Hour)}, Run: 79})
			if _, _, err := rl.Relay(st, out, next, relayNow, time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, _, err := rl.Start(st, next, true, false, relayNow); err != nil {
				t.Fatal(err)
			}
			for i, name := range []string{next.Name, "workspace-12", "", "workspace-13"} {
				sessions := []*claude.Session{{PID: 100 + i, ID: next.Session, HostID: next.HostSession, Name: name}}
				_, evs := rl.ObserveCLI(st, sessions, relayNow.Add(time.Duration(i)*time.Second))
				r := rl.Get(st)
				if r.Holder.Name != next.Name || r.CLI.Supervisor.Name != next.Name {
					t.Fatalf("restart lost run name: %+v", r)
				}
				for _, ev := range evs {
					if strings.HasSuffix(ev.Verb, ".rename") {
						t.Fatalf("restart renamed holder: %+v", ev)
					}
				}
			}
		})
	}
}

func TestTitleRunRestoresAnEmptyDesktopTitle(t *testing.T) {
	a, _ := supervisorApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	desktopRecord(t, a.cfg.Claude.DesktopDir, "local_desktop-run", "desktop-run", false)
	// No desktop steward is available: an empty title must still attempt
	// restoration and report the failure instead of silently skipping it.
	line := a.titleRun(context.Background(), state.Party{Session: "desktop-run", Name: "Guide run 80"}, "Guide run 80")
	if !strings.Contains(line, "the desktop recorded no title") {
		t.Fatalf("empty title was not restored: %q", line)
	}
}

// A background role run has no desktop record, so nothing to restore.
func TestTitleRunSkipsASessionWithoutADesktopRecord(t *testing.T) {
	a, _ := supervisorApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	if line := a.titleRun(context.Background(), state.Party{Session: "background-run", Name: "Guide run 81"}, "Guide run 81"); line != "" {
		t.Fatalf("a session without a desktop record was retitled: %q", line)
	}
}

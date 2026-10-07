package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	runName  = "Supervisor run 82"
	runTitle = "klaus-lab-14"
	runID    = "sup-82"
)

// A supervisor its desktop titled differently after the relay keeps its
// run name on the roster: a message by the run name addresses the role and
// reaches the holder's running CLI by its title, where the run name alone
// would be refused as an agent with no CLI.
func TestMessageByRunNameReachesTheRenamedSupervisor(t *testing.T) {
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	run := state.Party{Session: runID, HostSession: "local_sup82", Name: runName}
	holder := run
	holder.Name = runTitle
	st := &state.State{Agents: []state.Agent{{Party: run, Registered: now}, {Party: state.Party{Session: "w1", Name: "w1 worker"}}}}
	supervisorRole.set(st, state.Role{Holder: &state.Supervisor{Party: holder, Since: now}})
	sessions := []*claude.Session{{ID: runID, Name: runTitle, PID: 4242}}

	if r := absentAgent(st, sessions, runName, now); !strings.Contains(r, "no CLI") {
		t.Fatalf("the run name alone reads as an agent with no CLI: %q", r)
	}
	for _, name := range []string{runName, "supervisor RUN 82", runTitle, runID, "local_sup82"} {
		if got := holderRole(st, name); got != supervisorRole.name {
			t.Errorf("%q: role %q, want the supervisor", name, got)
		}
	}
	for _, name := range []string{"w1 worker", "Supervisor run 81", ""} {
		if got := holderRole(st, name); got != "" {
			t.Errorf("%q addresses no holder, got %q", name, got)
		}
	}
	to, err := roleAddress(supervisorRole.get(st).Holder, supervisorRole, sessions)
	if err != nil || to != runTitle {
		t.Fatalf("address %q, %v; want the running CLI's title", to, err)
	}
}

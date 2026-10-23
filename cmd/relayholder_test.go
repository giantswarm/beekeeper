package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// After a relay, supervisor status names the run that took the role, never
// the relieved one, also once the relieved run's CLI has ended; agents wake
// addresses the holder, and the relieved run is no longer on the roster to
// address. The relieved run's own status says it was relieved, naming its
// successor.
func TestStatusAndWakeNameTheHolderAfterARelay(t *testing.T) {
	const relieved, successor = "Supervisor run 11", "Supervisor run 12"
	a, out := stubApp(t)
	from := state.Party{Session: "s3", Name: relieved}
	to := state.Party{Session: "s9", Name: successor}
	worker := state.Party{Session: "s5", Name: agentFour}
	runLive(t, a, map[int]string{3: relieved, 9: successor, 5: agentFour})
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: from, Since: a.now.Add(-time.Hour)}
		st.SupervisorRun = 11
		st.Agents = []state.Agent{{Party: from}, {Party: to}, {Party: worker}}
		if _, _, err := supervisorRole.relay(st, from, to, a.now, 15*time.Minute); err != nil {
			return nil, err
		}
		_, evs, err := supervisorRole.start(st, to, true, false, a.now.Add(time.Minute))
		return evs, err
	}); err != nil {
		t.Fatal(err)
	}

	status := func(when string) {
		t.Helper()
		out.Reset()
		if err := a.roleStatus(supervisorRole); err != nil {
			t.Fatalf("status %s: %v", when, err)
		}
		if s := out.String(); !strings.HasPrefix(s, `"`+successor+`" supervises`) || strings.Contains(s, relieved) {
			t.Errorf("status %s: %q, want %q and never %q", when, s, successor, relieved)
		}
	}
	status("after the relay")
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findAgent(st, relieved); Code(err) != ExitRefused {
		t.Errorf("wake still addresses the relieved run: %v", err)
	}
	if i, err := findAgent(st, "supervisor"); err != nil || !st.Agents[i].Is(to) {
		t.Errorf("wake of the supervisor: %d, %v; want %q", i, err, successor)
	}

	// The relieved run's CLI ends: it leaves the running sessions, and the
	// holder stays its successor.
	runLive(t, a, map[int]string{9: successor, 5: agentFour})
	status("once the relieved run's CLI ended")

	a.as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", from.Session)
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	if err := a.roleStatus(supervisorRole); Code(err) != ExitRelieved || !strings.Contains(err.Error(), `"`+successor+`" took the role`) {
		t.Errorf("the relieved run's status: %v, want exit %d naming %q", err, ExitRelieved, successor)
	}
}

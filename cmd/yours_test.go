package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// The supervisor of the yours tests, and the worker it grants to.
var (
	yoursSup = state.Party{Session: "s0", Name: "Supervisor run 481"}
	yoursOne = state.Party{Session: "s1", HostSession: twinHost, Name: agentOne}
)

// The supervisor's `yours <resource>` to a roster agent records the grant
// lease grant would, says so once more when it is granted already, and
// names the command when the target is nobody.
func TestYoursGrantsRecordTheSupervisorsWord(t *testing.T) {
	var out bytes.Buffer
	a := quietApp(t, &out)
	a.cfg.Resources = []string{uiLab1}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: yoursSup, Since: a.now}
		st.Agents = append(st.Agents, state.Agent{Party: yoursOne, Registered: a.now, Task: "the proof"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	got := a.yoursGrants(nil, yoursSup, agentOne, []string{uiLab1, browser})
	for _, want := range []string{"beekeeper: your `yours kind-1` is recorded: granted kind-1 to \"Agent one\"", "\nyour `yours browser` is recorded: granted browser to \"Agent one\"", "no CLI of \"Agent one\" runs"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Grants) != 2 || !st.Grants[0].To.Is(yoursOne) || !st.Grants[0].By.Is(yoursSup) || st.Grants[0].Resource != uiLab1 || st.Grants[1].Resource != browser {
		t.Fatalf("grants %+v", st.Grants)
	}
	if got := a.yoursGrants(nil, yoursSup, yoursOne.HostSession, []string{uiLab1}); !strings.Contains(got, "kind-1 is already granted to \"Agent one\" (number 1 in its queue)") {
		t.Errorf("a second yours: %q", got)
	}
	got = a.yoursGrants(nil, yoursSup, "Agent nine", []string{uiLab1})
	if !strings.Contains(got, "your `yours kind-1` recorded no grant: ") || !strings.Contains(got, "`beekeeper lease grant kind-1 <session>`") {
		t.Errorf("a yours to nobody: %q", got)
	}
	if st, _ = a.store.Read(); len(st.Grants) != 2 {
		t.Fatalf("a yours to nobody recorded a grant: %+v", st.Grants)
	}
}

// The hook records nothing for a message from a session that does not hold
// the supervisor role, or that names no resource.
func TestYoursGrantIsTheSupervisorsAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("stateDir: "+dir+"\nresources: ["+uiLab1+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	var out bytes.Buffer
	a := &app{cfgPath: cfg, out: &out}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: yoursSup, Since: time.Now()}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := a.yoursGrant(yoursOne.Session, agentOne, "kind-1 yours"); got != "" {
		t.Errorf("a worker's yours: %q", got)
	}
	if got := a.yoursGrant(yoursSup.Session, agentOne, "merged 12 v1.2.0"); got != "" {
		t.Errorf("no resource named: %q", got)
	}
	if st, _ := store.Read(); len(st.Grants) != 0 {
		t.Fatalf("grants %+v", st.Grants)
	}
}

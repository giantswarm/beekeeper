package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// priorityApp is an app of a desk whose person is Timo, its board o/7.
func priorityApp(t *testing.T, as string) (*app, state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, Board: config.Board{Owner: "o", Project: 7}, Guide: config.Guide{Person: "Timo"}}
	return &app{cfg: cfg, store: store, out: &bytes.Buffer{}, as: as}, store
}

// Only the person changes priorities: a call without --as, the supervisor's
// or a worker's included, and one naming anybody else are refused with the
// rule before GitHub is asked anything.
func TestPrioritySetIsThePersonsOnly(t *testing.T) {
	asked := 0
	defer func(gh func(context.Context, ...string) ([]byte, error)) { boardGH = gh }(boardGH)
	boardGH = func(context.Context, ...string) ([]byte, error) {
		asked++
		return nil, nil
	}
	sup := state.Party{Session: "sup", Name: "Supervisor run 1"}
	worker := state.Party{Session: "w1", Name: "Worker one"}
	for _, tc := range []struct {
		name, as, session, want string
	}{
		{"no --as outside a session", "", "", "a call without --as: only the person changes priorities; the guide relays the person's word with --as Timo"},
		{"the supervisor without --as", "", "sup", "the supervisor without --as: only the person changes priorities"},
		{"a worker without --as", "", "w1", "the worker without --as: only the person changes priorities"},
		{"another name", "supervisor", "sup", `--as "supervisor" is no configured person (Timo): only the person changes priorities`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.session)
			t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
			a, store := priorityApp(t, tc.as)
			if err := store.Update(func(st *state.State) ([]state.Event, error) {
				st.SetSupervisorRole(state.Role{Holder: &state.Supervisor{Party: sup}})
				st.Agents = append(st.Agents, state.Agent{Party: worker, Task: "o/r#1"})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"o/r#1", "1"}, {"o/r#1"}} {
				c := a.prioritySetCmd()
				if len(args) == 1 {
					c = a.priorityUnsetCmd()
				}
				c.SetArgs(args)
				c.SetOut(&bytes.Buffer{})
				c.SetErr(&bytes.Buffer{})
				c.SilenceUsage, c.SilenceErrors = true, true
				err := c.Execute()
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s %v = exit %d %v; want exit %d with %q", c.Name(), args, Code(err), err, ExitRefused, tc.want)
				}
			}
		})
	}
	if asked != 0 {
		t.Errorf("GitHub asked %d times before the refusal", asked)
	}
}

// The person's word, relayed by --as, is taken whatever the case, and the
// configured persons are guide.person, identity.person and serve.people's
// names, each once.
func TestPriorityPersonTakesTheConfiguredPerson(t *testing.T) {
	a, _ := priorityApp(t, "timo")
	a.cfg.Identity.Person = "timo@example.com"
	a.cfg.Serve.People = map[string]string{"Timo": "timo@example.com", "pat": "pat@example.com"}
	if p, err := a.priorityPerson(); err != nil || p != "Timo" {
		t.Errorf("priorityPerson = %q, %v; want Timo", p, err)
	}
	if got := strings.Join(a.persons(), ", "); got != "Timo, timo@example.com, pat" {
		t.Errorf("persons = %s", got)
	}
}

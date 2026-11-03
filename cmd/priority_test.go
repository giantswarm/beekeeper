package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// supSession is the session of the supervisor the priority tests refuse.
const supSession = "prio-sup"

// priorityApp is an app of a desk whose person is Timo, its board o/7.
func priorityApp(t *testing.T, as string) (*app, state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, Board: config.Board{Owner: "o", Project: 7}, Guide: config.Guide{Person: personTimo}}
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
	sup := state.Party{Session: supSession, Name: firstRun}
	worker := state.Party{Session: "w1", Name: "Worker one"}
	for _, tc := range []struct {
		name, as, session, want string
	}{
		{"no --as outside a session", "", "", "a call without --as: only the person changes priorities; the guide relays the person's word with --as Timo"},
		{"the guide without --as", "", supSession, "the guide without --as: only the person changes priorities"},
		{"a worker without --as", "", "w1", "the worker without --as: only the person changes priorities"},
		{"another name", supervisorName, supSession, `--as "supervisor" is no configured person (Timo): only the person changes priorities`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.session)
			t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
			a, store := priorityApp(t, tc.as)
			if err := store.Update(func(st *state.State) ([]state.Event, error) {
				guideRole.set(st, state.Role{Holder: &state.Supervisor{Party: sup}})
				st.Agents = append(st.Agents, state.Agent{Party: worker, Task: refOne})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{refOne, "1"}, {refOne}} {
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
	a, _ := priorityApp(t, strings.ToLower(personTimo))
	a.cfg.Identity.Person = "timo@example.com"
	a.cfg.Serve.People = map[string]string{personTimo: "timo@example.com", pat: "pat@example.com"}
	if p, err := a.priorityPerson(); err != nil || p != personTimo {
		t.Errorf("priorityPerson = %q, %v; want Timo", p, err)
	}
	if got := strings.Join(a.persons(), ", "); got != "Timo, timo@example.com, Pat" {
		t.Errorf("persons = %s", got)
	}
}

// Position edits stay available to workers, but cannot mutate Priority numbers.
func TestBoardRankSplitLogsCallerAndWritesOnlyPosition(t *testing.T) {
	defer func(gh func(context.Context, ...string) ([]byte, error)) { boardGH = gh }(boardGH)
	a, store := priorityApp(t, "worker-script")
	writes := 0
	boardGH = func(_ context.Context, args ...string) ([]byte, error) {
		q := args[3]
		switch {
		case strings.Contains(q, "fields(first:50)"):
			return []byte(`{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"FS","name":"Status","options":[{"id":"s1","name":"Backlog"}]}]}}}}}`), nil
		case strings.Contains(q, "projectItems"):
			return []byte(`{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[{"id":"I1","project":{"id":"P"}}]}}}}}`), nil
		case strings.Contains(q, "updateProjectV2ItemPosition"):
			writes++
			return []byte(`{"data":{"updateProjectV2ItemPosition":{"clientMutationId":null}}}`), nil
		default:
			t.Fatalf("unexpected GitHub call, including any Priority write: %s", q)
			return nil, nil
		}
	}
	c := a.boardRankCmd()
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetArgs([]string{refOne, "--top"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(0, func(e state.Event) bool { return e.Verb == "board.rank" })
	if err != nil || writes != 1 || len(events) != 1 || events[0].By.Name != "worker-script" {
		t.Fatalf("writes=%d events=%+v error=%v", writes, events, err)
	}
}

func TestBoardRankSplitRefusesAnonymousWrites(t *testing.T) {
	for _, key := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_CODE_SESSION_NAME", "BEEKEEPER_OMP_AGENT", "BEEKEEPER_AGY_AGENT", "BEEKEEPER_CODEX_AGENT"} {
		t.Setenv(key, "")
	}
	defer func(gh func(context.Context, ...string) ([]byte, error)) { boardGH = gh }(boardGH)
	boardGH = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("anonymous rank reached GitHub")
		return nil, nil
	}
	a, _ := priorityApp(t, "")
	c := a.boardRankCmd()
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetArgs([]string{refOne, "--top"})
	err := c.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "identified caller") {
		t.Fatalf("rank = exit %d, %v", Code(err), err)
	}
}

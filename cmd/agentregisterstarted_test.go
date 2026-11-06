package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestStartedWorkerRegistersWithoutChangingItsEntry(t *testing.T) {
	const startedName = "started harness worker"
	for _, h := range []struct{ name, env, prefix string }{
		{agy.Harness, agy.EnvAgent, agy.HostPrefix},
		{codex.Harness, codex.EnvAgent, codex.HostPrefix},
		{omp.Harness, omp.EnvAgent, omp.HostPrefix},
	} {
		t.Run(h.name, func(t *testing.T) {
			for _, env := range []string{agy.EnvAgent, codex.EnvAgent, omp.EnvAgent} {
				t.Setenv(env, "")
			}
			t.Setenv(h.env, "started-worker")
			t.Setenv(agy.EnvName, startedName)
			t.Setenv("CLAUDE_CODE_SESSION_ID", "unrelated-session")
			dir := t.TempDir()
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			original := state.Agent{Party: state.Party{HostSession: h.prefix + "started-worker", Name: startedName}, Harness: h.name, Task: "finish task", Registered: now, AssignedAt: now}
			if err := store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{original}; return nil, nil }); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a := &app{cfg: &config.Config{StateDir: dir}, store: store, now: now, out: &out}
			c := a.agentsCmd()
			c.SetArgs([]string{registerVerb})
			c.SetOut(&out)
			c.SetErr(&out)
			if err := c.Execute(); err != nil {
				t.Fatalf("register: %v", err)
			}
			if !strings.Contains(out.String(), "already registered as "+startedName) {
				t.Fatalf("output: %s", out.String())
			}
			st, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Agents) != 1 || !reflect.DeepEqual(st.Agents[0], original) {
				t.Fatalf("registration changed entry: %+v", st.Agents)
			}
			c = a.agentsCmd()
			c.SetArgs([]string{"park", "waiting for dependency"})
			c.SetOut(&out)
			c.SetErr(&out)
			if err := c.Execute(); err != nil {
				t.Fatalf("park: %v", err)
			}
			st, err = store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Agents) != 1 || st.Agents[0].Park == nil || st.Agents[0].Task != original.Task {
				t.Fatalf("park lost started identity: %+v", st.Agents)
			}
		})
	}
}

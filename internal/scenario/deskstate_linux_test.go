//go:build scenario && linux

package scenario

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// A generated desk holds what its sizes say, at rest: idle workers, ended
// serve records and deliveries in flight in the scenario's repository.
func TestGenerateState(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	gen := Generate{Repository: "example/app", Agents: 3, Records: 5, Deliveries: 4, Reports: 2, Events: 7}
	var st state.State
	generateState(&st, now, gen)
	if len(st.Agents) != 3 || len(st.Starts) != 3 || len(st.Records) != 5 || len(st.Deliveries) != 4 || len(st.WorkerReports) != 2 {
		t.Fatalf("sizes: %d agents, %d starts, %d records, %d deliveries, %d reports", len(st.Agents), len(st.Starts), len(st.Records), len(st.Deliveries), len(st.WorkerReports))
	}
	for _, ag := range st.Agents {
		if ag.Task != "" || ag.Done || ag.IdleSince.IsZero() || ag.Park != nil {
			t.Fatalf("a generated worker is not idle: %+v", ag)
		}
	}
	for _, r := range st.Records {
		if r.Ended.IsZero() || !strings.HasPrefix(r.Issue, "example/app#") {
			t.Fatalf("a generated serve record: %+v", r)
		}
	}
	for _, d := range st.Deliveries {
		if !d.Active() || d.OutsideDesk || len(d.Pulls) != 1 || !strings.HasPrefix(d.Ref, "example/app#") {
			t.Fatalf("a generated delivery is not in flight on the desk: %+v", d)
		}
	}
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if err := generateEvents(log, now, gen); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); n++ {
		var e state.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Verb == "" || e.At.After(now) {
			t.Fatalf("event %d: %v: %s", n, err, sc.Text())
		}
	}
	if n != 7 {
		t.Fatalf("%d events, want 7", n)
	}
}

func TestStateValidate(t *testing.T) {
	repos := []string{"example/app"}
	if err := (&State{Generate: &Generate{Agents: 1}}).validate(repos); err != nil {
		t.Fatal(err)
	}
	if err := (*State)(nil).validate(nil); err != nil {
		t.Fatal(err)
	}
	if err := (&State{}).validate(repos); err == nil || !strings.Contains(err.Error(), "state without generate") {
		t.Fatalf("a state block that seeds nothing: %v", err)
	}
	for _, tc := range []struct {
		name  string
		gen   Generate
		repos []string
		want  string
	}{
		{"nothing", Generate{}, repos, "nothing to generate"},
		{"negative", Generate{Agents: -1}, repos, "agents: -1 is not within"},
		{"too many events", Generate{Events: maxGeneratedEvents + 1}, repos, "events:"},
		{"no repository", Generate{Agents: 1}, nil, "which has none"},
		{"another repository", Generate{Agents: 1, Repository: "example/other"}, repos, `"example/other" is none of the scenario's repositories`},
	} {
		err := (&State{Generate: &tc.gen}).validate(tc.repos)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

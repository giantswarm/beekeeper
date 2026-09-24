package merge

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const backstage = "giantswarm/backstage"

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		argv string
		repo string
		pr   int
	}{
		{"devctl pr merge " + backstage + " 12 --timeout 9m", backstage, 12},
		{"devctl pr merge --timeout 9m giantswarm/marge 3", "giantswarm/marge", 3},
		{"devctl pr merge giantswarm/marge --help", "", 0},
		{"devctl pr merge giantswarm/marge", "", 0},
		{"devctl pr wait giantswarm/marge 3", "", 0},
	} {
		repo, pr, ok := ParseArgs(strings.Fields(c.argv))
		if repo != c.repo || pr != c.pr || ok != (c.pr > 0) {
			t.Errorf("%s: got %s %d %v", c.argv, repo, pr, ok)
		}
	}
}

func TestParseDocument(t *testing.T) {
	o, ok := ParseDocument([]byte(`{"exitCode":0,"mergeCommitSha":"abc","release":{"verdict":"available","result":{"tag":"v1.2.3"}}}`))
	if !ok || !o.Merged || o.Release != "v1.2.3" || o.NoRelease {
		t.Errorf("released: %+v %v", o, ok)
	}
	o, _ = ParseDocument([]byte(`{"exitCode":0,"mergeCommitSha":"abc","release":{"verdict":"no_release"}}`))
	if !o.Merged || !o.NoRelease {
		t.Errorf("no release: %+v", o)
	}
	o, _ = ParseDocument([]byte(`{"exitCode":3,"mergeCommitSha":"","release":null}`))
	if o.Merged {
		t.Errorf("refused: %+v", o)
	}
	if _, ok := ParseDocument([]byte("not json")); ok {
		t.Error("garbage parsed")
	}
}

func TestBlocking(t *testing.T) {
	now := time.Now()
	st := &state.State{Holds: []state.Hold{
		{Target: "lane:serving", Reason: "model load"},
		{Target: "giantswarm/old", Until: now.Add(-time.Minute)},
	}}
	if h, ok := Blocking(st, now, "giantswarm/model-manager", "serving"); !ok || h.Reason != "model load" {
		t.Error("the lane hold does not stop its lane")
	}
	if _, ok := Blocking(st, now, backstage, "portal-tools"); ok {
		t.Error("a lane hold stops another lane")
	}
	if _, ok := Blocking(st, now, "giantswarm/old", "giantswarm/old"); ok {
		t.Error("an expired hold stops a merge")
	}
	st.Holds = append(st.Holds, state.Hold{Target: AllMerges, Except: ToolRepo})
	if _, ok := Blocking(st, now, backstage, "portal-tools"); !ok {
		t.Error("the tool-release window lets another merge through")
	}
	if _, ok := Blocking(st, now, ToolRepo, ToolRepo); ok {
		t.Error("the tool-release window stops the tool's own release")
	}
}

func TestQueueAndPrune(t *testing.T) {
	now := time.Now()
	st := &state.State{Merges: []state.Merge{
		{Repo: "o/b", PR: 2, Lane: "l", PID: 2, Phase: state.Waiting, Joined: now.Add(-time.Minute), Seen: now},
		{Repo: "o/a", PR: 1, Lane: "l", PID: 1, Phase: state.Waiting, Joined: now.Add(-2 * time.Minute), Seen: now},
		{Repo: "o/c", PR: 3, Lane: "l", PID: 3, Phase: state.Waiting, Joined: now, Seen: now.Add(-time.Hour)},
		{Repo: "o/d", PR: 4, Lane: "m", PID: 4, Phase: state.Running, Release: "v1", Roll: []string{"x"}},
	}}
	Prune(st, now, 15*time.Minute, func(pid int) bool { return pid == 1 })
	q := Queue(st, "l")
	if len(q.Waiting) != 2 || q.Position("o/a", 1) != 1 || q.Position("o/b", 2) != 2 {
		t.Errorf("queue order: %+v", q.Waiting)
	}
	m := Queue(st, "m")
	if m.Running != nil || m.Settling == nil || m.Settling.Release != "" || m.Settling.Exit != -1 {
		t.Errorf("a lost run does not settle with an unknown release: %+v", m)
	}
}

const hrJSON = `{"items":[
 {"metadata":{"namespace":"flux","name":"backstage"},"status":{"history":[{"chartName":"backstage","chartVersion":"2.66.1+d293"}],"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"namespace":"demo","name":"backstage"},"status":{"history":[{"chartName":"backstage","chartVersion":"2.60.0+aaaa"}],"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"namespace":"flux","name":"marge"},"status":{"history":[{"chartName":"marge","chartVersion":"0.38.11"}],"conditions":[{"type":"Ready","status":"False","message":"upgrade retries exhausted"}]}},
 {"metadata":{"namespace":"flux","name":"other"},"status":{"history":[{"chartName":"other","chartVersion":"1.0.0"}],"conditions":[{"type":"Ready","status":"False"}]}}
]}`

func TestReady(t *testing.T) {
	hrs, err := ParseHelmReleases([]byte(hrJSON))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lane := config.Lane{Name: "portal", Repositories: []string{backstage, "giantswarm/marge"}, Installation: "gazelle"}
	if ok, why := Ready(lane, hrs, nil, now, time.Minute); ok || !strings.Contains(why, "flux/marge is not Ready: upgrade retries exhausted") {
		t.Errorf("a lane HelmRelease not Ready: %v %q", ok, why)
	}
	hrs[2].Ready = true
	if ok, why := Ready(lane, hrs, nil, now, time.Minute); !ok {
		t.Errorf("an unrelated HelmRelease blocks the lane: %q", why)
	}
	roll := RollSet(hrs, backstage)
	if len(roll) != 1 || roll[0] != "flux/backstage" {
		t.Fatalf("roll set: %v (the pinned demo/backstage is not waited for)", roll)
	}
	s := &state.Merge{Repo: backstage, PR: 9, Release: "v2.66.2", Roll: roll, Finished: now}
	if ok, why := Ready(lane, hrs, s, now, time.Minute); ok || !strings.Contains(why, "rolling to 2.66.2") {
		t.Errorf("Ready on the old version frees the lane: %v %q", ok, why)
	}
	hrs[0].Version = "2.66.2"
	if ok, why := Ready(lane, hrs, s, now, time.Minute); !ok {
		t.Errorf("the rolled release does not free the lane: %q", why)
	}
	s = &state.Merge{Repo: backstage, PR: 9, Finished: now.Add(-30 * time.Second)}
	if ok, _ := Ready(lane, hrs, s, now, time.Minute); ok {
		t.Error("an unknown release frees the lane before the settle time")
	}
	if ok, _ := Ready(lane, hrs, s, now.Add(time.Minute), time.Minute); !ok {
		t.Error("an unknown release keeps the lane after the settle time")
	}
}

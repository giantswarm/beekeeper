package merge

import (
	"embed"
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
	for _, c := range []struct {
		name string
		doc  []byte
		want Outcome
	}{
		// testdata/pr-merge.json is the example under "The document" in
		// giantswarm/devctl's docs/pr-merge.md, its two elided array
		// elements dropped so it parses.
		{"documented example", testdata(t, "pr-merge.json"), Outcome{Merged: true, Release: "v8.91.0"}},
		// testdata/pr-merge-no-release.json is a real document, the merge of
		// giantswarm/github#6335.
		{"real no release", testdata(t, "pr-merge-no-release.json"), Outcome{Merged: true, NoRelease: true}},
		{"no release", []byte(`{"exitCode":0,"mergeCommitSha":"abc","release":{"verdict":"no_release","tag":""}}`), Outcome{Merged: true, NoRelease: true}},
		{"unconfirmed", []byte(`{"exitCode":9,"mergeCommitSha":"abc","release":{"verdict":"timeout","tag":"v1.2.4"}}`), Outcome{Merged: true}},
		{"nothing merged", []byte(`{"exitCode":3,"mergeCommitSha":"","release":null}`), Outcome{}},
	} {
		if o, ok := ParseDocument(c.doc); !ok || o != c.want {
			t.Errorf("%s: %+v %v, want %+v", c.name, o, ok, c.want)
		}
	}
	if _, ok := ParseDocument([]byte("not json")); ok {
		t.Error("garbage parsed")
	}
}

//go:embed testdata/*.json testdata/*.jsonl
var testdataFS embed.FS

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := testdataFS.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBlocking(t *testing.T) {
	now := time.Now()
	st := &state.State{Holds: []state.Hold{
		{Target: "lane:serving", Reason: "model load"},
		{Target: "giantswarm/old", Until: now.Add(-time.Minute)},
	}}
	if h, ok := Blocking(st, now, "giantswarm/model-manager", 7, "serving"); !ok || h.Reason != "model load" {
		t.Error("the lane hold does not stop its lane")
	}
	if _, ok := Blocking(st, now, backstage, 8, "portal-tools"); ok {
		t.Error("a lane hold stops another lane")
	}
	if _, ok := Blocking(st, now, "giantswarm/old", 1, "giantswarm/old"); ok {
		t.Error("an expired hold stops a merge")
	}
	st.Holds = append(st.Holds, state.Hold{Target: AllMerges, Except: ToolRepo})
	if _, ok := Blocking(st, now, backstage, 8, "portal-tools"); !ok {
		t.Error("the tool-release window lets another merge through")
	}
	if _, ok := Blocking(st, now, ToolRepo, 9, ToolRepo); ok {
		t.Error("the tool-release window stops the tool's own release")
	}
}

func TestBlockingExcept(t *testing.T) {
	now := time.Now()
	st := &state.State{Holds: []state.Hold{{Target: "lane:serving", Except: "giantswarm/model-manager#172", Reason: "its release"}}}
	if _, ok := Blocking(st, now, "giantswarm/model-manager", 172, "serving"); ok {
		t.Error("the lane hold stops the pull request it excepts")
	}
	for _, c := range []struct {
		repo string
		pr   int
	}{{"giantswarm/model-manager", 180}, {"giantswarm/cluster-manager", 172}} {
		if _, ok := Blocking(st, now, c.repo, c.pr, "serving"); !ok {
			t.Errorf("the lane hold lets %s#%d through", c.repo, c.pr)
		}
	}
	st.Holds[0].Except = "giantswarm/model-manager"
	if _, ok := Blocking(st, now, "giantswarm/model-manager", 180, "serving"); ok {
		t.Error("the lane hold stops the repository it excepts")
	}
}

func TestQueueAndPrune(t *testing.T) {
	now := time.Now()
	st := &state.State{Merges: []state.Merge{
		{Repo: "o/b", PR: 2, Lane: "l", PID: 2, Phase: state.Waiting, Joined: now.Add(-time.Minute), Seen: now},
		{Repo: "o/a", PR: 1, Lane: "l", PID: 1, Phase: state.Waiting, Joined: now.Add(-2 * time.Minute), Seen: now},
		{Repo: "o/c", PR: 3, Lane: "l", PID: 3, Phase: state.Waiting, Joined: now, Seen: now.Add(-time.Hour)},
		{Repo: "o/d", PR: 4, Lane: "m", PID: 4, Phase: state.Running, Release: "v1", Roll: []string{"x"}},
		{Repo: "o/e", PR: 5, Lane: "l", Phase: state.Waiting, Seeded: true, Joined: now, Seen: now.Add(-time.Hour)},
	}}
	Prune(st, now, 15*time.Minute, 12*time.Hour, func(pid int) bool { return pid == 1 })
	q := Queue(st, "l")
	if len(q.Waiting) != 3 || q.Position("o/a", 1) != 1 || q.Position("o/b", 2) != 2 || q.Position("o/e", 5) != 3 {
		t.Errorf("queue order: %+v", q.Waiting)
	}
	m := Queue(st, "m")
	if m.Running != nil || m.Settling == nil || m.Settling.Release != "" || m.Settling.Exit != -1 {
		t.Errorf("a lost run does not settle with an unknown release: %+v", m)
	}
	st.Merges = append(st.Merges, state.Merge{Repo: "o/f", PR: 6, Lane: "l", Phase: state.Waiting, Seeded: true, Outside: true, Joined: now, Seen: now})
	Prune(st, now, 15*time.Minute, 12*time.Hour, func(pid int) bool { return pid == 1 })
	if q := Queue(st, "l"); q.Position("o/f", 6) != 1 || q.Position("o/a", 1) != 2 {
		t.Errorf("a settled outside merge does not head its lane: %+v", q.Waiting)
	}
}

func TestMerged(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	earlier := state.Merge{Repo: "o/a", PR: 1, Lane: "l", Phase: state.Settling, Release: "v1", Finished: at.Add(-time.Hour)}
	m := state.Merge{Repo: "o/f", PR: 6, Lane: "l", Phase: state.Waiting, PID: 5, Seeded: true, Outside: true, Roll: []string{"x"}, Checked: at}
	Merged(&m, at)
	if m.Phase != state.Settling || !m.Finished.Equal(at) || m.PID != 0 || m.Seeded || m.Roll != nil || m.Release != "" || !m.Outside {
		t.Errorf("merged outside: %+v", m)
	}
	if q := Queue(&state.State{Merges: []state.Merge{m, earlier}}, "l"); q.Settling == nil || q.Settling.Key() != "o/f#6" || q.SettlingKeys() != "o/a#1 o/f#6" {
		t.Errorf("the lane does not settle both merges, the latest last: %+v %q", q.Settling, q.SettlingKeys())
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
	s.Roll = roll
	if ok, why := Ready(lane, hrs, s, now.Add(time.Minute), time.Minute); !ok {
		t.Errorf("a merge with a roll set and an unknown release keeps the lane after the settle time: %q", why)
	}
}

// testdata/helmreleases-gazelle.json is gazelle's real answer to kubectl get
// helmreleases -A -o json, trimmed to the agent-platform lane's neighbours
// and to what the roll check reads: every chart version carries build
// metadata (4.74.0+971d12027db0) and each HelmRelease takes its chart from
// an OCIRepository (spec.chartRef), so the chart name is in the history.
func TestReadyGazelleBuildMetadata(t *testing.T) {
	hrs, err := ParseHelmReleases(testdata(t, "helmreleases-gazelle.json"))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "giantswarm/agent-platform"
	lane := config.Lane{Name: "ap", Installation: "gazelle", Repositories: []string{repo}}
	roll := RollSet(hrs, repo)
	if len(roll) != 1 || roll[0] != "flux-giantswarm/agent-platform" {
		t.Fatalf("roll set %v, want only the agent-platform chart's HelmRelease", roll)
	}
	now := time.Now()
	for release, want := range map[string]string{"v4.74.0": "", "v4.75.0": "flux-giantswarm/agent-platform is on 4.74.0, rolling to 4.75.0"} {
		ready, why := Ready(lane, hrs, &state.Merge{Repo: repo, PR: 671, Release: release, Roll: roll, Finished: now}, now, 5*time.Minute)
		if ready != (want == "") || why != want {
			t.Errorf("%s: ready %v %q, want %q", release, ready, why, want)
		}
	}
}

//go:build scenario

package scenario

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/update"
)

const releasesScenario = `
name: releases
flow: selfupdate
given:
  releases:
    installed: v1.0.0-rc.1
    published: [v1.0.0-rc.2, v1.0.0]
when:
  - name: loop
    background: {args: [supervisor, loop]}
  - name: old
    run: {release: v1.0.0-rc.1, args: [version]}
then:
  - log: {name: loop, contains: ["SUPERVISOR"]}
`

// given.releases publishes its releases on the scenario's GitHub, which a
// scenario that names none gets for them: the binary and its bundle as
// files of the sandbox, a pre-release where the version says so.
func TestReleasesArePublished(t *testing.T) {
	s, err := Parse("releases.yaml", []byte(releasesScenario))
	if err != nil {
		t.Fatal(err)
	}
	g := s.Given.GitHub
	if g == nil || len(g.Repositories) != 1 || g.Repositories[0].Name != update.Repository {
		t.Fatalf("given.github = %+v, want the updater's repository", g)
	}
	rels := g.Repositories[0].Releases
	if len(rels) != 2 || rels[0].Tag != "v1.0.0-rc.2" || !rels[0].Prerelease || rels[1].Tag != "v1.0.0" || rels[1].Prerelease {
		t.Fatalf("releases = %+v", rels)
	}
	assets := rels[0].Assets
	if len(assets) != 2 || assets[0].Name != ReleaseAsset() || assets[1].Name != ReleaseAsset()+".bundle" ||
		assets[0].File != "/work/releases/v1.0.0-rc.2/"+ReleaseAsset() || assets[1].File != assets[0].File+".bundle" {
		t.Errorf("assets = %+v", assets)
	}
	if got := s.Given.Releases.builds(); len(got) != 3 || got[0].Version != "v1.0.0-rc.1" {
		t.Errorf("builds = %+v, want the installed release and the two published ones", got)
	}
	if names := g.RepositoryNames(); len(names) != 1 || names[0] != update.Repository {
		t.Errorf("repositories = %v: the updater's repository gets an origin for its tags", names)
	}
}

func TestReleasesAndLogRejects(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"a version without v", strings.Replace(releasesScenario, "installed: v1.0.0-rc.1", "installed: 1.0.0", 1), `"1.0.0" is no release version`},
		{"a version with build metadata", strings.Replace(releasesScenario, "v1.0.0-rc.2, v1.0.0]", "v1.0.0+dirty]", 1), `"v1.0.0+dirty" is no release version`},
		{"a release published twice", strings.Replace(releasesScenario, "v1.0.0-rc.2, v1.0.0]", "v1.0.0, v1.0.0]", 1), "published names v1.0.0 twice"},
		{"a run of no release", strings.Replace(releasesScenario, "release: v1.0.0-rc.1", "release: v9.9.9", 1), `release "v9.9.9" is none of given.releases`},
		{"a log of no background step", strings.Replace(releasesScenario, "log: {name: loop", "log: {name: old", 1), `log "old" names no earlier background step`},
		{"a log without a check", strings.Replace(releasesScenario, `, contains: ["SUPERVISOR"]`, "", 1), "log without contains, not_contains or matches"},
		{"a log with a bad pattern", strings.Replace(releasesScenario, `contains: ["SUPERVISOR"]`, `matches: "("`, 1), "log: matches:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.name+".yaml", []byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse: %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// One checker reads a log for every step kind: a text the step never
// wrote fails, and so does a log the view cannot read.
func TestCheckLog(t *testing.T) {
	read := func(name string) (string, error) {
		return "12:00:00 HIGH LOAD: 1m 50 over 8\nSUPERVISOR REFILL started 0: starts held\n", nil
	}
	v := View{Kind: KindWaitFor, Log: read}
	if err := Check(Expect{Log: &LogExpect{Name: "loop", Contains: []string{"HIGH LOAD"}, NotContains: []string{"ENDED"}, Matches: `^SUPERVISOR REFILL started \d+`}}, v); err != nil {
		t.Errorf("a log that holds: %v", err)
	}
	if err := Check(Expect{Log: &LogExpect{Name: "loop", Contains: []string{"ENDED HIGH LOAD"}}}, v); err == nil || !strings.Contains(err.Error(), `log of loop lacks "ENDED HIGH LOAD"`) {
		t.Errorf("a missing line: %v", err)
	}
	if err := Check(Expect{Log: &LogExpect{Name: "loop", NotContains: []string{"starts held"}}}, v); err == nil {
		t.Error("a line that must not be there passed")
	}
	if err := Check(Expect{Log: &LogExpect{Name: "loop", Contains: []string{"x"}}}, View{Kind: KindRun}); err == nil || !strings.Contains(err.Error(), "log is not observed") {
		t.Errorf("a view without logs: %v", err)
	}
}

package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
)

const handTestUnmapped = "example/elsewhere"

// handPlacementTestApp is a desk whose supervisor.hosts are host-b, which
// maps no repository of the tests, then host-a, which maps
// sshTestRepository; the repository's default branch is main. It returns
// the default branch lookups and the hosts' start requests.
func handPlacementTestApp(t *testing.T) (*app, string, *[]string, *[]execution.StartRequest) {
	t.Helper()
	a, brief, requests := sshAgyTestApp(t, map[string]string{claudeComm: codexTestClaudeVer, capacity.HarnessAgy: testAgyVersion})
	other := a.cfg.Agents.Hosts[sshTestOtherSelector]
	other.Repositories = map[string]string{"example/unrelated": "/srv/source"}
	a.cfg.Agents.Hosts[sshTestOtherSelector] = other
	a.cfg.Supervisor.Hosts = []string{sshTestOtherSelector, sshTestSelector}
	a.cfg.Supervisor.HostsFirst = true
	var looked []string
	prev := loop.GhDefaultBranch
	loop.GhDefaultBranch = func(_ context.Context, repo string) (string, error) {
		looked = append(looked, repo)
		return mainBranch, nil
	}
	t.Cleanup(func() { loop.GhDefaultBranch = prev })
	return a, brief, &looked, requests
}

// A start without --host goes where the loop's would (supervisor.hostsFirst):
// the first host that maps its repository and has room, pinned to the
// repository's default branch, and it says so.
func TestHandStartIsPlacedOnTheFirstHostThatTakesIt(t *testing.T) {
	a, brief, looked, reqs := handPlacementTestApp(t)
	out, err := runAgents(a, agentStartName, "--name", "hand", brief, "--repo", sshTestRepository, "--harness", claudeComm, "--model", bgModel)
	if err != nil {
		t.Fatalf("hand start: %v\n%s", err, out)
	}
	if !strings.Contains(out, "placed on "+sshTestSelector+" by supervisor.hostsFirst: "+sshTestRepository+" at "+mainBranch) {
		t.Fatalf("placement line missing:\n%s", out)
	}
	if len(*reqs) == 0 {
		t.Fatalf("no execution requested on the host:\n%s", out)
	}
	var requests []string
	for _, r := range *reqs {
		requests = append(requests, string(r.Ref.Host)+" "+r.Repository)
	}
	if want := sshTestSelector + " " + sshTestRepository; slices.ContainsFunc(requests, func(r string) bool { return r != want }) {
		t.Fatalf("requests = %q, want each %q", requests, want)
	}
	if !slices.Equal(*looked, []string{sshTestRepository}) {
		t.Fatalf("default branch lookups = %q", *looked)
	}
}

// The repository a start names is --repo, else its brief's, else its
// checkout's GitHub origin; a --ref given pins the start instead of the
// default branch.
func TestHandStartPlacementRepositoryAndRef(t *testing.T) {
	a, _, looked, _ := handPlacementTestApp(t)
	ctx := context.Background()
	worker := capacity.StartKind(false, false)
	p := a.placeHandStart(ctx, handStart{brief: "Work https://github.com/" + sshTestRepository + "/issues/1", harness: claudeComm, model: bgModel, kind: worker})
	if p.Host != sshTestSelector || p.Repository != sshTestRepository || p.Ref != mainBranch {
		t.Fatalf("brief's repository: %+v", p)
	}
	p = a.placeHandStart(ctx, handStart{repository: sshTestRepository, ref: "release-1", harness: claudeComm, model: bgModel, kind: worker})
	if p.Host != sshTestSelector || p.Ref != "release-1" || len(*looked) != 1 {
		t.Fatalf("--ref: %+v, lookups %q", p, *looked)
	}
	keepTestGit(t)
	checkout := t.TempDir()
	for _, args := range [][]string{{"init", "-b", mainBranch}, {"remote", "add", "origin", "git@github.com:" + sshTestRepository + ".git"}} {
		if _, err := runGit(ctx, checkout, args...); err != nil {
			t.Fatal(err)
		}
	}
	if repo, err := a.handStartRepository(ctx, "", "no repository here", checkout); err != nil || repo != sshTestRepository {
		t.Fatalf("checkout's origin: %q, %v", repo, err)
	}
	if _, err := a.handStartRepository(ctx, "", "https://github.com/example/one/issues/1 example/two#2", checkout); err == nil || !strings.Contains(err.Error(), "several repositories") {
		t.Fatalf("two repositories: %v", err)
	}
}

// What keeps a start here says why; the loop's own starts, a desk under
// hostsFirst: false and one without hosts place nothing.
func TestHandStartStaysHereAndSaysWhy(t *testing.T) {
	a, _, _, _ := handPlacementTestApp(t)
	ctx := context.Background()
	worker := capacity.StartKind(false, false)
	base := handStart{repository: sshTestRepository, harness: claudeComm, model: bgModel, kind: worker}
	for _, tc := range []struct {
		name string
		edit func(*handStart)
		want string
	}{
		{"review", func(s *handStart) { s.review, s.kind = true, capacity.StartKind(true, false) }, "review start stays here"},
		{"timer", func(s *handStart) { s.byTimer = true }, "timer's start stays here"},
		{"desktop", func(s *handStart) { s.desktop = true }, "--desktop"},
		{"background", func(s *handStart) { s.mode = "background" }, "--mode background runs on this machine"},
		{"no model", func(s *handStart) { s.model = "" }, "names no model"},
		{"unmapped", func(s *handStart) { s.repository = handTestUnmapped }, "no host takes it: " + sshTestOtherSelector + ": " + handTestUnmapped + " is not mapped; " + sshTestSelector + ": " + handTestUnmapped + " is not mapped"},
	} {
		s := base
		tc.edit(&s)
		if p := a.placeHandStart(ctx, s); p.Host != "" || !strings.Contains(p.Local, tc.want) {
			t.Errorf("%s: %+v, want local %q", tc.name, p, tc.want)
		}
	}
	a.as = loop.LoopParty.Name
	if p := a.placeHandStart(ctx, base); p != (handPlacement{}) {
		t.Fatalf("loop start placed again: %+v", p)
	}
	a.as = ""
	a.cfg.Supervisor.HostsFirst = false
	if p := a.placeHandStart(ctx, base); p != (handPlacement{}) {
		t.Fatalf("hostsFirst false: %+v", p)
	}
	a.cfg.Supervisor.HostsFirst, a.cfg.Supervisor.Hosts = true, nil
	if p := a.placeHandStart(ctx, base); p != (handPlacement{}) {
		t.Fatalf("no hosts: %+v", p)
	}
}

// A start that names --repo, which only a host serves, is refused with the
// hosts' reasons when none takes it.
func TestHandStartWithRepoNoHostTakesIsRefused(t *testing.T) {
	a, brief, _, _ := handPlacementTestApp(t)
	_, err := runAgents(a, agentStartName, "--name", "hand", brief, "--repo", handTestUnmapped, "--harness", claudeComm, "--model", bgModel)
	if err == nil || !strings.Contains(err.Error(), "--repo and --ref require an SSH --host: no host takes it") || !strings.Contains(err.Error(), handTestUnmapped+" is not mapped") {
		t.Fatalf("unmapped --repo: %v", err)
	}
}

func TestBriefRepositories(t *testing.T) {
	got := briefRepositories("Work example/one#1, https://github.com/Example/One/pull/2 and https://github.com/example/two")
	if !slices.Equal(got, []string{"example/one", "example/two"}) {
		t.Fatalf("repositories = %q", got)
	}
	if got := briefRepositories(paramTask); len(got) != 0 {
		t.Fatalf("no repository: %q", got)
	}
}

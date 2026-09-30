package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/state"
)

func labApp(out *bytes.Buffer, running ...string) *app {
	return &app{out: out, cfg: &config.Config{
		Resources: []string{"agentlab-1", "agentlab-2", graveler},
		Labs:      map[string]string{"agentlab-1": "agentlab", "agentlab-2": "agentlab-2"},
	}, kindClusters: func() ([]string, error) { return running, nil }}
}

// Two running kind clusters and no held lab lease: each shows under the
// lease it belongs to, running and free; a third is unmapped.
func TestLabsList(t *testing.T) {
	var out bytes.Buffer
	a := labApp(&out, "agentlab-2", "agentlab", "scratch")
	l := a.readLabs(map[string]string{graveler: agentFour})
	want := []labView{{Lease: "agentlab-1", Cluster: "agentlab", Running: true}, {Lease: "agentlab-2", Cluster: "agentlab-2", Running: true}}
	if len(l.Labs) != 2 || l.Labs[0] != want[0] || l.Labs[1] != want[1] {
		t.Errorf("labs = %+v, want %+v", l.Labs, want)
	}
	if len(l.Unmapped) != 1 || l.Unmapped[0] != "scratch" {
		t.Errorf("unmapped = %v, want [scratch]", l.Unmapped)
	}
	a.printLabs(l)
	got := out.String()
	for _, line := range []string{"agentlab-1  agentlab      running  free", "agentlab-2  agentlab-2    running  free",
		"-           scratch       running  unmapped: no lab lease stands for it"} {
		if !strings.Contains(got, line) {
			t.Errorf("lease list lacks %q:\n%s", line, got)
		}
	}
}

func TestLabsHeldAndStopped(t *testing.T) {
	var out bytes.Buffer
	a := labApp(&out, "agentlab")
	l := a.readLabs(map[string]string{"agentlab-1": agentFour})
	if l.Labs[0].Holder != agentFour || !l.Labs[0].Running || l.Labs[1].Running || len(l.Unmapped) != 0 {
		t.Errorf("labs = %+v", l)
	}
	a.printLabs(l)
	if got := out.String(); !strings.Contains(got, `held by "`+agentFour+`"`) || !strings.Contains(got, "agentlab-2    not running  free") {
		t.Errorf("lease list:\n%s", got)
	}
}

// Without lab leases docker is not asked and nothing is printed.
func TestLabsUnconfigured(t *testing.T) {
	var out bytes.Buffer
	a := &app{out: &out, cfg: &config.Config{Resources: []string{graveler}},
		kindClusters: func() ([]string, error) { t.Fatal("docker asked without lab leases"); return nil, nil }}
	a.printLabs(a.readLabs(nil))
	if a.claimedLab(graveler) != "" || out.Len() != 0 {
		t.Errorf("claim %q, output %q", a.claimedLab(graveler), out.String())
	}
}

func TestLabsDockerDown(t *testing.T) {
	var out bytes.Buffer
	a := labApp(&out)
	a.kindClusters = func() ([]string, error) { return nil, errors.New("docker: not found") }
	a.printLabs(a.readLabs(nil))
	if !strings.Contains(out.String(), "kind clusters unknown (docker: not found)") {
		t.Errorf("output %q", out.String())
	}
	if got := a.claimedLab("agentlab-1"); got != " (kind cluster agentlab)" {
		t.Errorf("claim says %q", got)
	}
}

// A claim of a lab lease states the cluster it covers.
func TestClaimedLab(t *testing.T) {
	a := labApp(nil, "agentlab")
	for res, want := range map[string]string{
		"agentlab-1": " (kind cluster agentlab, running)",
		"agentlab-2": " (kind cluster agentlab-2, not running)",
		graveler:     "",
	} {
		if got := a.claimedLab(res); got != want {
			t.Errorf("claim of %s says %q, want %q", res, got, want)
		}
	}
}

// free names a running cluster whose lease is free as idle, with its last
// holder, and one no lease maps as unmapped.
func TestClusterNotes(t *testing.T) {
	a := labApp(nil)
	events := []state.Event{
		{Verb: "lease.claim", By: state.Party{Name: "Board pull 1"}, Detail: "agentlab-2: proof"},
		{Verb: "lease.claim", By: state.Party{Name: "Board pull 2"}, Detail: "agentlab-2: second proof"},
		{Verb: "lease.claim", By: state.Party{Name: "Board pull 3"}, Detail: "agentlab-1: other"},
	}
	cs := []machine.Cluster{{Name: "agentlab"}, {Name: "agentlab-2"}, {Name: "scratch"}}
	got := a.clusterNotes(cs, []lease.Holder{{Env: "agentlab-1", Name: agentFour}}, events)
	for cl, want := range map[string]string{
		"agentlab":   `agentlab-1, held by "` + agentFour + `"`,
		"agentlab-2": `idle: agentlab-2 is free, last held by "Board pull 2"`,
		"scratch":    "unmapped: no lab lease stands for it",
	} {
		if got[cl] != want {
			t.Errorf("%s: %q, want %q", cl, got[cl], want)
		}
	}
	if n := a.clusterNotes(cs, nil, nil)["agentlab-2"]; n != "idle: agentlab-2 is free" {
		t.Errorf("without events: %q", n)
	}
}

// free opens no store: its notes read the event log on their own.
func TestFreeClusterNotesWithoutStore(t *testing.T) {
	a := labApp(nil)
	a.cfg.StateDir, a.cfg.LeaseDir = t.TempDir(), t.TempDir()
	notes := a.freeClusterNotes([]machine.Cluster{{Name: "agentlab"}}, nil)
	if notes["agentlab"] != "idle: agentlab-1 is free" {
		t.Errorf("notes = %v", notes)
	}
}

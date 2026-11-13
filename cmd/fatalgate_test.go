//go:build unix

package cmd

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/fatal"
)

func TestFatalMainGateRefusesOrdinaryMergeAndPromotion(t *testing.T) {
	for _, pr := range []int{0, 7} {
		t.Run(strconv.Itoa(pr), func(t *testing.T) {
			a := heldApp(t)
			q := fatal.Queue{Incidents: []fatal.Incident{{ID: "main:" + scratchRepo, Repair: scratchRepo + "#8"}}}
			if err := q.Save(a.store.Dir()); err != nil {
				t.Fatal(err)
			}
			stubFatalBranches(t, fatalScopeBase, nil)
			g := placedGate(a, pr, os.Getpid())
			var err error
			out := gateStderr(t, func() { _, err = g.Step() })
			if Code(err) != ExitGateRefused || !strings.Contains(out, "scope: default-branch merges and releases in "+scratchRepo) {
				t.Fatalf("gate did not refuse: %v %s", err, out)
			}
		})
	}
}

func TestFatalMainRepairStillPassesNormalGateAdmission(t *testing.T) {
	a := heldApp(t)
	q := fatal.Queue{Incidents: []fatal.Incident{{ID: "main:" + scratchRepo, Repair: scratchRepo + "#7"}}}
	if err := q.Save(a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	g := placedGate(a, 7, os.Getpid())
	g.Placed, g.Queued = false, false
	why, err := g.Step()
	if err != nil || !strings.Contains(why, "merges and promotes run machine-wide") {
		t.Fatalf("repair bypassed cap or did not reach normal admission: %s %v", why, err)
	}
}

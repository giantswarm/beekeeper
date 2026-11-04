package upgrade

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestDueScheduleUsesScheduledRelease(t *testing.T) {
	const next = "35.2.0"
	for _, age := range []time.Duration{10 * time.Minute, 50 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			objs := withMC(t, "0843-after", map[string]string{
				upgradingMark: marked, upgradeStartTime: stamp(age),
				scheduleRelease: next, scheduleTime: stamp(time.Minute),
			})
			ups := Detect(objs, at, func(string) bool { return false })
			if len(ups) != 1 {
				t.Fatalf("upgrades %+v", ups)
			}
			u := ups[0]
			if u.Cluster != "mc" || u.From != to || u.To != next || !u.Since.Equal(at.Add(-time.Minute)) || u.Rolling {
				t.Errorf("due schedule behind a previous mark: %+v", u)
			}
		})
	}
}

func TestDueScheduleReholdsLiftedUpgrade(t *testing.T) {
	const next = "35.2.0"
	st := &state.State{}
	Reconcile(st, prod, []Upgrade{{Cluster: "mc", To: to}}, at.Add(-10*time.Minute), watch)
	if !Lift(st, HoldTarget(prod, "mc"), state.Party{Name: "Supervisor"}, at.Add(-5*time.Minute)) {
		t.Fatal("the previous upgrade was not held")
	}
	objs := withMC(t, "0843-after", map[string]string{
		upgradingMark: marked, upgradeStartTime: stamp(10 * time.Minute),
		scheduleRelease: next, scheduleTime: stamp(time.Minute),
	})
	held := HeldClusters(st, at)
	ups := Detect(objs, at, func(cluster string) bool { return held(prod, cluster) })
	begun, _ := Reconcile(st, prod, ups, at, watch)
	if len(begun) != 1 {
		t.Fatalf("the new scheduled upgrade did not begin a hold: %+v", st.Holds)
	}
	h, ok := Held(st, prod, at)
	if !ok || h.UpgradeTo != next || h.LiftedBy != nil {
		t.Errorf("the previous lift admitted a different scheduled upgrade: %+v, held %v", h, ok)
	}
}

func TestReleaseChangePrecedesDueSchedule(t *testing.T) {
	objs := withMC(t, "0821-changed", map[string]string{
		scheduleRelease: "35.2.0", scheduleTime: stamp(time.Minute),
	})
	ups := Detect(objs, at, func(string) bool { return false })
	if len(ups) != 1 {
		t.Fatalf("upgrades %+v", ups)
	}
	if u := ups[0]; u.From != "35.0.1" || u.To != to {
		t.Errorf("the due schedule hid an observed release change: %+v", u)
	}
}

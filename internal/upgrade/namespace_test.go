package upgrade

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestSameNameClustersHaveIndependentUpgradeHolds(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			st := &state.State{}
			a := Upgrade{Namespace: "ns-a", Cluster: "dev", To: to}
			b := Upgrade{Namespace: "ns-b", Cluster: "dev", To: to}
			Reconcile(st, prod, []Upgrade{a}, at, watch)
			if !Lift(st, HoldTarget(prod, "ns-a/dev"), watch, at) {
				t.Fatal("first cluster was not held")
			}
			running := []Upgrade{a, b}
			if reverse {
				running = []Upgrade{b, a}
			}
			begun, _ := Reconcile(st, prod, running, at.Add(time.Minute), watch)
			if len(begun) != 1 || begun[0].Target != HoldTarget(prod, "ns-b/dev") || len(st.Holds) != 2 {
				t.Fatalf("the first lift admitted the second cluster: begun %+v, holds %+v", begun, st.Holds)
			}
			if !Lift(st, HoldTarget(prod, "ns-b/dev"), watch, at.Add(time.Minute)) {
				t.Fatal("second cluster was not independently held")
			}
			if _, held := Held(st, prod, at.Add(time.Minute)); held {
				t.Fatal("both lifted clusters still hold the installation")
			}
			Reconcile(st, prod, []Upgrade{b}, at.Add(2*time.Minute), watch)
			if len(st.Holds) != 1 || st.Holds[0].Target != HoldTarget(prod, "ns-b/dev") || st.Holds[0].LiftedBy == nil {
				t.Fatalf("ending one cluster changed the other's lift: %+v", st.Holds)
			}
			Reconcile(st, prod, nil, at.Add(3*time.Minute), watch)
			if len(st.Holds) != 0 {
				t.Fatalf("ended clusters retained holds: %+v", st.Holds)
			}
		})
	}
	st := &state.State{}
	running := []Upgrade{{Namespace: "ns-a", Cluster: "dev", To: to}, {Namespace: "ns-b", Cluster: "dev", To: to}}
	begun, _ := Reconcile(st, prod, running, at, watch)
	if len(begun) != 2 || begun[0].Target == begun[1].Target {
		t.Fatalf("same-name clusters did not get distinct holds: %+v", begun)
	}
}

func TestHeldUpgradeRolloutIsScopedToNamespace(t *testing.T) {
	var objs []Object
	for _, ns := range []string{"ns-a", "ns-b"} {
		o := Object{Kind: kindCluster}
		o.Metadata.Name, o.Metadata.Namespace = "dev", ns
		o.Status.Conditions = []Condition{{Type: rollingOut, Status: "True"}}
		objs = append(objs, o)
	}
	st := &state.State{}
	Reconcile(st, prod, []Upgrade{{Namespace: "ns-a", Cluster: "dev", To: to}}, at, watch)
	Lift(st, HoldTarget(prod, "ns-a/dev"), watch, at)
	held := HeldClusters(st, at)
	ups := Detect(objs, at, func(c string) bool { return held(prod, c) })
	if len(ups) != 1 || ups[0].Namespace != "ns-a" || !ups[0].Rolling {
		t.Fatalf("another namespace's rollout borrowed the hold: %+v", ups)
	}
}

func TestLegacyUpgradeHoldMigration(t *testing.T) {
	for _, lifted := range []bool{false, true} {
		st := &state.State{Holds: []state.Hold{{Target: HoldTarget(prod, "dev"), At: at, UpgradeTo: to}}}
		if lifted {
			Lift(st, HoldTarget(prod, "dev"), watch, at)
		}
		// Namespace identity was never persisted, so even a single known
		// upgrade cannot safely inherit the old manual lift.
		Reconcile(st, prod, []Upgrade{{Namespace: "ns-b", Cluster: "dev", To: to}}, at.Add(time.Minute), watch)
		if len(st.Holds) != 1 || st.Holds[0].Target != HoldTarget(prod, "ns-b/dev") || !st.Holds[0].Active(at.Add(time.Minute)) {
			t.Fatalf("legacy lift %v: %+v", lifted, st.Holds)
		}
	}
	// An active legacy hold must also survive the transition after the
	// release mark cleared, without restarting its stall interval.
	st := &state.State{Holds: []state.Hold{{Target: HoldTarget(prod, "dev"), At: at.Add(-time.Hour), UpgradeTo: to}}}
	o := Object{Kind: kindCluster}
	o.Metadata.Namespace, o.Metadata.Name = "ns-a", "dev"
	o.Status.Conditions = []Condition{{Type: rollingOut, Status: "True"}}
	held := HeldClusters(st, at)
	ups := Detect([]Object{o}, at, func(c string) bool { return held(prod, c) })
	Reconcile(st, prod, ups, at, watch)
	if len(st.Holds) != 1 || st.Holds[0].Target != HoldTarget(prod, "ns-a/dev") || !st.Holds[0].At.Equal(at.Add(-time.Hour)) {
		t.Fatalf("legacy rollout lost its hold or stall clock: %+v", st.Holds)
	}
}

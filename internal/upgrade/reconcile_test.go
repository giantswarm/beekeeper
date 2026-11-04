package upgrade

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestReconcileReplacesActiveUpgrade(t *testing.T) {
	st := &state.State{}
	Reconcile(st, prod, []Upgrade{{Cluster: "mc", From: "35.0.1", To: to}}, at.Add(-23*time.Hour), watch)
	next := Upgrade{Cluster: "mc", From: to, To: "35.2.0"}
	begun, ended := Reconcile(st, prod, []Upgrade{next}, at, watch)
	if len(begun) != 1 || len(ended) != 1 || len(st.Holds) != 1 {
		t.Fatalf("a different upgrade kept the previous active hold: begun %+v, ended %+v, holds %+v", begun, ended, st.Holds)
	}
	h := st.Holds[0]
	if h.UpgradeTo != next.To || h.Reason != reasonHead+Describe(prod, next) || !h.At.Equal(at) || ended[0].UpgradeTo != to {
		t.Errorf("replacement hold %+v, ended %+v", h, ended)
	}
	// The new rollout gets its own stall interval, even when the previous
	// hold would now have passed StallAfter.
	if begun, ended = Reconcile(st, prod, []Upgrade{{Cluster: "mc", Rolling: true}}, at.Add(2*time.Hour), watch); len(begun)+len(ended) != 0 {
		t.Fatalf("the previous hold's age ended the new rollout: begun %+v, ended %+v", begun, ended)
	}
	if !Lift(st, HoldTarget(prod, "mc"), state.Party{Name: "Supervisor"}, at.Add(2*time.Hour)) {
		t.Fatal("the new upgrade was not held")
	}
	if begun, ended = Reconcile(st, prod, []Upgrade{next}, at.Add(2*time.Hour+time.Minute), watch); len(begun)+len(ended) != 0 {
		t.Errorf("lifting the new upgrade did not stick: begun %+v, ended %+v", begun, ended)
	}
	if _, ok := Held(st, prod, at.Add(2*time.Hour+time.Minute)); ok {
		t.Error("the new upgrade was re-held after its lift")
	}
}

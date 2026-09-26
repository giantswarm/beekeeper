package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// testdata/prod is a real release upgrade of a management cluster, mc, from
// 35.0.1 to 35.1.1 (08:21:29 to 08:34:47), next to a workload cluster, ops,
// that stays on 35.1.0 throughout; stripped to the fields the detection
// reads. 0843-after and the events are the objects as read after it;
// 0820-before, 0821-changed (the release label changed, cluster-api-events
// not yet reacted) and 0827-during (cluster-api-events' mark and start time,
// the control plane rolling) are rebuilt from them, the events and the
// conditions' transition times, the rolling control plane from another
// installation's control plane read mid-upgrade.
var phases = []string{"0820-before", "0821-changed", "0827-during", "0843-after"}

func objects(t *testing.T, phase string) []Object {
	t.Helper()
	var objs []Object
	for _, k := range Kinds {
		raw, err := os.ReadFile(filepath.Join("testdata", prod, phase, kindFile(k)+".json")) //nolint:gosec // testdata
		if err != nil {
			t.Fatal(err)
		}
		o, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		objs = append(objs, o...)
	}
	return objs
}

func kindFile(kind string) string {
	for i, c := range kind {
		if c == '.' {
			return kind[:i]
		}
	}
	return kind
}

const (
	prod = "prod"
	to   = "35.1.1"
)

var (
	at    = time.Date(2026, 9, 25, 8, 30, 0, 0, time.UTC)
	watch = state.Party{Name: "beekeeper watch"}
)

// TestReplay reads the phases in order as the watch does and checks that
// the upgrade begins when mc's release label changes, runs while
// cluster-api-events marks it, and ends after, with ops never upgrading.
func TestReplay(t *testing.T) {
	st := &state.State{}
	var lines []string
	for _, phase := range phases {
		held := HeldClusters(st, at)
		ups := Detect(objects(t, phase), at, func(c string) bool { return held(prod, c) })
		begun, ended := Reconcile(st, prod, ups, at, watch)
		for _, h := range begun {
			lines = append(lines, phase+" "+Line(h))
		}
		for _, h := range ended {
			lines = append(lines, phase+" "+EndLine(h))
		}
		if _, ok := Held(st, prod, at); ok != (phase == "0821-changed" || phase == "0827-during") {
			t.Errorf("%s: prod held %v", phase, ok)
		}
	}
	want := []string{
		"0821-changed UPGRADE prod/mc 35.0.1 → 35.1.1",
		"0843-after UPGRADE ENDED prod/mc 35.0.1 → 35.1.1 (since " + at.Local().Format("15:04") + ")",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines\n%q\nwant\n%q", lines, want)
	}
}

func TestDetectDuring(t *testing.T) {
	ups := Detect(objects(t, "0827-during"), at, func(string) bool { return false })
	if len(ups) != 1 {
		t.Fatalf("upgrades %+v", ups)
	}
	u := ups[0]
	if u.Cluster != "mc" || u.From != "" || u.To != to || u.ControlPlane != (Progress{3, 4}) || u.NodePools != (Progress{15, 15}) ||
		!u.Since.Equal(time.Date(2026, 9, 25, 8, 21, 29, 0, time.UTC)) {
		t.Errorf("upgrade %+v", u)
	}
}

// TestRolling: an upgrade cluster-api-events has marked done runs on while
// its control plane rolls, but only one that began.
func TestRolling(t *testing.T) {
	objs := objects(t, "0827-during")
	for i := range objs {
		if objs[i].Kind == kindCluster {
			objs[i].Metadata.Annotations[upgradingMark] = "false"
		}
	}
	if ups := Detect(objs, at, func(c string) bool { return c == "mc" }); len(ups) != 1 || !ups[0].Rolling {
		t.Errorf("a held upgrade whose control plane rolls ended: %+v", ups)
	}
	if ups := Detect(objs, at, func(string) bool { return false }); len(ups) != 0 {
		t.Errorf("a rollout alone began an upgrade: %+v", ups)
	}
	st := &state.State{}
	if begun, _ := Reconcile(st, prod, Detect(objs, at, func(string) bool { return true }), at, watch); len(begun) != 0 {
		t.Errorf("a rollout set a hold: %+v", begun)
	}
}

func TestSchedule(t *testing.T) {
	objs := objects(t, "0820-before")
	for i := range objs {
		if objs[i].Kind == kindCluster && objs[i].Metadata.Name == "ops" {
			objs[i].Metadata.Annotations[scheduleRelease] = to
			objs[i].Metadata.Annotations[scheduleTime] = "2026-09-25T09:00:00Z"
		}
	}
	none := func(string) bool { return false }
	if ups := Detect(objs, at, none); len(ups) != 0 {
		t.Errorf("an upgrade scheduled for later runs: %+v", ups)
	}
	ups := Detect(objs, at.Add(time.Hour), none)
	if len(ups) != 1 || ups[0].Cluster != "ops" || ups[0].From != "35.1.0" || ups[0].To != to {
		t.Errorf("a due upgrade: %+v", ups)
	}
}

func TestReconcileKeepsOthers(t *testing.T) {
	st := &state.State{Holds: []state.Hold{{Target: "lane:serving"}, {Target: HoldTarget("test", "mc")}, {Target: HoldTarget(prod, "gone")}}}
	begun, ended := Reconcile(st, prod, []Upgrade{{Cluster: "mc", From: "1", To: "2"}}, at, watch)
	if len(begun) != 1 || len(ended) != 1 || ended[0].Target != "upgrade:prod/gone" {
		t.Errorf("begun %+v ended %+v", begun, ended)
	}
	if begun, ended = Reconcile(st, prod, []Upgrade{{Cluster: "mc", From: "1", To: "2"}}, at, watch); len(begun)+len(ended) != 0 {
		t.Errorf("a second reading said %+v %+v", begun, ended)
	}
	if len(st.Holds) != 3 {
		t.Errorf("holds %+v", st.Holds)
	}
}

// fakeKubectl answers `get <kind>` with dir/<kind>.json, else as a server
// that does not serve the kind.
func fakeKubectl(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubectl")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do [ "$1" = get ] && break; shift; done
kind="${2%%.*}"
[ -n "$FAIL" ] && { echo "error: $FAIL" >&2; exit 1; }
[ -f "` + dir + `/$kind.json" ] && exec cat "` + dir + `/$kind.json"
echo "error: the server doesn't have a resource type \"$kind\"" >&2
exit 1
`
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // a test's script
		t.Fatal(err)
	}
	return p
}

func TestRead(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", prod, "0827-during"))
	if err != nil {
		t.Fatal(err)
	}
	r := Reader{Kubectl: fakeKubectl(t, dir), Timeout: 10 * time.Second}
	none := func(string, string) bool { return false }
	ss := r.Read(context.Background(), []Target{{Name: prod, Context: prod}, {Name: "far"}}, at, none)
	if len(ss) != 2 || len(ss[0].Upgrades) != 1 || ss[0].Upgrades[0].From != "35.0.1" {
		t.Fatalf("statuses %+v", ss)
	}
	if w := ss[0].Words(at); w != "prod/mc 35.0.1 → 35.1.1 for 9m (control plane 3/4, node pools 15/15)" {
		t.Errorf("words %q", w)
	}
	if ss[1].Err == "" {
		t.Errorf("an installation without a context is readable: %+v", ss[1])
	}
	// A held upgrade's from is in its hold's reason: the events are not read.
	held := func(i, c string) bool { return i == prod && c == "mc" }
	if ss = r.Read(context.Background(), []Target{{Name: prod, Context: prod}}, at, held); ss[0].Upgrades[0].From != "" {
		t.Errorf("the events of a held upgrade were read: %+v", ss[0])
	}

	empty := Reader{Kubectl: fakeKubectl(t, t.TempDir()), Timeout: 10 * time.Second}
	if ss = empty.Read(context.Background(), []Target{{Name: "lab", Context: "lab"}}, at, none); !ss[0].NoClusterAPI || ss[0].Words(at) != "lab none (no Cluster API)" {
		t.Errorf("an installation without the Cluster API: %+v", ss[0])
	}
	t.Setenv("FAIL", "Unauthorized")
	if ss = r.Read(context.Background(), []Target{{Name: prod, Context: prod}}, at, none); ss[0].Err != "error: Unauthorized" || len(ss[0].Upgrades) != 0 {
		t.Errorf("an unreadable installation: %+v", ss[0])
	}
}

func TestLiftSticks(t *testing.T) {
	st := &state.State{}
	stuck := []Upgrade{{Cluster: "wc", To: "36.0.0"}}
	Reconcile(st, prod, stuck, at, watch)
	timo := state.Party{Name: "Supervisor"}
	if !Lift(st, HoldTarget(prod, "wc"), timo, at) || Lift(st, HoldTarget(prod, "wc"), timo, at) {
		t.Fatal("lift: want the first to lift and the second to find nothing held")
	}
	for tick := range 3 {
		if begun, ended := Reconcile(st, prod, stuck, at.Add(time.Duration(tick)*time.Minute), watch); len(begun)+len(ended) != 0 {
			t.Fatalf("tick %d re-held: begun %+v ended %+v", tick, begun, ended)
		}
		if _, held := Held(st, prod, at); held {
			t.Fatalf("tick %d: the lifted hold is active again", tick)
		}
	}
	if len(st.Holds) != 1 || st.Holds[0].LiftedBy == nil || st.Holds[0].LiftedBy.Name != "Supervisor" {
		t.Fatalf("holds %+v", st.Holds)
	}
	if !HeldClusters(st, at)(prod, "wc") {
		t.Error("a lifted upgrade's rollout must still count as running")
	}
	rolling := []Upgrade{{Cluster: "wc", Rolling: true}}
	if begun, ended := Reconcile(st, prod, rolling, at, watch); len(begun)+len(ended) != 0 || st.Holds[0].LiftedBy == nil {
		t.Errorf("the rollout running on re-held: begun %+v ended %+v", begun, ended)
	}
	begun, _ := Reconcile(st, prod, []Upgrade{{Cluster: "wc", From: "36.0.0", To: "36.1.0"}}, at, watch)
	if len(begun) != 1 || st.Holds[0].LiftedBy != nil || st.Holds[0].UpgradeTo != "36.1.0" {
		t.Errorf("a different upgrade must hold again: begun %+v holds %+v", begun, st.Holds)
	}
	Lift(st, HoldTarget(prod, "wc"), timo, at)
	if _, ended := Reconcile(st, prod, nil, at, watch); len(ended) != 1 || len(st.Holds) != 0 {
		t.Errorf("the upgrade's end must remove the lifted hold: ended %+v holds %+v", ended, st.Holds)
	}
}

func TestLiftBackfillsTarget(t *testing.T) {
	st := &state.State{Holds: []state.Hold{{Target: HoldTarget(prod, "wc")}}}
	Reconcile(st, prod, []Upgrade{{Cluster: "wc", To: "36.0.0"}}, at, watch)
	if st.Holds[0].UpgradeTo != "36.0.0" {
		t.Errorf("holds %+v", st.Holds)
	}
}

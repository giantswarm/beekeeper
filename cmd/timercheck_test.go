package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fakeRead answers the timers' check commands with read for the test.
func fakeRead(t *testing.T, read func(cmd string) ([]byte, error)) {
	t.Helper()
	was := readProbe
	readProbe = func(_ context.Context, cmd string) ([]byte, error) { return read(cmd) }
	t.Cleanup(func() { readProbe = was })
}

const cpWhen = "controlplane-ready admin@mc/org-x/wc1"

// KubeadmControlPlanes as kubectl returns them: three replicas wanted, the
// v1beta1 shape with updatedReplicas and the Ready summary, the v1beta2 one
// with upToDateReplicas and no Ready condition.
const (
	kcpV1beta1Rolling = `{"apiVersion":"controlplane.cluster.x-k8s.io/v1beta1","kind":"KubeadmControlPlane",
		"metadata":{"generation":4},"spec":{"replicas":3},
		"status":{"observedGeneration":4,"replicas":4,"readyReplicas":3,"updatedReplicas":2,
			"conditions":[{"type":"Ready","status":"False","reason":"RollingUpdateInProgress"}]}}`
	kcpV1beta1Rolled = `{"apiVersion":"controlplane.cluster.x-k8s.io/v1beta1","kind":"KubeadmControlPlane",
		"metadata":{"generation":4},"spec":{"replicas":3},
		"status":{"observedGeneration":4,"replicas":3,"readyReplicas":3,"updatedReplicas":3,
			"conditions":[{"type":"Ready","status":"True"}]}}`
	kcpV1beta2Rolling = `{"apiVersion":"controlplane.cluster.x-k8s.io/v1beta2","kind":"KubeadmControlPlane",
		"metadata":{"generation":7},"spec":{"replicas":3},
		"status":{"observedGeneration":7,"replicas":4,"readyReplicas":4,"availableReplicas":4,"upToDateReplicas":1,
			"conditions":[{"type":"Available","status":"True"},{"type":"RollingOut","status":"True"}],
			"deprecated":{"v1beta1":{"updatedReplicas":1,"readyReplicas":4}}}}`
	kcpV1beta2Rolled = `{"apiVersion":"controlplane.cluster.x-k8s.io/v1beta2","kind":"KubeadmControlPlane",
		"metadata":{"generation":7},"spec":{"replicas":3},
		"status":{"observedGeneration":7,"replicas":3,"readyReplicas":3,"availableReplicas":3,"upToDateReplicas":3,
			"conditions":[{"type":"Available","status":"True"},{"type":"RollingOut","status":"False"}]}}`
)

func TestControlPlaneReadyReadsBothAPIVersions(t *testing.T) {
	for name, tc := range map[string]struct {
		obj   string
		holds bool
		why   string
	}{
		"v1beta1 rolling": {kcpV1beta1Rolling, false, "4 of 3 replicas, 3 ready, 2 up to date"},
		"v1beta1 rolled":  {kcpV1beta1Rolled, true, ""},
		"v1beta2 rolling": {kcpV1beta2Rolling, false, "4 of 3 replicas, 4 ready, 1 up to date"},
		"v1beta2 rolled":  {kcpV1beta2Rolled, true, ""},
		"v1beta1 with the v1beta2 counts": {`{"kind":"KubeadmControlPlane","spec":{"replicas":1},
			"status":{"replicas":1,"readyReplicas":1,"v1beta2":{"upToDateReplicas":1}}}`, true, ""},
		"the Ready summary alone": {`{"kind":"KubeadmControlPlane","spec":{"replicas":3},
			"status":{"conditions":[{"type":"Ready","status":"True"}]}}`, true, ""},
		"a spec the status has not seen": {strings.Replace(kcpV1beta2Rolled, `"generation":7`, `"generation":8`, 1),
			false, "generation 8 not observed yet (status at 7)"},
		"the default of one replica": {`{"kind":"KubeadmControlPlane","spec":{},"status":{}}`,
			false, "0 of 1 replicas, 0 ready, 0 up to date"},
	} {
		holds, why, err := controlPlaneReady([]byte(tc.obj))
		if err != nil || holds != tc.holds || why != tc.why {
			t.Errorf("%s: holds %v, %q, %v", name, holds, why, err)
		}
	}
	for _, obj := range []string{`{"kind":"HelmRelease"}`, `error: not json`} {
		if _, _, err := controlPlaneReady([]byte(obj)); err == nil {
			t.Errorf("%s read as a KubeadmControlPlane", obj)
		}
	}
}

// A controlplane-ready timer records why the control plane is not ready yet
// and wakes its agent within one tick of the rollout, for either API
// version.
func TestControlPlaneTimerFiresOnceRolledOut(t *testing.T) {
	for version, stages := range map[string][2]string{
		"v1beta1": {kcpV1beta1Rolling, kcpV1beta1Rolled},
		"v1beta2": {kcpV1beta2Rolling, kcpV1beta2Rolled},
	} {
		t.Run(version, func(t *testing.T) {
			w, _, out := notifyingWatch(t, t.TempDir(), false)
			obj := stages[0]
			fakeRead(t, func(cmd string) ([]byte, error) {
				if !strings.Contains(cmd, "get kubeadmcontrolplanes.controlplane.cluster.x-k8s.io wc1") {
					return nil, errors.New("unexpected " + cmd)
				}
				return []byte(obj), nil
			})
			var woke []string
			wasWake := wakeOwner
			wakeOwner = func(_ *app, _ context.Context, _ state.Party, q, msg, _ string) error {
				woke = append(woke, q+": "+msg)
				return nil
			}
			t.Cleanup(func() { wakeOwner = wasWake })
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: state.Party{Session: "s9", Name: "BK 594"}}}
				st.Timers = []state.Timer{{ID: 1, Due: relayNow.Add(-time.Minute), When: cpWhen, Wake: "BK 594", What: "upgrade the node pools"}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			live := []*claude.Session{}
			w.pending(context.Background(), live)
			st, _ := w.store.Read()
			if len(woke) != 0 || len(st.Timers) != 1 || st.Timers[0].Unreadable || !strings.Contains(st.Timers[0].Reason, "of 3 replicas") {
				t.Fatalf("rolling: woke %v, timers %+v", woke, st.Timers)
			}
			obj = stages[1]
			w.now = relayNow.Add(timerEvery)
			w.pending(context.Background(), live)
			w.timerActs.Wait()
			if len(woke) != 1 || woke[0] != "BK 594: beekeeper timer #1 ("+cpWhen+" holds): upgrade the node pools" {
				t.Fatalf("woke %q", woke)
			}
			if st, _ := w.store.Read(); len(st.Timers) != 0 {
				t.Fatalf("still open: %+v", st.Timers)
			}
			if strings.Contains(out.String(), "UNREADABLE") {
				t.Fatalf("watch said %q", out.String())
			}
		})
	}
}

// A reference the watch cannot read is one TIMER UNREADABLE line, again only
// when the reason changes; the timer keeps waiting and fires once it holds.
func TestUnreadableReferenceIsOneWatchLine(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	answer := func() ([]byte, error) {
		return nil, errors.New(`exit status 1: error: context "admin@mc" does not exist`)
	}
	fakeRead(t, func(string) ([]byte, error) { return answer() })
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{{ID: 3, Due: relayNow.Add(-time.Minute), When: cpWhen, What: "look at wc1"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	live := []*claude.Session{}
	tick := func(n int) {
		w.now = relayNow.Add(time.Duration(n) * timerEvery)
		w.pending(context.Background(), live)
	}
	tick(0)
	tick(1)
	line := `TIMER UNREADABLE: #3, ` + cpWhen + ` cannot be read (exit status 1: error: context "admin@mc" does not exist); it keeps waiting: look at wc1`
	if got := strings.Count(out.String(), "TIMER UNREADABLE"); got != 1 || !strings.Contains(out.String(), line) {
		t.Fatalf("%d lines, watch said %q", got, out.String())
	}
	st, _ := w.store.Read()
	if len(st.Timers) != 1 || !st.Timers[0].Unreadable || !strings.Contains(st.Timers[0].Reason, "does not exist") {
		t.Fatalf("timers %+v", st.Timers)
	}
	evs, err := w.store.Events(0, func(e state.Event) bool { return e.Verb == "timer.unreadable" })
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %+v, %v", evs, err)
	}
	answer = func() ([]byte, error) {
		return nil, errors.New(`exit status 1: Error from server (NotFound): kubeadmcontrolplanes.controlplane.cluster.x-k8s.io "wc1" not found`)
	}
	tick(2)
	if got := strings.Count(out.String(), "TIMER UNREADABLE"); got != 2 || !strings.Contains(out.String(), `"wc1" not found`) {
		t.Fatalf("%d lines after a new reason, watch said %q", got, out.String())
	}
	answer = func() ([]byte, error) { return []byte(kcpV1beta2Rolling), nil }
	tick(3)
	if st, _ := w.store.Read(); len(st.Timers) != 1 || st.Timers[0].Unreadable || st.Timers[0].Reason != "4 of 3 replicas, 4 ready, 1 up to date" {
		t.Fatalf("readable again: %+v", st.Timers)
	}
	answer = func() ([]byte, error) { return []byte(kcpV1beta2Rolled), nil }
	tick(4)
	if !strings.Contains(out.String(), "TIMER: #3, "+cpWhen+" holds: look at wc1") {
		t.Fatalf("watch said %q", out.String())
	}
}

// A check reads its reference's output: a failure to read it, or output that
// is not the reference, is unreadable; a probe's non-zero exit is "not yet".
func TestCheckResultTellsUnreadableFromNotYet(t *testing.T) {
	kcp, _ := conditionCheck(cpWhen)
	pr, _ := conditionCheck(prMerged7)
	probe := checkOf(state.Timer{Probe: probeFails})
	failed := errors.New("exit status 1: boom")
	for name, tc := range map[string]struct {
		c    timerCheck
		out  string
		err  error
		want checkResult
	}{
		"a reference not read":    {kcp, "", failed, checkResult{unreadable: true, reason: "exit status 1: boom"}},
		"not the reference":       {kcp, `{"kind":"Cluster"}`, nil, checkResult{unreadable: true, reason: "not a KubeadmControlPlane: kind Cluster"}},
		"a PR not merged":         {pr, "false\n", nil, checkResult{reason: prNotMerged}},
		"a PR merged":             {pr, "true\n", nil, checkResult{holds: true}},
		"an empty answer":         {pr, "", nil, checkResult{unreadable: true, reason: "an empty answer"}},
		"a probe not holding yet": {probe, "", failed, checkResult{reason: "exit status 1: boom"}},
		"a probe holding":         {probe, "", nil, checkResult{holds: true}},
	} {
		if got := tc.c.result([]byte(tc.out), tc.err); got != tc.want {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestHelmReleaseReadyReadsItsReadyCondition(t *testing.T) {
	for obj, want := range map[string]string{
		`{"kind":"HelmRelease","metadata":{"generation":2},"status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"True"}]}}`:                             "",
		`{"kind":"HelmRelease","metadata":{"generation":2},"status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"False","message":"upgrade failed"}]}}`: "Ready False: upgrade failed",
		`{"kind":"HelmRelease","metadata":{"generation":3},"status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"True"}]}}`:                             "generation 3 not observed yet (status at 2)",
		`{"kind":"HelmRelease","metadata":{"generation":1},"status":{"observedGeneration":1}}`:                                                                                                   "no Ready condition yet",
	} {
		holds, why, err := helmReleaseReady([]byte(obj))
		if err != nil || holds != (want == "") || why != want {
			t.Errorf("%s: holds %v, %q, %v", obj, holds, why, err)
		}
	}
}

func TestProbeOutputSaysHowItFailed(t *testing.T) {
	if out, err := probeOutput(context.Background(), "echo ok"); err != nil || string(out) != "ok\n" {
		t.Fatalf("%q, %v", out, err)
	}
	_, err := probeOutput(context.Background(), "echo first >&2; echo 'error: no such context' >&2; exit 1")
	if err == nil || err.Error() != "exit status 1: error: no such context" {
		t.Fatalf("%v", err)
	}
}

// timer list -v says what each condition's last check found; timer check
// runs it now and leaves the timer as it is.
func TestTimerListAndCheckSayWhatTheCheckFound(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	a := w.app
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Timers = []state.Timer{
			{ID: 1, Due: relayNow, When: cpWhen, Checked: relayNow.UTC(), Reason: "context gone", Unreadable: true, What: "a"},
			{ID: 2, Due: relayNow, When: prMerged7, Checked: relayNow.UTC(), Reason: "not merged", What: "b"},
			{ID: 3, Due: relayNow, Probe: probeHolds, What: "c"},
			{ID: 4, Due: relayNow, What: "plain"},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.timerList(true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n    checked 02:00: cannot be read: context gone\n",
		"\n    checked 02:00: not yet: not merged\n",
		"\n    not checked yet\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in %q", want, out.String())
		}
	}
	if strings.Count(out.String(), "\n    ") != 3 {
		t.Errorf("a plain timer has a check: %q", out.String())
	}
	fakeRead(t, func(string) ([]byte, error) { return []byte(kcpV1beta2Rolled), nil })
	out.Reset()
	if err := a.timerCheck(context.Background(), "1"); err != nil || out.String() != "#1 "+cpWhen+": it holds\n" {
		t.Fatalf("%q, %v", out.String(), err)
	}
	if st, _ := a.store.Read(); !st.Timers[0].Unreadable {
		t.Fatalf("check changed the timer: %+v", st.Timers[0])
	}
	if err := a.timerCheck(context.Background(), "4"); err == nil {
		t.Fatal("checked a plain timer")
	}
	if err := a.timerCheck(context.Background(), "9"); err == nil {
		t.Fatal("checked a timer that is not open")
	}
}

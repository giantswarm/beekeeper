// Package upgrade detects the release upgrades running on an installation's
// Cluster API clusters and keeps the automatic holds that stop the
// installation's merges and lease claims while they run.
//
// A cluster's upgrade begins when its release changes: the Cluster's
// release.giantswarm.io/version label differs from the release
// cluster-api-events last recorded for it
// (giantswarm.io/last-known-cluster-upgrade-version), cluster-api-events has
// marked it upgrading (giantswarm.io/cluster-upgrading "true", set from the
// release change until its control plane and workers have rolled), or its
// scheduled upgrade is due (alpha.giantswarm.io/update-schedule-target-release
// differs from the label and update-schedule-target-time has passed). An
// upgrade that began runs on while the cluster's control plane or node pools
// have not rolled: a KubeadmControlPlane whose status.version is not its
// spec.version, or a control plane, MachinePool or MachineDeployment with
// fewer up-to-date replicas than replicas or a RollingOut condition that is
// True, or a Cluster whose own RollingOut condition is True. A cluster on an
// older release than another is not upgrading: only a
// change of its own release begins an upgrade.
package upgrade

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// HoldPrefix starts the target of an upgrade's automatic hold,
	// upgrade:<installation>/<cluster>.
	HoldPrefix = "upgrade:"

	releaseLabel      = "release.giantswarm.io/version"
	clusterNameLabel  = "cluster.x-k8s.io/cluster-name"
	lastKnownRelease  = "giantswarm.io/last-known-cluster-upgrade-version"
	upgradingMark     = "giantswarm.io/cluster-upgrading"
	upgradeStartTime  = "giantswarm.io/upgrade-start-time"
	scheduleRelease   = "alpha.giantswarm.io/update-schedule-target-release"
	scheduleTime      = "alpha.giantswarm.io/update-schedule-target-time"
	rollingOut        = "RollingOut"
	kindCluster       = "Cluster"
	kindControlPlane  = "KubeadmControlPlane"
	kindMachinePool   = "MachinePool"
	kindMachineDeploy = "MachineDeployment"
)

// Kinds are the lists read per installation, one each per reading, at the
// API version whose fields the detection reads.
var Kinds = []string{
	"clusters.v1beta2.cluster.x-k8s.io",
	"kubeadmcontrolplanes.v1beta2.controlplane.cluster.x-k8s.io",
	"machinepools.v1beta2.cluster.x-k8s.io",
	"machinedeployments.v1beta2.cluster.x-k8s.io",
}

// Object is the part of a Cluster, KubeadmControlPlane, MachinePool or
// MachineDeployment the detection reads.
type Object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Version string `json:"version,omitempty"`
	} `json:"spec"`
	Status struct {
		Version          string      `json:"version,omitempty"`
		Replicas         *int32      `json:"replicas,omitempty"`
		UpToDateReplicas *int32      `json:"upToDateReplicas,omitempty"`
		Conditions       []Condition `json:"conditions,omitempty"`
	} `json:"status"`
}

// Condition is a status condition.
type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Parse reads a `kubectl get -o json` list.
func Parse(raw []byte) ([]Object, error) {
	var l struct {
		Items []Object `json:"items"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Progress is how many replicas of a cluster's control plane or node pools
// are up to date.
type Progress struct {
	UpToDate int32 `json:"upToDate"`
	Replicas int32 `json:"replicas"`
}

func (p Progress) String() string { return fmt.Sprintf("%d/%d", p.UpToDate, p.Replicas) }

// Upgrade is a cluster's running upgrade. From is empty until it is known.
type Upgrade struct {
	Cluster      string    `json:"cluster"`
	Namespace    string    `json:"namespace"`
	From         string    `json:"from,omitempty"`
	To           string    `json:"to,omitempty"`
	Since        time.Time `json:"since,omitzero"`
	ControlPlane Progress  `json:"controlPlane"`
	NodePools    Progress  `json:"nodePools"`
	// Rolling is an upgrade whose release change is done and whose
	// rollout is not; it runs on only while its hold does.
	Rolling bool `json:"rolling,omitempty"`
}

// Detect returns the upgrades running on the clusters in objs at now, in the
// clusters' order. held says whether a cluster's upgrade hold is active: an
// upgrade that began runs on until its cluster has rolled.
func Detect(objs []Object, now time.Time, held func(cluster string) bool) []Upgrade {
	type parts struct {
		cp, pools   Progress
		rolledOut   bool
		clusterRoll bool
	}
	byCluster := map[string]*parts{}
	part := func(ns, name string) *parts {
		k := ns + "/" + name
		if byCluster[k] == nil {
			byCluster[k] = &parts{rolledOut: true}
		}
		return byCluster[k]
	}
	for _, o := range objs {
		switch o.Kind {
		case kindControlPlane, kindMachinePool, kindMachineDeploy:
			p := part(o.Metadata.Namespace, o.Metadata.Labels[clusterNameLabel])
			pr := &p.pools
			if o.Kind == kindControlPlane {
				pr = &p.cp
			}
			pr.Replicas += deref(o.Status.Replicas)
			pr.UpToDate += deref(o.Status.UpToDateReplicas)
			p.rolledOut = p.rolledOut && rolled(o)
		case kindCluster:
			part(o.Metadata.Namespace, o.Metadata.Name).clusterRoll = isTrue(o.Status.Conditions, rollingOut)
		}
	}
	var out []Upgrade
	for _, o := range objs {
		if o.Kind != kindCluster {
			continue
		}
		p := part(o.Metadata.Namespace, o.Metadata.Name)
		u := Upgrade{Cluster: o.Metadata.Name, Namespace: o.Metadata.Namespace, ControlPlane: p.cp, NodePools: p.pools}
		if !begun(o, now, &u) {
			if !held(u.Cluster) || (p.rolledOut && !p.clusterRoll) {
				continue
			}
			u.Rolling = true
		}
		out = append(out, u)
	}
	return out
}

// begun reports whether the cluster's release is changing and fills in the
// releases and since.
func begun(o Object, now time.Time, u *Upgrade) bool {
	a := o.Metadata.Annotations
	label := o.Metadata.Labels[releaseLabel]
	u.Since, _ = time.Parse(time.RFC3339, a[upgradeStartTime])
	switch last := a[lastKnownRelease]; {
	case label != "" && last != "" && label != last:
		u.From, u.To = last, label
	case a[upgradingMark] == "true":
		u.To = label
	default:
		t, err := time.Parse(time.RFC3339, a[scheduleTime])
		if target := a[scheduleRelease]; err != nil || target == "" || target == label || now.Before(t) {
			return false
		}
		u.From, u.To, u.Since = label, a[scheduleRelease], t
	}
	return true
}

// rolled reports whether a control plane or node pool has rolled out.
func rolled(o Object) bool {
	if o.Kind == kindControlPlane && o.Status.Version != o.Spec.Version {
		return false
	}
	if o.Status.UpToDateReplicas != nil && deref(o.Status.UpToDateReplicas) < deref(o.Status.Replicas) {
		return false
	}
	return !isTrue(o.Status.Conditions, rollingOut)
}

func isTrue(cs []Condition, t string) bool {
	return slices.ContainsFunc(cs, func(c Condition) bool { return c.Type == t && c.Status == "True" })
}

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// HoldTarget is the hold target of a cluster's upgrade.
func HoldTarget(installation, cluster string) string {
	return HoldPrefix + installation + "/" + cluster
}

// Describe is an upgrade in words: <installation>/<cluster> <from> → <to>.
func Describe(installation string, u Upgrade) string {
	return fmt.Sprintf("%s/%s %s → %s", installation, u.Cluster, orUnknown(u.From), orUnknown(u.To))
}

func orUnknown(v string) string {
	if v == "" {
		return "?"
	}
	return v
}

// reasonHead starts an upgrade hold's reason.
const reasonHead = "upgrade "

// Line is a hold's begin line, UPGRADE <installation>/<cluster> <from> → <to>.
func Line(h state.Hold) string { return "UPGRADE " + strings.TrimPrefix(h.Reason, reasonHead) }

// EndLine is a hold's end line.
func EndLine(h state.Hold) string {
	return "UPGRADE ENDED " + strings.TrimPrefix(h.Reason, reasonHead) + " (since " + h.At.Local().Format("15:04") + ")"
}

// Is reports whether a hold is an upgrade's automatic hold.
func Is(h state.Hold) bool { return strings.HasPrefix(h.Target, HoldPrefix) }

// Held returns an active upgrade hold of the installation.
func Held(st *state.State, installation string, now time.Time) (state.Hold, bool) {
	if installation == "" {
		return state.Hold{}, false
	}
	for _, h := range st.Holds {
		if strings.HasPrefix(h.Target, HoldPrefix+installation+"/") && h.Active(now) {
			return h, true
		}
	}
	return state.Hold{}, false
}

// HeldClusters says which clusters of which installation hold an upgrade,
// lifted or not: an upgrade whose hold was lifted still runs.
func HeldClusters(st *state.State, now time.Time) func(installation, cluster string) bool {
	held := map[string]bool{}
	for _, h := range st.Holds {
		if Is(h) && !h.Expired(now) {
			held[h.Target] = true
		}
	}
	return func(installation, cluster string) bool { return held[HoldTarget(installation, cluster)] }
}

// Reconcile makes the installation's upgrade holds those of its running
// upgrades: a hold for each that has none (begun), none for any that no
// longer runs (ended). A lifted hold stays lifted while its upgrade runs; a
// different upgrade of the cluster (another target release) replaces it with
// an active hold. Only a readable installation is reconciled.
func Reconcile(st *state.State, installation string, running []Upgrade, now time.Time, by state.Party) (begun, ended []state.Hold) {
	want := map[string]Upgrade{}
	for _, u := range running {
		want[HoldTarget(installation, u.Cluster)] = u
	}
	have := map[string]bool{}
	st.Holds = slices.DeleteFunc(st.Holds, func(h state.Hold) bool {
		if !strings.HasPrefix(h.Target, HoldPrefix+installation+"/") {
			return false
		}
		u, runs := want[h.Target]
		switch {
		case runs && h.Active(now):
			have[h.Target] = true
			return false
		case runs && h.LiftedBy != nil && !h.Expired(now) && sameUpgrade(h, u):
			have[h.Target] = true
			return false
		case runs && h.LiftedBy != nil:
			return true // a different upgrade: its hold begins below
		}
		ended = append(ended, h)
		return true
	})
	for i, h := range st.Holds {
		if u, ok := want[h.Target]; ok && h.UpgradeTo == "" && !u.Rolling {
			st.Holds[i].UpgradeTo = u.To
		}
	}
	for _, u := range running {
		t := HoldTarget(installation, u.Cluster)
		if have[t] || u.Rolling {
			continue
		}
		h := state.Hold{Target: t, Reason: reasonHead + Describe(installation, u), By: by, At: now.UTC(), UpgradeTo: u.To}
		st.Holds = append(st.Holds, h)
		begun = append(begun, h)
	}
	return begun, ended
}

// sameUpgrade reports whether a running upgrade is the one the hold stands
// for: a rollout that runs on (its release change done) or the same target
// release.
func sameUpgrade(h state.Hold, u Upgrade) bool {
	return u.Rolling || u.To == "" || h.UpgradeTo == "" || u.To == h.UpgradeTo
}

// Lift records a person's lift of an upgrade hold: the hold stays, inactive,
// until its upgrade ends. It reports whether the target held an active hold.
func Lift(st *state.State, target string, by state.Party, now time.Time) bool {
	for i, h := range st.Holds {
		if h.Target == target && h.Active(now) {
			st.Holds[i].LiftedBy, st.Holds[i].LiftedAt = &by, now.UTC()
			return true
		}
	}
	return false
}

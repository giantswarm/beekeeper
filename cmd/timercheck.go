package cmd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// timerCondition is a typed condition of timer add --when: the reference it
// names (form, matched by ref), the command that reads it and what its
// output says of the reference.
type timerCondition struct {
	form   string
	ref    *regexp.Regexp
	github bool
	read   func(m []string) string
	judge  func(m []string) judge
}

// judge says whether a reference's output holds the condition and, if not,
// why; an error is output it cannot read.
type judge func(out []byte) (bool, string, error)

var (
	ghRef   = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)
	kubeRef = regexp.MustCompile(`^([\w.@:-]+)/([a-z0-9][a-z0-9.-]*)/([a-z0-9][a-z0-9.-]*)$`)
	// hrRef is a kubeRef with the chart version it waits for, optional.
	hrRef = regexp.MustCompile(`^([\w.@:-]+)/([a-z0-9][a-z0-9.-]*)/([a-z0-9][a-z0-9.-]*)(?:\s+(v?[0-9][\w.+-]*))?$`)
)

// always is the judge of a condition whose reference adds nothing to it.
func always(j judge) func([]string) judge { return func([]string) judge { return j } }

// prNotMerged is why a pr-merged condition does not hold yet.
const prNotMerged = "not merged"

// timerConditions are the typed conditions by kind. Each is one read of one
// reference, so the timers on the same reference share it.
var timerConditions = map[string]timerCondition{
	"pr-merged": {"owner/repo#n", ghRef, true, func(m []string) string {
		return fmt.Sprintf("gh api repos/%s/pulls/%s --jq .merged", m[1], m[2])
	}, always(outputIs("true", prNotMerged))},
	"issue-closed": {"owner/repo#n", ghRef, true, func(m []string) string {
		return fmt.Sprintf("gh api repos/%s/issues/%s --jq .state", m[1], m[2])
	}, always(outputIs("closed", "open"))},
	"helmrelease-ready": {"context/namespace/name [version]", hrRef, false, func(m []string) string {
		return fmt.Sprintf("kubectl --context %s -n %s get helmreleases.helm.toolkit.fluxcd.io %s -o json", m[1], m[2], m[3])
	}, func(m []string) judge { return helmReleaseReady(m[4]) }},
	"controlplane-ready": {"context/namespace/name", kubeRef, false, func(m []string) string {
		return fmt.Sprintf("kubectl --context %s -n %s get kubeadmcontrolplanes.controlplane.cluster.x-k8s.io %s -o json", m[1], m[2], m[3])
	}, always(controlPlaneReady)},
}

// timerCheck is how a timer's condition is checked: the command that reads
// it, whether it reads GitHub and, for a typed condition, what its output
// says; a --probe's exit code says it itself.
type timerCheck struct {
	cmd    string
	github bool
	judge  judge
}

// checkResult is one check of a timer's condition: whether it holds and, if
// not, why, or why its reference could not be read.
type checkResult struct {
	holds, unreadable bool
	reason            string
}

// result judges the output and error of c's command.
func (c timerCheck) result(out []byte, err error) checkResult {
	switch {
	case err != nil && c.judge == nil:
		// A probe that exits non-zero says "not yet".
		return checkResult{reason: err.Error()}
	case err != nil:
		return checkResult{unreadable: true, reason: err.Error()}
	case c.judge == nil:
		return checkResult{holds: true}
	}
	holds, why, err := c.judge(out)
	if err != nil {
		return checkResult{unreadable: true, reason: err.Error()}
	}
	return checkResult{holds: holds, reason: why}
}

// conditionCheck is the check of a typed condition "<kind> <ref>".
func conditionCheck(when string) (timerCheck, error) {
	kind, ref, _ := strings.Cut(strings.TrimSpace(when), " ")
	c, ok := timerConditions[kind]
	if !ok {
		return timerCheck{}, usageErr("--when %q: a condition is one of %s, then its reference; --probe takes a command", when, strings.Join(slices.Sorted(maps.Keys(timerConditions)), ", "))
	}
	m := c.ref.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return timerCheck{}, usageErr("--when %q: %s names %s", when, kind, c.form)
	}
	return timerCheck{c.read(m), c.github, c.judge(m)}, nil
}

// checkOf is the check of t's condition; without a command for a timer
// without one.
func checkOf(t state.Timer) timerCheck {
	if t.Probe != "" {
		return timerCheck{cmd: t.Probe}
	}
	if t.When == "" {
		return timerCheck{}
	}
	c, err := conditionCheck(t.When)
	if err != nil {
		return timerCheck{}
	}
	return c
}

// readProbe runs a timer's check command and returns its output; a seam for
// the tests.
var readProbe = probeOutput

// probeOutput runs cmd within probeTimeout and returns its output; its error
// says how it failed in the command's last line on stderr.
func probeOutput(ctx context.Context, cmd string) ([]byte, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var stderr bytes.Buffer
	c := exec.CommandContext(pctx, "sh", "-c", cmd) //nolint:gosec // the condition its timer's setter gave on this machine
	c.Stderr = &stderr
	out, err := c.Output()
	switch {
	case err == nil:
		return out, nil
	case errors.Is(pctx.Err(), context.DeadlineExceeded):
		return out, fmt.Errorf("no answer within %s", probeTimeout)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return out, fmt.Errorf("%v: %s", err, truncate(last, 200))
	}
	return out, err
}

// outputIs judges an output of one line: want holds, anything else is why
// not, "false" said as not.
func outputIs(want, not string) judge {
	return func(out []byte) (bool, string, error) {
		switch got := strings.TrimSpace(string(out)); got {
		case want:
			return true, "", nil
		case "":
			return false, "", errors.New("an empty answer")
		case "false":
			return false, not, nil
		default:
			return false, got, nil
		}
	}
}

// kubeCondition is a Kubernetes status condition.
type kubeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// conditionOf is the condition of type typ among cs, nil without one.
func conditionOf(cs []kubeCondition, typ string) *kubeCondition {
	for i := range cs {
		if cs[i].Type == typ {
			return &cs[i]
		}
	}
	return nil
}

// generations are an object's generation and the one its status reports
// on: a status behind its spec says nothing about it yet.
type generations struct {
	Generation         int64
	ObservedGeneration int64
}

// stale says why the status is behind the spec; empty when it is current.
func (g generations) stale() string {
	if g.ObservedGeneration < g.Generation {
		return fmt.Sprintf("generation %d not observed yet (status at %d)", g.Generation, g.ObservedGeneration)
	}
	return ""
}

// helmReleaseReady holds once a Flux HelmRelease's status reports on its
// spec, its Ready condition is true and, given a version, it runs that
// chart version. The version it runs is the newest release of its history,
// or without one the attempted revision while Ready: current Flux leaves
// the applied revision empty.
func helmReleaseReady(version string) judge {
	return func(out []byte) (bool, string, error) {
		var o struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				History            []struct {
					ChartVersion string `json:"chartVersion"`
				} `json:"history"`
				LastAttemptedRevision string          `json:"lastAttemptedRevision"`
				Conditions            []kubeCondition `json:"conditions"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &o); err != nil || o.Kind != "HelmRelease" {
			return false, "", fmt.Errorf("not a HelmRelease: %v", cmp.Or[any](err, "kind "+o.Kind))
		}
		if s := (generations{o.Metadata.Generation, o.Status.ObservedGeneration}).stale(); s != "" {
			return false, s, nil
		}
		c := conditionOf(o.Status.Conditions, "Ready")
		ready := c != nil && c.Status == "True"
		var rolled string
		switch {
		case len(o.Status.History) > 0:
			rolled = merge.Bare(o.Status.History[0].ChartVersion)
		case ready:
			rolled = merge.Bare(o.Status.LastAttemptedRevision)
		}
		switch {
		case version != "" && rolled == "":
			return false, "no release yet, waiting for " + merge.Bare(version), nil
		case version != "" && rolled != merge.Bare(version):
			return false, "runs " + rolled + ", not " + merge.Bare(version), nil
		case c == nil:
			return false, "no Ready condition yet", nil
		case ready:
			return true, "", nil
		}
		return false, "Ready " + c.Status + ": " + cmp.Or(c.Message, c.Reason), nil
	}
}

// controlPlaneReady holds once a KubeadmControlPlane's status reports on its
// spec and either it has spec.replicas replicas, every one ready and up to
// date, or its Ready condition is true. It reads both Cluster API versions:
// v1beta1 (updatedReplicas, the v1beta2 counts its status may carry, and the
// Ready summary, false while a machine is not up to date) and v1beta2
// (upToDateReplicas, no Ready condition).
func controlPlaneReady(out []byte) (bool, string, error) {
	var o struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int64 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64           `json:"observedGeneration"`
			Replicas           int64           `json:"replicas"`
			ReadyReplicas      int64           `json:"readyReplicas"`
			UpToDateReplicas   *int64          `json:"upToDateReplicas"`
			UpdatedReplicas    *int64          `json:"updatedReplicas"`
			Conditions         []kubeCondition `json:"conditions"`
			V1Beta2            struct {
				UpToDateReplicas *int64 `json:"upToDateReplicas"`
			} `json:"v1beta2"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &o); err != nil || o.Kind != "KubeadmControlPlane" {
		return false, "", fmt.Errorf("not a KubeadmControlPlane: %v", cmp.Or[any](err, "kind "+o.Kind))
	}
	s := o.Status
	if st := (generations{o.Metadata.Generation, s.ObservedGeneration}).stale(); st != "" {
		return false, st, nil
	}
	want := int64(1) // the API's default
	if o.Spec.Replicas != nil {
		want = *o.Spec.Replicas
	}
	var upToDate int64
	for _, n := range []*int64{s.UpToDateReplicas, s.UpdatedReplicas, s.V1Beta2.UpToDateReplicas} {
		if n != nil {
			upToDate = *n
			break
		}
	}
	if s.Replicas == want && s.ReadyReplicas == want && upToDate == want {
		return true, "", nil
	}
	if c := conditionOf(s.Conditions, "Ready"); c != nil && c.Status == "True" {
		return true, "", nil
	}
	return false, fmt.Sprintf("%d of %d replicas, %d ready, %d up to date", s.Replicas, want, s.ReadyReplicas, upToDate), nil
}

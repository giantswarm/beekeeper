package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/state"
)

// verbLeaseClaim is the event a granted claim logs.
const verbLeaseClaim = "lease.claim"

// labView is a lab lease with the kind cluster it stands for.
type labView struct {
	Lease   string `json:"lease"`
	Cluster string `json:"cluster"`
	Running bool   `json:"running"`
	// Holder is the lease's holder as `lease list` names it, "" when free.
	Holder string `json:"holder,omitempty"`
}

// labs is what `lease list` reports of the kind clusters: each lab lease
// with its cluster, and the running clusters no lab lease maps.
type labs struct {
	Labs     []labView `json:"labs,omitempty"`
	Unmapped []string  `json:"unmappedClusters,omitempty"`
	// Err is set when docker cannot be asked which clusters run.
	Err string `json:"clustersError,omitempty"`
}

// runningClusters lists the names of the running kind clusters.
func (a *app) runningClusters() ([]string, error) {
	if a.kindClusters != nil {
		return a.kindClusters()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, err := machine.KindClusters(ctx)
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names, err
}

// readLabs maps the configured lab leases onto the running kind clusters;
// held names each held lease's holder. Without lab leases it asks nothing.
func (a *app) readLabs(held map[string]string) labs {
	if len(a.cfg.Labs) == 0 {
		return labs{}
	}
	running, err := a.runningClusters()
	if err != nil {
		return labs{Err: err.Error()}
	}
	var l labs
	for _, res := range a.cfg.Resources {
		if cl := a.cfg.LabCluster(res); cl != "" {
			l.Labs = append(l.Labs, labView{Lease: res, Cluster: cl, Running: slices.Contains(running, cl), Holder: held[res]})
		}
	}
	for _, cl := range running {
		if a.cfg.LabLease(cl) == "" {
			l.Unmapped = append(l.Unmapped, cl)
		}
	}
	return l
}

func (a *app) printLabs(l labs) {
	if l.Err != "" {
		_, _ = fmt.Fprintf(a.out, "labs: kind clusters unknown (%s)\n", truncate(l.Err, 60))
		return
	}
	if len(l.Labs) == 0 && len(l.Unmapped) == 0 {
		return
	}
	w := a.table()
	_, _ = fmt.Fprintln(w, "LAB\tKIND CLUSTER\tCLUSTER\tLEASE")
	for _, v := range l.Labs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Lease, v.Cluster, runningText(v.Running), holderText(v.Holder))
	}
	for _, cl := range l.Unmapped {
		_, _ = fmt.Fprintf(w, "-\t%s\t%s\tunmapped: no lab lease stands for it\n", cl, runningText(true))
	}
	_ = w.Flush()
}

func runningText(running bool) string {
	if running {
		return "running"
	}
	return "not running"
}

func holderText(holder string) string {
	if holder == "" {
		return "free"
	}
	return fmt.Sprintf("held by %q", holder)
}

// claimedLab is what a claim of a lab lease says of its kind cluster, ""
// for any other resource.
func (a *app) claimedLab(res string) string {
	cl := a.cfg.LabCluster(res)
	if cl == "" {
		return ""
	}
	running, err := a.runningClusters()
	switch {
	case err != nil:
		return fmt.Sprintf(" (kind cluster %s)", cl)
	case slices.Contains(running, cl):
		return fmt.Sprintf(" (kind cluster %s, running)", cl)
	}
	return fmt.Sprintf(" (kind cluster %s, not running)", cl)
}

// clusterNotes says per running kind cluster which lab lease stands for
// it and whether that is held: a running cluster whose lease is free is
// idle, named with its last holder when the event log knows one. Without
// lab leases there are no notes.
func (a *app) clusterNotes(clusters []machine.Cluster, holders []lease.Holder, events []state.Event) map[string]string {
	if len(a.cfg.Labs) == 0 {
		return nil
	}
	held := map[string]string{}
	for _, h := range holders {
		held[h.Env] = h.Name
	}
	notes := map[string]string{}
	for _, c := range clusters {
		res := a.cfg.LabLease(c.Name)
		switch {
		case res == "":
			notes[c.Name] = "unmapped: no lab lease stands for it"
		case held[res] != "":
			notes[c.Name] = fmt.Sprintf("%s, held by %q", res, held[res])
		default:
			notes[c.Name] = "idle: " + res + " is free"
			if who := lastHolder(events, res); who != "" {
				notes[c.Name] += fmt.Sprintf(", last held by %q", who)
			}
		}
	}
	return notes
}

// lastHolder is who claimed res last in events, "" when none did.
func lastHolder(events []state.Event, res string) string {
	for _, e := range slices.Backward(events) {
		if e.Verb == verbLeaseClaim && strings.HasPrefix(e.Detail, res+":") {
			return e.By.Name
		}
	}
	return ""
}

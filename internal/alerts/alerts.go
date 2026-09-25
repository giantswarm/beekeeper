// Package alerts reads the alerts of the installations the sessions work on
// and turns two readings into NEW and RESOLVED lines.
//
// This file is the pure half: an Alertmanager answer becomes a set keyed by
// fingerprint, two sets become change lines, one set becomes the grouped
// snapshot. Reading an Alertmanager is read.go, the baseline is state.go.
package alerts

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Page is the severity that pages the on-call person; it is printed in capitals.
const Page = "page"

// Severities are the severities from lowest to highest, as Giant Swarm's
// rules (none, notify, page) and kube-prometheus-stack's (info, warning,
// critical) set them. A floor is one of them; an alert whose severity is
// none of them is never below a floor.
var Severities = []string{"none", "info", "warning", "notify", "critical", "page"}

// Below reports whether severity ranks under floor.
func Below(severity, floor string) bool {
	s := slices.Index(Severities, severity)
	return s >= 0 && s < slices.Index(Severities, floor)
}

// objectLabels name the alerting object, most specific first. pod, node and
// job come last and name the object only when nothing more specific does (a
// restarting pod, a node without its agent, a failing scrape job) and the pod
// is not the exporter's own: an alert on a metric without a pod label carries
// the scrape target's, whose name starts with the alert's service.
var objectLabels = []string{"deployment", "statefulset", "daemonset", "cronjob", "job_name", "name",
	"certificatename", "persistentvolumeclaim", "horizontalpodautoscaler", "exported_pod",
	"gvk", "pod", "node", "job"}

var scrapeTarget = []string{"pod", "node", "job"}

// Raw is one alert as the Alertmanager v2 API returns it.
type Raw struct {
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	StartsAt    string            `json:"startsAt"`
}

// Alert is what a line prints of one alert.
type Alert struct {
	Severity  string `json:"severity"`
	Team      string `json:"team"`
	Alertname string `json:"alertname"`
	Cluster   string `json:"cluster"`
	Where     string `json:"where"`
	Since     string `json:"since"`
}

// Answer is one installation's reading: its alerts, or why it did not answer.
type Answer struct {
	OK     bool
	Alerts []Raw
	Why    string
}

// Rules are the settings the lines depend on.
type Rules struct {
	// Ignore are alert names that never appear.
	Ignore []string
	// Team is the team whose alerts are marked in capitals and counted.
	Team string
	// Collapse is the number of changes of one alertname in one run above
	// which they are one line with a count.
	Collapse int
	// Floors are the lowest severity printed, per installation. An alert
	// below its floor stays in the baseline and is never printed, so a
	// changed floor prints no burst of NEW or RESOLVED lines.
	Floors map[string]string
	// Flap is the flap damper.
	Flap Damper
}

// Damper holds back an alert that changes too often: its Changes-th NEW or
// RESOLVED within Window is one FLAPPING line, and its changes print nothing
// until it has been stable for Window. Changes under 2 is no damper.
type Damper struct {
	Changes int
	Window  time.Duration
}

// Flap is the damper's memory of one alert: its changes within the window,
// and whether it was reported flapping.
type Flap struct {
	Changes  []time.Time `json:"changes"`
	Flapping bool        `json:"flapping,omitempty"`
}

// shown reports whether an installation's alert is at or above its floor.
func (r Rules) shown(installation string, a Alert) bool {
	return !Below(a.Severity, r.Floors[installation])
}

// hidden is the count of the set's alerts below the installation's floor, as
// a clause of a head line.
func (r Rules) hidden(installation string, s Set) string {
	n := 0
	for _, a := range s {
		if !r.shown(installation, a) {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d below the floor %s", n, r.Floors[installation])
}

// Set is an installation's alerts by fingerprint.
type Set map[string]Alert

// Normalize turns an answer's alerts into the set, ignored names dropped.
func (r Rules) Normalize(raw []Raw, installation string) Set {
	out, _ := r.normalize(raw, installation)
	return out
}

// normalize also returns the fingerprints in the answer's order.
func (r Rules) normalize(raw []Raw, installation string) (Set, []string) {
	out, order := Set{}, []string{}
	for _, a := range raw {
		l := a.Labels
		if slices.Contains(r.Ignore, l["alertname"]) {
			continue
		}
		cluster := first(l["cluster_id"], l["cluster"], l["installation"], installation)
		namespace := first(l["exported_namespace"], l["namespace"])
		exporter := l["service"] != "" && strings.HasPrefix(l["pod"], l["service"])
		obj := ""
		for _, k := range objectLabels {
			if l[k] != "" && (!exporter || !slices.Contains(scrapeTarget, k)) {
				obj = l[k]
				break
			}
		}
		where := namespace + "/" + obj
		if namespace == "" || obj == "" {
			where = namespace + obj
		}
		if where == "" {
			where = "-"
		}
		if cluster != installation {
			where += "@" + cluster
		}
		key := a.Fingerprint
		if key == "" {
			b, _ := json.Marshal(l)
			key = string(b)
		}
		if _, seen := out[key]; !seen {
			order = append(order, key)
		}
		out[key] = Alert{
			Severity: first(l["severity"], "-"), Team: first(l["team"], "-"),
			Alertname: first(l["alertname"], "-"), Cluster: cluster, Where: where, Since: a.StartsAt,
		}
	}
	return out, order
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// mark prints a page and the team's alerts in capitals.
func (r Rules) mark(a Alert) (string, string) {
	severity, team := a.Severity, a.Team
	if severity == Page {
		severity = strings.ToUpper(severity)
	}
	if r.Team != "" && team == r.Team {
		team = strings.ToUpper(team)
	}
	return severity, team
}

// rank orders pages first, then the team's alerts, then by name.
func (r Rules) rank(a, b Alert) int {
	return cmp.Or(
		cmpBool(a.Severity != Page, b.Severity != Page),
		cmpBool(r.Team == "" || a.Team != r.Team, r.Team == "" || b.Team != r.Team),
		strings.Compare(a.Alertname, b.Alertname))
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// sinceText is the start as HH:MMZ today, else MM-DD HH:MMZ.
func sinceText(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "?"
	}
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04Z")
	}
	return t.Format("01-02 15:04Z")
}

// changeLines is one line per alert, or one line per alertname when more
// than collapse of them changed together.
func (r Rules) changeLines(kind, installation string, alerts []Alert, now time.Time, collapse int) []string {
	alerts = slices.Clone(alerts)
	slices.SortStableFunc(alerts, func(a, b Alert) int {
		return cmp.Or(r.rank(a, b), strings.Compare(a.Where, b.Where))
	})
	type key struct{ alertname, severity, team string }
	var order []key
	groups := map[key][]Alert{}
	for _, a := range alerts {
		k := key{a.Alertname, a.Severity, a.Team}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], a)
	}
	var lines []string
	for _, k := range order {
		group := groups[k]
		severity, team := r.mark(group[0])
		if len(group) > collapse {
			earliest := slices.MinFunc(group, func(a, b Alert) int { return strings.Compare(a.Since, b.Since) }).Since
			lines = append(lines, fmt.Sprintf("ALERT %s %s %s %s %s x%d (%s, ...) since %s",
				kind, installation, severity, team, k.alertname, len(group), group[0].Where, sinceText(earliest, now)))
			continue
		}
		for _, a := range group {
			lines = append(lines, fmt.Sprintf("ALERT %s %s %s %s %s %s since %s",
				kind, installation, severity, team, a.Alertname, a.Where, sinceText(a.Since, now)))
		}
	}
	return lines
}

// Installation is an installation's baseline: its last set, kept while it
// does not answer. A nil Alerts means it has not been seen yet.
type Installation struct {
	Reachable bool `json:"reachable"`
	Alerts    Set  `json:"alerts"`
	// Flaps are the damper's records of the alerts that changed within its
	// window, by fingerprint.
	Flaps map[string]*Flap `json:"flaps,omitempty"`
}

// Step returns the lines one watch run prints for an installation and the
// baseline it leaves. prev is nil on the installation's first run.
func (r Rules) Step(installation string, prev *Installation, ans Answer, now time.Time) ([]string, *Installation) {
	if !ans.OK {
		next := &Installation{}
		if prev != nil {
			next.Alerts, next.Flaps = prev.Alerts, prev.Flaps
			if !prev.Reachable {
				return nil, next
			}
		}
		return []string{fmt.Sprintf("ALERTS %s unreachable: %s", installation, ans.Why)}, next
	}
	current := r.Normalize(ans.Alerts, installation)
	next := &Installation{Reachable: true, Alerts: current}
	if prev == nil || prev.Alerts == nil {
		return r.firstLook(installation, current, now), next
	}
	var lines []string
	if !prev.Reachable {
		lines = append(lines, fmt.Sprintf("ALERTS %s reachable again", installation))
	}
	next.Flaps = r.Flap.keep(prev.Flaps, now)
	added, flapping := r.Flap.damp(next.Flaps, r.missing(installation, current, prev.Alerts), now)
	gone, flappingGone := r.Flap.damp(next.Flaps, r.missing(installation, prev.Alerts, current), now)
	lines = append(lines, r.changeLines(New, installation, added, now, r.Collapse)...)
	lines = append(lines, r.changeLines("RESOLVED", installation, gone, now, r.Collapse)...)
	lines = append(lines, r.changeLines("FLAPPING", installation, append(flapping, flappingGone...), now, r.Collapse)...)
	return lines, next
}

// New is the kind of a line about an alert that started firing.
const New = "NEW"

// IsNew reports whether a line of Step is about an alert that started firing.
func IsNew(line string) bool { return strings.HasPrefix(line, "ALERT "+New+" ") }

// keep returns copies of the records of the alerts that changed within the
// window: an alert stable for the window is forgotten, and a flapping one is
// released, so its next change prints again.
func (d Damper) keep(flaps map[string]*Flap, now time.Time) map[string]*Flap {
	out := map[string]*Flap{}
	for fp, f := range flaps {
		if n := len(f.Changes); n > 0 && now.Sub(f.Changes[n-1]) < d.Window {
			out[fp] = &Flap{Changes: slices.Clone(f.Changes), Flapping: f.Flapping}
		}
	}
	return out
}

// damp records the change of every alert in changed and splits them into
// those that print and those that start flapping now, the latter since their
// first change within the window. The change of an alert that is flapping
// already is neither.
func (d Damper) damp(flaps map[string]*Flap, changed map[string]Alert, now time.Time) (print, flapping []Alert) {
	for fp, a := range changed {
		if d.Changes < 2 {
			print = append(print, a)
			continue
		}
		f := flaps[fp]
		if f == nil {
			f = &Flap{}
			flaps[fp] = f
		}
		f.Changes = append(slices.DeleteFunc(f.Changes, func(t time.Time) bool { return now.Sub(t) >= d.Window }), now)
		switch {
		case f.Flapping:
		case len(f.Changes) >= d.Changes:
			f.Flapping = true
			a.Since = f.Changes[0].UTC().Format(time.RFC3339Nano)
			flapping = append(flapping, a)
		default:
			print = append(print, a)
		}
	}
	return print, flapping
}

func (r Rules) firstLook(installation string, current Set, now time.Time) []string {
	pages, mine := 0, 0
	all := make([]Alert, 0, len(current))
	for _, a := range current {
		if !r.shown(installation, a) {
			continue
		}
		all = append(all, a)
		if a.Severity == Page {
			pages++
		}
		if r.Team != "" && a.Team == r.Team {
			mine++
		}
	}
	head := fmt.Sprintf("ALERTS %s first look: %d active, %d page", installation, len(all), pages)
	if r.Team != "" {
		head += fmt.Sprintf(", %d %s", mine, r.Team)
	}
	head += r.hidden(installation, current)
	return append([]string{head}, r.changeLines("OPEN", installation, all, now, 1)...)
}

// missing are the alerts of a at or above the installation's floor whose
// fingerprint b lacks.
func (r Rules) missing(installation string, a, b Set) map[string]Alert {
	out := map[string]Alert{}
	for k, v := range a {
		if _, ok := b[k]; !ok && r.shown(installation, v) {
			out[k] = v
		}
	}
	return out
}

// SnapshotLines is the current set grouped by severity, team, alertname and
// cluster, pages and the team's alerts first.
func (r Rules) SnapshotLines(installation string, ans Answer, now time.Time) []string {
	if !ans.OK {
		return []string{fmt.Sprintf("%s unreachable: %s", installation, ans.Why)}
	}
	current, order := r.normalize(ans.Alerts, installation)
	shown := 0
	type group struct {
		Alert
		n int
	}
	var groups []*group
	byKey := map[[4]string]*group{}
	for _, fp := range order {
		a := current[fp]
		if !r.shown(installation, a) {
			continue
		}
		shown++
		k := [4]string{a.Severity, a.Team, a.Alertname, a.Cluster}
		if g, ok := byKey[k]; ok {
			g.n++
			continue
		}
		byKey[k] = &group{Alert: a, n: 1}
		groups = append(groups, byKey[k])
	}
	slices.SortStableFunc(groups, func(a, b *group) int {
		return cmp.Or(
			cmpBool(a.Severity != Page, b.Severity != Page),
			cmpBool(r.Team == "" || a.Team != r.Team, r.Team == "" || b.Team != r.Team),
			cmp.Compare(b.n, a.n),
			strings.Compare(a.Alertname, b.Alertname))
	})
	lines := []string{fmt.Sprintf("%s at %s: %d active%s", installation, now.UTC().Format("15:04Z"), shown, r.hidden(installation, current))}
	for _, g := range groups {
		s, t := r.mark(g.Alert)
		lines = append(lines, fmt.Sprintf("  %-7s %-11s %-50s %-12s %3d", s, t, g.Alertname, g.Cluster, g.n))
	}
	return lines
}

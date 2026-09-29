package alerts

import (
	"slices"
	"strings"
	"time"
)

// Quiet is a rule whose alerts' changes wake nobody: the watch keeps their
// lines out of its output and logs them instead. Every field is a glob (*
// matches any run of characters); an empty one matches everything. A rule
// never quiets an alert of the team, one on an installation in play, or a
// page, unless the rule names a cluster: a page on another team's test
// cluster is that team's.
type Quiet struct {
	Installation string `yaml:"installation" json:"installation,omitempty"`
	Cluster      string `yaml:"cluster" json:"cluster,omitempty"`
	Alertname    string `yaml:"alertname" json:"alertname,omitempty"`
	Severity     string `yaml:"severity" json:"severity,omitempty"`
}

// String names the rule in a quiet line.
func (q Quiet) String() string {
	var parts []string
	for _, f := range [][2]string{{"installation", q.Installation}, {"cluster", q.Cluster}, {"alertname", q.Alertname}, {"severity", q.Severity}} {
		if f[1] != "" {
			parts = append(parts, f[0]+" "+f[1])
		}
	}
	if len(parts) == 0 {
		return "every alert"
	}
	return strings.Join(parts, ", ")
}

func (q Quiet) matches(installation string, a Alert) bool {
	return Glob(q.Installation, installation) && Glob(q.Cluster, a.Cluster) &&
		Glob(q.Alertname, a.Alertname) && Glob(q.Severity, a.Severity) &&
		(a.Severity != Page || q.Cluster != "")
}

// Glob reports whether s matches pattern, in which * matches any run of
// characters, slashes included; an empty pattern matches everything.
func Glob(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return s == pattern
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// Repeat is the reason of a quiet NEW line whose alert resolved within the
// damper's window with the same start: Alertmanager keeps an alert's start
// while it fires, so the RESOLVED was a reading that missed it (an
// Alertmanager restart, a reconnect), not an end, and its NEW was said.
const Repeat = "repeat of an alert that never ended (same start)"

// resolvedKeep is how long a resolved alert is remembered for Repeat when
// the damper has no window.
const resolvedKeep = time.Hour

// quiet is why an alert's change wakes nobody, or "" when it does. kind is
// the line's kind; resolved is what the installation's baseline remembers
// of resolved alerts.
func (r Rules) quiet(installation, kind, fp string, a Alert, resolved map[string]Resolved) string {
	if r.Team != "" && a.Team == r.Team {
		return ""
	}
	if r.InPlay != nil && r.InPlay(installation) {
		return ""
	}
	for _, q := range r.Quiet {
		if q.matches(installation, a) {
			return "quiet rule " + q.String()
		}
	}
	if kind == New && a.Severity != Page {
		if p, ok := resolved[fp]; ok && p.Since == a.Since {
			return Repeat
		}
	}
	return ""
}

// Resolved is a resolved alert the baseline remembers for Repeat: its start
// and when it resolved.
type Resolved struct {
	Since string    `json:"since"`
	At    time.Time `json:"at"`
}

// recentResolved are the resolved alerts of prev within the damper's
// window.
func (r Rules) recentResolved(prev map[string]Resolved, now time.Time) map[string]Resolved {
	window := r.Flap.Window
	if window <= 0 {
		window = resolvedKeep
	}
	out := map[string]Resolved{}
	for fp, p := range prev {
		if now.Sub(p.At) < window {
			out[fp] = p
		}
	}
	return out
}

// nextResolved are the recent resolved alerts less those back, with those
// gone now.
func nextResolved(recent map[string]Resolved, gone, back map[string]Alert, now time.Time) map[string]Resolved {
	out := map[string]Resolved{}
	for fp, p := range recent {
		if _, ok := back[fp]; !ok {
			out[fp] = p
		}
	}
	for fp, a := range gone {
		out[fp] = Resolved{Since: a.Since, At: now}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// split divides one kind's changes into those that wake and the quiet ones,
// each quiet line ending in its reason.
func (r Rules) split(kind, installation string, changed []Alert, resolved map[string]Resolved, now time.Time) (loud []Alert, quiet []string) {
	byReason := map[string][]Alert{}
	var reasons []string
	for _, a := range changed {
		why := r.quiet(installation, kind, a.fp, a, resolved)
		if why == "" {
			loud = append(loud, a)
			continue
		}
		if _, ok := byReason[why]; !ok {
			reasons = append(reasons, why)
		}
		byReason[why] = append(byReason[why], a)
	}
	slices.Sort(reasons)
	for _, why := range reasons {
		for _, l := range r.changeLines(kind, installation, byReason[why], now, r.Collapse) {
			quiet = append(quiet, l+" (quiet: "+why+")")
		}
	}
	return loud, quiet
}

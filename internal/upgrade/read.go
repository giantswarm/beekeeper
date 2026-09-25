package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Target is an installation to read and the kube context that reaches it.
type Target struct {
	Name    string
	Context string
}

// Status is one installation's reading: its running upgrades, or why it
// could not be read. An unreadable installation is neither upgrading nor
// quiet.
type Status struct {
	Installation string    `json:"installation"`
	Err          string    `json:"error,omitempty"`
	Upgrades     []Upgrade `json:"upgrades,omitempty"`
	// NoClusterAPI is an installation that serves none of the kinds: it has
	// no cluster to upgrade.
	NoClusterAPI bool `json:"noClusterAPI,omitempty"`
}

// Words is the status in one line: the installation and none, unreadable,
// or its upgrades; with a now, also how long each has run and its progress.
func (s Status) Words(now time.Time) string {
	switch {
	case s.Err != "":
		return s.Installation + " unreadable: " + s.Err
	case s.NoClusterAPI:
		return s.Installation + " none (no Cluster API)"
	case len(s.Upgrades) == 0:
		return s.Installation + " none"
	}
	ws := make([]string, 0, len(s.Upgrades))
	for _, u := range s.Upgrades {
		w := Describe(s.Installation, u)
		if u.Rolling {
			w = s.Installation + "/" + u.Cluster + " rolling out"
		}
		if !now.IsZero() {
			if !u.Since.IsZero() {
				w += " for " + minutes(now.Sub(u.Since))
			}
			w += fmt.Sprintf(" (control plane %s, node pools %s)", u.ControlPlane, u.NodePools)
		}
		ws = append(ws, w)
	}
	return strings.Join(ws, "; ")
}

// minutes is a duration in whole minutes: 9m, 1h05m.
func minutes(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// Reader lists an installation's clusters, control planes and node pools
// with kubectl.
type Reader struct {
	// Kubectl is the kubectl binary.
	Kubectl string
	// Timeout bounds the reading of one installation.
	Timeout time.Duration
}

// notServed is kubectl's error for a kind the API server does not serve.
const notServed = "the server doesn't have a resource type"

// errNotServed is a kind the installation does not serve.
var errNotServed = errors.New("not served")

// Read reads every target in parallel, each within r.Timeout: one list per
// kind, and for an upgrade whose hold does not exist yet and whose from
// release is unknown, its cluster's events. held says which clusters hold
// an upgrade.
func (r Reader) Read(ctx context.Context, targets []Target, now time.Time, held func(installation, cluster string) bool) []Status {
	out := make([]Status, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() {
			out[i] = r.readOne(ctx, t, now, func(cluster string) bool { return held(t.Name, cluster) })
		})
	}
	wg.Wait()
	return out
}

func (r Reader) readOne(ctx context.Context, t Target, now time.Time, held func(string) bool) Status {
	s := Status{Installation: t.Name}
	if t.Context == "" {
		s.Err = "no kube context reaches it"
		return s
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	lists := make([][]Object, len(Kinds))
	errs := make([]error, len(Kinds))
	var wg sync.WaitGroup
	for i, kind := range Kinds {
		wg.Go(func() {
			raw, err := r.kubectl(ctx, t.Context, "get", kind, "-A", "-o", "json")
			if err == nil {
				lists[i], err = Parse(raw)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	var objs []Object
	served := 0
	for i, err := range errs {
		switch {
		case errors.Is(err, errNotServed):
		case err != nil:
			s.Err = err.Error()
			return s
		default:
			served++
			objs = append(objs, lists[i]...)
		}
	}
	s.NoClusterAPI = served == 0
	s.Upgrades = Detect(objs, now, held)
	for i, u := range s.Upgrades {
		if u.From == "" && !u.Rolling && !held(u.Cluster) {
			s.Upgrades[i].From = r.from(ctx, t.Context, u)
		}
	}
	return s
}

// upgradingEvent is cluster-api-events' message on an upgrade's start.
var upgradingEvent = regexp.MustCompile(`^from release (\S+) to (\S+)$`)

// from is the release the upgrade started from, as the cluster's latest
// Upgrading* event to its release says; empty when none does.
func (r Reader) from(ctx context.Context, kubeContext string, u Upgrade) string {
	raw, err := r.kubectl(ctx, kubeContext, "get", "events", "-n", u.Namespace, "-o", "json",
		"--field-selector", "involvedObject.kind=Cluster,involvedObject.name="+u.Cluster)
	if err != nil {
		return ""
	}
	var l struct {
		Items []struct {
			Reason        string    `json:"reason"`
			Message       string    `json:"message"`
			LastTimestamp time.Time `json:"lastTimestamp"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &l) != nil {
		return ""
	}
	var from string
	var at time.Time
	for _, e := range l.Items {
		m := upgradingEvent.FindStringSubmatch(e.Message)
		if strings.HasPrefix(e.Reason, "Upgrading") && m != nil && m[2] == u.To && !e.LastTimestamp.Before(at) {
			from, at = m[1], e.LastTimestamp
		}
	}
	return from
}

// kubectl runs one bounded, read-only kubectl call on the context.
func (r Reader) kubectl(ctx context.Context, kubeContext string, args ...string) ([]byte, error) {
	args = append([]string{"--context", kubeContext, "--request-timeout", r.Timeout.String()}, args...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.Kubectl, args...) //nolint:gosec // kubectl from the configuration
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	msg := lastLine(stderr.String())
	switch {
	case strings.Contains(msg, notServed):
		return nil, errNotServed
	case ctx.Err() != nil:
		return nil, fmt.Errorf("no answer within %s", r.Timeout)
	case msg == "":
		return nil, err
	}
	return nil, errors.New(msg)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

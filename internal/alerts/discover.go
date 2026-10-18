package alerts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// discoveryReads is how many installations a discovery reads at once.
const discoveryReads = 8

// Discovery is the installations found carrying a HelmRelease of the team,
// kept in the state directory between discoveries.
type Discovery struct {
	At            time.Time        `json:"at"`
	Installations map[string]Found `json:"installations"`
}

// Found is an installation that carries the team's HelmReleases: the kube
// context it was read in and the releases' names.
type Found struct {
	Context  string   `json:"context"`
	Releases []string `json:"releases"`
}

// Targets are the installations found that configured does not name,
// sorted, each with the team's releases it carries as its reason.
func (d *Discovery) Targets(team string, configured []string) []Target {
	var out []Target
	for _, name := range slices.Sorted(maps.Keys(d.Installations)) {
		if slices.Contains(configured, name) {
			continue
		}
		rel := d.Installations[name].Releases
		why := strings.Join(rel[:min(3, len(rel))], ", ")
		if len(rel) > 3 {
			why += fmt.Sprintf(" +%d", len(rel)-3)
		}
		out = append(out, Target{Name: name, Why: fmt.Sprintf("carries %s's %s", team, why)})
	}
	return out
}

// discoveryFile is the discovery's file in the state directory.
func discoveryFile(dir string) string { return filepath.Join(dir, "alerts-discovery.json") }

// LoadDiscovery reads the last discovery from the state directory; none is
// an empty one.
func LoadDiscovery(dir string) (*Discovery, error) {
	d := &Discovery{}
	raw, err := os.ReadFile(discoveryFile(dir))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, d); err != nil {
			return nil, fmt.Errorf("%s: %w", discoveryFile(dir), err)
		}
	}
	if d.Installations == nil {
		d.Installations = map[string]Found{}
	}
	return d, nil
}

// SaveDiscovery writes the discovery with one rename.
func SaveDiscovery(dir string, d *Discovery) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".alerts-discovery-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
	if err := cmp.Or(werr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), discoveryFile(dir))
}

// Discoverer finds the installations that carry a HelmRelease of the team:
// every cluster Teleport lists, but the excluded ones, read for
// HelmReleases labelled Label=Team. An installation without a kube context
// is logged into Login (tsh kube login with KUBECONFIG=Login), so the reads
// need no context in the person's kubeconfig.
type Discoverer struct {
	// Tsh is the tsh binary, TeleportHome the profile it reads.
	Tsh, TeleportHome string
	// Kubectl is the kubectl binary, Kubeconfig the files it reads (as
	// KUBECONFIG; empty, the environment's).
	Kubectl    string
	Kubeconfig []string
	// Login is the kubeconfig file a missing context is logged into;
	// empty, none is.
	Login string
	// Template maps an installation to its context (ResolveContext's).
	Template func(string) string
	// Exclude are the glob patterns of Teleport cluster names not read.
	Exclude []string
	// Label is the HelmRelease label that names the team.
	Label, Team string
	// Timeout bounds the reading of one installation, its login included.
	Timeout time.Duration
}

// Discover reads every installation once. One that does not answer keeps
// what prev found of it and is named in unread with why; an error is a
// failed listing of Teleport's clusters, nothing discovered.
func (d Discoverer) Discover(ctx context.Context, prev *Discovery, now time.Time) (*Discovery, []string, error) {
	names, err := d.clusters(ctx)
	if err != nil {
		return nil, nil, err
	}
	contexts := kubeContexts(ctx, d.Kubectl, d.Kubeconfig)
	if d.Login != "" {
		// One login at a time: each writes the same file.
		logged := false
		for _, name := range names {
			if ResolveContext(name, "", d.Template, contexts) == "" {
				logged = d.login(ctx, name) == nil || logged
			}
		}
		if logged {
			contexts = kubeContexts(ctx, d.Kubectl, d.Kubeconfig)
		}
	}
	out := &Discovery{At: now, Installations: map[string]Found{}}
	why := make([]string, len(names))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, discoveryReads)
	for i, name := range names {
		kctx := ResolveContext(name, "", d.Template, contexts)
		if kctx == "" {
			why[i] = "no kube context"
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			rel, err := d.releases(ctx, kctx)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				why[i] = err.Error()
			case len(rel) > 0:
				out.Installations[name] = Found{Context: kctx, Releases: rel}
			}
		})
	}
	wg.Wait()
	var unread []string
	for i, name := range names {
		if why[i] == "" {
			continue
		}
		unread = append(unread, name+": "+why[i])
		if f, ok := prev.found(name); ok {
			out.Installations[name] = f
		}
	}
	return out, unread, nil
}

func (d *Discovery) found(name string) (Found, bool) {
	if d == nil {
		return Found{}, false
	}
	f, ok := d.Installations[name]
	return f, ok
}

// clusters are the names of Teleport's kube clusters but the excluded ones,
// sorted.
func (d Discoverer) clusters(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(d.Timeout, time.Minute))
	defer cancel()
	out, err := d.tsh(ctx, "", "kube", "ls", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("tsh kube ls: %s", cmdError(err))
	}
	var listed []struct {
		Name string `json:"kube_cluster_name"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("tsh kube ls: %w", err)
	}
	var names []string
	for _, c := range listed {
		if c.Name != "" && !d.excluded(c.Name) && !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (d Discoverer) excluded(name string) bool {
	return slices.ContainsFunc(d.Exclude, func(p string) bool {
		ok, _ := path.Match(p, name)
		return ok
	})
}

// login logs the installation into the login file.
func (d Discoverer) login(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(d.Timeout, time.Minute))
	defer cancel()
	return d.tsh(ctx, d.Login, "kube", "login", name).Run()
}

// tsh is a tsh command on the profile, with KUBECONFIG=kubeconfig unless
// it is empty.
func (d Discoverer) tsh(ctx context.Context, kubeconfig string, args ...string) *exec.Cmd {
	cmd := proc.Command(ctx, d.Tsh, args...) //nolint:gosec // tsh from the configuration
	cmd.Env = os.Environ()
	if d.TeleportHome != "" {
		cmd.Env = append(cmd.Env, "TELEPORT_HOME="+d.TeleportHome)
	}
	if kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
	}
	return cmd
}

// releases are the names of the HelmReleases labelled with the team in the
// context, sorted.
func (d Discoverer) releases(ctx context.Context, kubeContext string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(d.Timeout, time.Minute))
	defer cancel()
	cmd := proc.Command(ctx, d.Kubectl, "--context", kubeContext, "get", "helmreleases.helm.toolkit.fluxcd.io", "-A", //nolint:gosec // kubectl from the configuration
		"-l", d.Label+"="+d.Team, "-o", "jsonpath={.items[*].metadata.name}")
	proc.Kubeconfig(cmd, d.Kubeconfig)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("timed out")
		}
		return nil, errors.New(cmdError(err))
	}
	rel := strings.Fields(string(out))
	slices.Sort(rel)
	return slices.Compact(rel), nil
}

// cmdError is the last line a failed command wrote to stderr, else the
// error.
func cmdError(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n"); lines[len(lines)-1] != "" {
			return lines[len(lines)-1]
		}
	}
	return err.Error()
}

// kubeContexts are the context names of the kubeconfig files (the
// environment's when empty); nil when kubectl fails.
func kubeContexts(ctx context.Context, kubectl string, files []string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := proc.Command(ctx, kubectl, "config", "get-contexts", "-o", "name") //nolint:gosec // kubectl from the configuration
	proc.Kubeconfig(cmd, files)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// DiscoveryLines say the installations beyond configured that next found
// and prev had not, and those prev had and next no longer found.
func DiscoveryLines(prev, next *Discovery, team string, configured []string) []string {
	var lines []string
	for _, t := range next.Targets(team, configured) {
		if _, ok := prev.found(t.Name); !ok {
			lines = append(lines, "ALERTS DISCOVERED "+t.Name+" "+t.Why)
		}
	}
	for _, t := range prev.Targets(team, configured) {
		if _, ok := next.found(t.Name); !ok {
			lines = append(lines, fmt.Sprintf("ALERTS UNDISCOVERED %s carries no HelmRelease of %s any more", t.Name, team))
		}
	}
	return lines
}

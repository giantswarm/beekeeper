package alerts

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// query keeps the active alerts only: nothing silenced or inhibited.
const query = "active=true&silenced=false&inhibited=false"

// teleportPrefix is the context `tsh kube login` writes for an installation.
const teleportPrefix = "teleport.giantswarm.io-"

const (
	// forwardWithin bounds the wait for a port-forward's local port.
	forwardWithin = 15 * time.Second
	// requestTimeout bounds one Alertmanager request.
	requestTimeout = 15 * time.Second
	// stopGrace is how long a port-forward has after SIGTERM before it is killed.
	stopGrace = 3 * time.Second
)

// endpoint is an Alertmanager service, in the order they are tried: Mimir's
// with the tenant header (the tenant "anonymous" holds nothing, and the API
// server's service proxy cannot send the header), else a plain one.
type endpoint struct {
	namespace, service string
	port               int
	path               string
	header             map[string]string
}

var endpoints = []endpoint{
	{"mimir", "mimir-alertmanager", 8080, "/alertmanager/api/v2/alerts", map[string]string{"X-Scope-OrgID": "giantswarm"}},
	{"monitoring", "kube-prometheus-stack-alertmanager", 9093, "/api/v2/alerts", nil},
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// Target is an installation to read, the kube context that reaches it and
// why it is read.
type Target struct {
	Name    string `json:"name"`
	Context string `json:"context"`
	Why     string `json:"why"`
}

// Targets are the configured installations, then every leased one whose name
// resolves to a kube context (a kind lab's or the browser's does not), each
// with its context resolved.
func Targets(configured []Target, leased map[string]string, contexts []string) []Target {
	var out []Target
	seen := map[string]int{}
	for _, t := range configured {
		if _, ok := seen[t.Name]; ok {
			continue
		}
		t.Context, t.Why = ResolveContext(t.Name, t.Context, contexts), "configured"
		seen[t.Name] = len(out)
		out = append(out, t)
	}
	names := make([]string, 0, len(leased))
	for name := range leased {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		why := "leased by " + leased[name]
		if i, ok := seen[name]; ok {
			out[i].Why += ", " + why
			continue
		}
		if ctx := ResolveContext(name, "", contexts); ctx != "" {
			out = append(out, Target{Name: name, Context: ctx, Why: why})
		}
	}
	return out
}

// ResolveContext is the explicit context, else teleport.giantswarm.io-<name>,
// else the context named <name>, else the one ending in @<name>.
func ResolveContext(name, explicit string, contexts []string) string {
	if explicit != "" {
		return explicit
	}
	for _, c := range []string{teleportPrefix + name, name} {
		if slices.Contains(contexts, c) {
			return c
		}
	}
	for _, c := range contexts {
		if strings.HasSuffix(c, "@"+name) {
			return c
		}
	}
	return ""
}

// Reader reads Alertmanagers through kubectl port-forwards.
type Reader struct {
	// Kubectl is the kubectl binary.
	Kubectl string
	// Timeout bounds the reading of one installation, forwards included.
	Timeout time.Duration
}

// Contexts are the kubeconfig's context names.
func (r Reader) Contexts(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.Kubectl, "config", "get-contexts", "-o", "name").Output() //nolint:gosec // kubectl from the configuration
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// Read reads every target in parallel, each within r.Timeout, and returns
// the answers in the targets' order. Every port-forward has ended when it
// returns.
func (r Reader) Read(ctx context.Context, targets []Target) []Answer {
	out := make([]Answer, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() { out[i] = r.fetch(ctx, t) })
	}
	wg.Wait()
	return out
}

func (r Reader) fetch(ctx context.Context, t Target) Answer {
	if t.Context == "" {
		return Answer{Why: "no kube context for " + t.Name}
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	why := ""
	for _, ep := range endpoints {
		f, port, err := r.forward(ctx, t.Context, ep)
		if f == nil {
			why = err
			if strings.Contains(strings.ToLower(why), "not found") {
				continue
			}
			return Answer{Why: why}
		}
		ans := get(ctx, port, ep)
		f.close()
		return ans
	}
	return Answer{Why: cmp.Or(why, "no Alertmanager")}
}

func get(ctx context.Context, port int, ep endpoint) Answer {
	fail := func(format string, args ...any) Answer {
		return Answer{Why: fmt.Sprintf("%s/%s: ", ep.namespace, ep.service) + fmt.Sprintf(format, args...)}
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s?%s", port, ep.path, query), nil)
	if err != nil {
		return fail("%v", err)
	}
	for k, v := range ep.header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fail("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var raw []Raw
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return fail("%v", err)
	}
	return Answer{OK: true, Alerts: raw}
}

// portForward is a running `kubectl port-forward`, in its own process group
// so that its end takes everything it started.
type portForward struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    *os.File
}

// forward starts a port-forward to the endpoint and waits for its local port;
// on failure it returns nil and the last line kubectl printed.
func (r Reader) forward(ctx context.Context, kubeContext string, ep endpoint) (*portForward, int, string) {
	fctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(fctx, r.Kubectl, "--context", kubeContext, "-n", ep.namespace, //nolint:gosec // kubectl from the configuration
		"port-forward", "svc/"+ep.service, fmt.Sprintf(":%d", ep.port))
	// Pdeathsig ends the forward even when beekeeper itself is killed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = stopGrace
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, 0, err.Error()
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	_ = pw.Close()
	if err != nil {
		cancel()
		_ = pr.Close()
		return nil, 0, err.Error()
	}
	f := &portForward{cmd: cmd, cancel: cancel, out: pr}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-fctx.Done():
			}
		}
	}()
	timer := time.NewTimer(forwardWithin)
	defer timer.Stop()
	last := ""
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				f.close()
				return nil, 0, cmp.Or(last, fmt.Sprintf("no port-forward within %s", forwardWithin))
			}
			if m := forwarding.FindStringSubmatch(l); m != nil {
				port, _ := strconv.Atoi(m[1])
				return f, port, ""
			}
			last = strings.TrimSpace(l)
		case <-timer.C:
			f.close()
			return nil, 0, cmp.Or(last, fmt.Sprintf("no port-forward within %s", forwardWithin))
		case <-ctx.Done():
			f.close()
			return nil, 0, "timed out"
		}
	}
}

// close ends the forward's process group and waits for it.
func (f *portForward) close() {
	f.cancel()
	_ = f.cmd.Wait()
	_ = f.out.Close()
}

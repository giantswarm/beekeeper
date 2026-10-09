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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// query keeps the active, unsilenced alerts, the inhibited ones included:
// Normalize keeps the team's alerts inhibited by working hours only.
const query = "active=true&silenced=false&inhibited=true"

const (
	// forwardWithin bounds the wait for a port-forward's local port.
	forwardWithin = 15 * time.Second
	// requestTimeout bounds one Alertmanager request.
	requestTimeout = 15 * time.Second
	// stopGrace is how long a port-forward has after SIGTERM before it is killed.
	stopGrace = 3 * time.Second
)

// retryPause is the pause before another attempt at a failed reading.
var retryPause = 2 * time.Second

// endpoint is an Alertmanager service, in the order they are tried: Mimir's
// with the tenant header when a tenant is configured (the tenant
// "anonymous" holds nothing, and the API server's service proxy cannot send
// the header), else a plain one.
type endpoint struct {
	namespace, service string
	port               int
	path               string
	header             map[string]string
}

var plain = endpoint{"monitoring", "kube-prometheus-stack-alertmanager", 9093, "/api/v2/alerts", nil}

// endpoints are the Alertmanagers the reader tries, in order.
func (r *Reader) endpoints() []endpoint {
	if r.Tenant == "" {
		return []endpoint{plain}
	}
	mimir := endpoint{"mimir", "mimir-alertmanager", 8080, "/alertmanager/api/v2/alerts", map[string]string{"X-Scope-OrgID": r.Tenant}}
	return []endpoint{mimir, plain}
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
// with its context resolved (template: ResolveContext's).
func Targets(configured []Target, leased map[string]string, template func(string) string, contexts []string) []Target {
	var out []Target
	seen := map[string]int{}
	for _, t := range configured {
		if _, ok := seen[t.Name]; ok {
			continue
		}
		t.Context, t.Why = ResolveContext(t.Name, t.Context, template, contexts), "configured"
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
		if ctx := ResolveContext(name, "", template, contexts); ctx != "" {
			out = append(out, Target{Name: name, Context: ctx, Why: why})
		}
	}
	return out
}

// ResolveContext is the explicit context, else the templated one (template
// maps a name to its context, "" for none), else the context named <name>,
// else the one ending in @<name>.
func ResolveContext(name, explicit string, template func(string) string, contexts []string) string {
	if explicit != "" {
		return explicit
	}
	for _, c := range []string{template(name), name} {
		if c != "" && slices.Contains(contexts, c) {
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

// Reader reads Alertmanagers through kubectl port-forwards. A Reader that
// keeps its forwards holds each installation's open across reads, until
// Close: a read is then an HTTP request, no kubectl or Teleport credential
// plugin run (kubectl caches the credential until it expires).
type Reader struct {
	// Kubectl is the kubectl binary.
	Kubectl string
	// Timeout bounds the reading of one installation, forwards included.
	Timeout time.Duration
	// Tenant is the Mimir tenant read first; empty, only the plain
	// Alertmanager is read.
	Tenant string
	// Keep holds each installation's port-forward open for the next read,
	// and the kubeconfig's contexts until it changes; Close ends them.
	Keep bool

	mu sync.Mutex
	// held is the open port-forward of each kube context, kept.
	held map[string]*portForward
	// contexts are the kubeconfig's contexts, read when its files were
	// stamped so, kept.
	contexts []string
	stamp    string
}

// Contexts are the kubeconfig's context names; a Reader that keeps its
// forwards asks kubectl again only once the kubeconfig changed.
func (r *Reader) Contexts(ctx context.Context) []string {
	stamp := kubeconfigStamp()
	if r.Keep {
		r.mu.Lock()
		if r.contexts != nil && r.stamp == stamp {
			defer r.mu.Unlock()
			return r.contexts
		}
		r.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := proc.Command(ctx, r.Kubectl, "config", "get-contexts", "-o", "name").Output() //nolint:gosec // kubectl from the configuration
	if err != nil {
		return nil
	}
	contexts := strings.Fields(string(out))
	if r.Keep {
		r.mu.Lock()
		r.contexts, r.stamp = contexts, stamp
		r.mu.Unlock()
	}
	return contexts
}

// kubeconfigStamp is the path, modification time and size of each
// kubeconfig file kubectl reads: $KUBECONFIG's, else ~/.kube/config.
func kubeconfigStamp() string {
	paths := filepath.SplitList(os.Getenv("KUBECONFIG"))
	if len(paths) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			paths = []string{filepath.Join(home, ".kube", "config")}
		}
	}
	var b strings.Builder
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil { //nolint:gosec // the kubeconfig kubectl reads
			fmt.Fprintf(&b, "%s %d %d\n", p, fi.ModTime().UnixNano(), fi.Size())
		} else {
			fmt.Fprintf(&b, "%s -\n", p)
		}
	}
	return b.String()
}

// Read reads every target in parallel, each within r.Timeout, and returns
// the answers in the targets' order. Unless the Reader keeps its forwards,
// every port-forward has ended when it returns.
func (r *Reader) Read(ctx context.Context, targets []Target) []Answer {
	out := make([]Answer, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() { out[i] = r.fetch(ctx, t) })
	}
	wg.Wait()
	return out
}

// Close ends every port-forward the Reader keeps.
func (r *Reader) Close() {
	r.mu.Lock()
	held := r.held
	r.held = nil
	r.mu.Unlock()
	for _, f := range held {
		f.close()
	}
}

// fetch reads one installation, attempt after attempt until one answers or
// r.Timeout has passed: a port-forward through a proxy fails now and then
// and answers at the next attempt, and a kept one that dropped is replaced
// at the next. A failure names the last attempt's reason and the number of
// attempts.
func (r *Reader) fetch(ctx context.Context, t Target) Answer {
	if t.Context == "" {
		return Answer{Why: "no kube context for " + t.Name}
	}
	if r.unknown(t.Context) {
		return Answer{Why: fmt.Sprintf("context %q is not in the kubeconfig", t.Context)}
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var last Answer
	for n := 1; ; n++ {
		ans := r.attempt(ctx, t.Context)
		if ans.OK {
			return ans
		}
		if ctx.Err() == nil || last.Why == "" {
			last = ans
		}
		if missingContext.MatchString(ans.Why) {
			return ans
		}
		pause := time.NewTimer(retryPause)
		select {
		case <-pause.C:
			if ctx.Err() == nil {
				continue
			}
		case <-ctx.Done():
			pause.Stop()
		}
		if n > 1 {
			last.Why += fmt.Sprintf(" (%d attempts)", n)
		}
		return last
	}
}

// attempt reads through the context's kept forward while it runs, else
// through a new one to the first Alertmanager the context has. A kept
// forward that fails is ended, the attempt with it.
func (r *Reader) attempt(ctx context.Context, kubeContext string) Answer {
	if f := r.take(kubeContext); f != nil {
		ans := get(ctx, f.port, f.ep)
		r.give(kubeContext, f, ans.OK)
		return ans
	}
	why := ""
	for _, ep := range r.endpoints() {
		f, err := r.forward(ctx, kubeContext, ep)
		if f == nil {
			why = err
			if strings.Contains(strings.ToLower(why), "not found") {
				continue
			}
			return Answer{Why: why}
		}
		ans := get(ctx, f.port, ep)
		r.give(kubeContext, f, ans.OK)
		return ans
	}
	return Answer{Why: cmp.Or(why, "no Alertmanager")}
}

// missingContext is kubectl's answer for a context the kubeconfig lacks:
// no later attempt finds it.
var missingContext = regexp.MustCompile(`context ".*" does not exist`)

// unknown reports whether the kubeconfig's kept contexts lack kubeContext.
func (r *Reader) unknown(kubeContext string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contexts != nil && !slices.Contains(r.contexts, kubeContext)
}

// take is the context's kept forward while it runs; one that ended is
// closed. The forward is the caller's until give.
func (r *Reader) take(kubeContext string) *portForward {
	r.mu.Lock()
	f := r.held[kubeContext]
	delete(r.held, kubeContext)
	r.mu.Unlock()
	if f != nil && !f.running() {
		f.close()
		return nil
	}
	return f
}

// give keeps a forward that answered, when the Reader keeps its forwards,
// and closes it otherwise.
func (r *Reader) give(kubeContext string, f *portForward, answered bool) {
	if r.Keep && answered && f.running() {
		r.mu.Lock()
		old := r.held[kubeContext]
		if r.held == nil {
			r.held = map[string]*portForward{}
		}
		r.held[kubeContext] = f
		r.mu.Unlock()
		if old != nil {
			old.close()
		}
		return
	}
	f.close()
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

// portForward is a running `kubectl port-forward` to an endpoint on a local
// port, in its own process group so that its end takes everything it
// started.
type portForward struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    *os.File
	port   int
	ep     endpoint
	// exited is closed once the process has ended.
	exited chan struct{}
}

// forward starts a port-forward to the endpoint and waits for its local port;
// on failure it returns nil and the last line kubectl printed. A Reader that
// keeps its forwards starts it beyond ctx, which bounds only the wait.
func (r *Reader) forward(ctx context.Context, kubeContext string, ep endpoint) (*portForward, string) {
	life := ctx
	if r.Keep {
		life = context.WithoutCancel(ctx)
	}
	fctx, cancel := context.WithCancel(life)
	cmd := proc.Command(fctx, r.Kubectl, "--context", kubeContext, "-n", ep.namespace, //nolint:gosec // kubectl from the configuration
		"port-forward", "svc/"+ep.service, fmt.Sprintf(":%d", ep.port))
	ownGroup(cmd)
	cmd.WaitDelay = stopGrace
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err.Error()
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	_ = pw.Close()
	if err != nil {
		cancel()
		_ = pr.Close()
		return nil, err.Error()
	}
	f := &portForward{cmd: cmd, cancel: cancel, out: pr, ep: ep, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(f.exited)
	}()
	// The lines kubectl prints until its port, then drained: a kept
	// forward says every connection it handles, and a full pipe would
	// stall it.
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
				return nil, cmp.Or(last, fmt.Sprintf("no port-forward within %s", forwardWithin))
			}
			if m := forwarding.FindStringSubmatch(l); m != nil {
				f.port, _ = strconv.Atoi(m[1])
				go func() {
					for range lines {
					}
				}()
				return f, ""
			}
			last = strings.TrimSpace(l)
		case <-timer.C:
			f.close()
			return nil, cmp.Or(last, fmt.Sprintf("no port-forward within %s", forwardWithin))
		case <-ctx.Done():
			f.close()
			return nil, "timed out"
		}
	}
}

// running reports whether the forward's process still runs.
func (f *portForward) running() bool {
	select {
	case <-f.exited:
		return false
	default:
		return true
	}
}

// close ends the forward's process group and waits for it.
func (f *portForward) close() {
	f.cancel()
	<-f.exited
	_ = f.out.Close()
}

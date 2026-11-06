package alerts

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// query keeps the active, unsilenced alerts, the inhibited ones included:
// Normalize keeps the team's alerts inhibited by working hours only.
const query = "active=true&silenced=false&inhibited=true"

const (
	// forwardWithin bounds the wait for a port-forward's local port.
	forwardWithin = 15 * time.Second
	// requestTimeout bounds one Alertmanager request.
	requestTimeout = 15 * time.Second
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

// Target is an installation to read, the kube context that reaches it and
// why it is read.
type Target struct {
	Name    string `json:"name"`
	Context string `json:"context"`
	Why     string `json:"why"`
}

// WhyConfigured is the reason a configured installation is read.
const WhyConfigured = "configured"

// Targets are the configured installations (a reason given kept, else
// WhyConfigured), then every leased one whose name
// resolves to a kube context (a kind lab's or the browser's does not), each
// with its context resolved (template: ResolveContext's).
func Targets(configured []Target, leased map[string]string, template func(string) string, contexts []string) []Target {
	var out []Target
	seen := map[string]int{}
	for _, t := range configured {
		if _, ok := seen[t.Name]; ok {
			continue
		}
		t.Context, t.Why = ResolveContext(t.Name, t.Context, template, contexts), cmp.Or(t.Why, WhyConfigured)
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
// else the one ending in @<name>. An explicit context that contexts lacks
// is none; nil contexts (unknown) keep it.
func ResolveContext(name, explicit string, template func(string) string, contexts []string) string {
	if explicit != "" {
		if contexts != nil && !slices.Contains(contexts, explicit) {
			return ""
		}
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

// Reader reads Alertmanagers through in-process client-go port-forwards.
type Reader struct {
	// Kubectl is retained for configuration compatibility; reads do not run it.
	Kubectl string
	// Kubeconfig are the kubeconfig files kubectl reads, as KUBECONFIG;
	// empty, the environment's.
	Kubeconfig []string
	// Timeout bounds the reading of one installation, forwards included.
	Timeout time.Duration
	// Tenant is the Mimir tenant read first; empty, only the plain
	// Alertmanager is read.
	Tenant string
	// Keep holds each installation's port-forward open for the next read,
	// and the kubeconfig's contexts until it changes; Close ends them.
	Keep bool

	// startForward allows tests to supply in-process forwards.
	startForward func(context.Context, string, endpoint) (*portForward, string)
	mu           sync.Mutex
	// held is the open port-forward of each kube context, kept.
	held map[string]*portForward
	// contexts are the kubeconfig's contexts, read when its files were
	// stamped so, kept.
	contexts []string
	stamp    string
}

// Contexts are the kubeconfig's context names; a Reader that keeps its
// forwards reloads them only once the kubeconfig changes.
func (r *Reader) Contexts(_ context.Context) []string {
	stamp := kubeconfigStamp(r.Kubeconfig)
	if r.Keep {
		r.mu.Lock()
		if r.contexts != nil && r.stamp == stamp {
			defer r.mu.Unlock()
			return r.contexts
		}
		r.mu.Unlock()
	}
	cfg, err := r.loadingRules().Load()
	if err != nil {
		return nil
	}
	contexts := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		contexts = append(contexts, name)
	}
	slices.Sort(contexts)
	if r.Keep {
		r.mu.Lock()
		r.contexts, r.stamp = contexts, stamp
		r.mu.Unlock()
	}
	return contexts
}

// kubeconfigStamp is the path, modification time and size of each
// kubeconfig file kubectl reads: files, else $KUBECONFIG's, else
// ~/.kube/config.
func kubeconfigStamp(files []string) string {
	paths := files
	if len(paths) == 0 {
		paths = filepath.SplitList(os.Getenv("KUBECONFIG"))
	}
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
	r.retire(targets)
	out := make([]Answer, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() { out[i] = r.fetch(ctx, t) })
	}
	wg.Wait()
	return out
}

// retire ends cached forwards no target uses any more, as leases end and
// discoveries change. Active contexts keep their forwards across readings.
func (r *Reader) retire(targets []Target) {
	active := make(map[string]bool, len(targets))
	for _, t := range targets {
		active[t.Context] = true
	}
	var retired []*portForward
	r.mu.Lock()
	for kubeContext, f := range r.held {
		if !active[kubeContext] {
			delete(r.held, kubeContext)
			retired = append(retired, f)
		}
	}
	r.mu.Unlock()
	for _, f := range retired {
		f.close()
	}
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
		return Answer{Why: "no kube context for " + t.Name, NoContext: true}
	}
	if r.unknown(t.Context) {
		return Answer{Why: fmt.Sprintf("context %q is not in the kubeconfig", t.Context), NoContext: true}
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
			ans.NoContext = true
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

// portForward owns a local listener and SPDY connection, with no child process.
type portForward struct {
	cancel context.CancelFunc
	port   int
	ep     endpoint
	exited chan struct{}
}

func (f *portForward) running() bool {
	select {
	case <-f.exited:
		return false
	default:
		return true
	}
}

func (f *portForward) close() {
	f.cancel()
	<-f.exited
}

func (r *Reader) loadingRules() *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(r.Kubeconfig) > 0 {
		rules.Precedence = r.Kubeconfig
	}
	return rules
}

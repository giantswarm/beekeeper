package cmd

import (
	"context"

	"errors"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/upgrade"

	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func fleetFixture(t *testing.T, count int) (*fleetWatch, []v1alpha1.Environment) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	var objects []ctrlclient.Object
	var envs []v1alpha1.Environment
	for i := range count {
		name := string(rune('a' + i))
		env := v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.EnvironmentSpec{Installation: name, MusterCluster: "cluster-" + name, Team: "fleet"}}
		envs = append(envs, env)
		objects = append(objects, env.DeepCopy())
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&v1alpha1.Environment{}, &v1alpha1.Hold{}, &v1alpha1.MergeLane{}).Build()
	s := &server{store: kube.New(c), cfg: &config.Config{}, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.cfg.Serve.FleetWatch = config.FleetWatch{Kagent: "instances", Kubernetes: "list", Alertmanager: "alerts", Concurrency: 2}
	return &fleetWatch{s: s, baseline: map[string]map[string]string{}, agents: map[string]map[string]fleetAgent{}, changed: map[string]time.Time{}, alertState: map[string]*alerts.Installation{}}, envs
}

func fleetLines(t *testing.T, w *fleetWatch) string {
	t.Helper()
	evs, err := w.s.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range evs {
		lines = append(lines, e.Detail)
	}
	return strings.Join(lines, "\n")
}

func TestFleetWatchBoundedSources(t *testing.T) {
	w, _ := fleetFixture(t, 5)
	var active, peak, calls atomic.Int32
	w.call = func(ctx context.Context, tool string, args map[string]any) (string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		calls.Add(1)
		if !strings.HasPrefix(args["cluster"].(string), "cluster-") {
			t.Error("missing installation routing")
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		switch tool {
		case "instances":
			return `{"instances":[]}`, nil
		case "alerts":
			return `{"alerts":[]}`, nil
		default:
			return `{"items":[]}`, nil
		}
	}
	if err := w.round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 || peak.Load() < 2 {
		t.Fatalf("peak %d", peak.Load())
	}
	if calls.Load() != 45 {
		t.Fatalf("calls %d, want nine sources per installation", calls.Load())
	}
}

func TestFleetWatchAgentLifecycle(t *testing.T) {
	w, envs := fleetFixture(t, 2)
	env := envs[1]
	now := time.Now()
	w.s.now = func() time.Time { return now }
	running := `{"instances":[{"id":"agent-2","state":"RUNNING","current_task":{"id":"task"}}]}`
	if err := w.applyAgents(env, "b/agents", running); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if err := w.applyAgents(env, "b/agents", running); err != nil {
		t.Fatal(err)
	}
	failed := strings.Replace(running, "RUNNING", "FAILED", 1)
	for range 2 {
		if err := w.applyAgents(env, "b/agents", failed); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.applyAgents(env, "b/agents", `{}`); err == nil {
		t.Fatal("accepted incomplete response")
	}
	if strings.Contains(fleetLines(t, w), "AGENT GONE") {
		t.Fatal("failed read lost agent")
	}
	if err := w.applyAgents(env, "b/agents", `{"instances":[]}`); err != nil {
		t.Fatal(err)
	}
	lines := fleetLines(t, w)
	for _, want := range []string{"AGENT STUCK b agent-2", "AGENT FAILED b agent-2", "AGENT GONE b agent-2"} {
		if strings.Count(lines, want) != 1 {
			t.Fatalf("want one %s: %s", want, lines)
		}
	}
}

func TestFleetWatchAlertsAndTemplates(t *testing.T) {
	w, envs := fleetFixture(t, 1)
	env := envs[0]
	raw := `{"alerts":[{"fingerprint":"f","labels":{"alertname":"Failure","severity":"page"}}]}`
	for range 2 {
		if err := w.applyAlerts(env, "a/alerts", raw); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := w.applyAlerts(env, "a/alerts", `{"alerts":[]}`); err != nil {
			t.Fatal(err)
		}
	}
	r := fleetReading{env: env, reads: []fleetRead{{key: "templates", raw: `{"items":[{"metadata":{"name":"template"},"status":{"conditions":[{"type":"Admitted","status":"False","reason":"Denied"}]}}]}`}, {key: "agents", err: errors.New("unavailable")}}}
	if err := w.apply(r); err == nil {
		t.Fatal("missing source error")
	}
	lines := fleetLines(t, w)
	for _, want := range []string{"NEW", "RESOLVED", "PAGE", "TEMPLATE NOT ADMITTED"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %s: %s", want, lines)
		}
	}
}

func TestFleetListRejectsIncomplete(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"items":null}`, `{"items":[],"truncated":{"omitted":1}}`, `{"items":{}}`, `invalid`} {
		if _, err := fleetList(raw, "items"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestFleetWatchTimeoutAndCancellation(t *testing.T) {
	w, envs := fleetFixture(t, 1)
	w.s.cfg.Serve.FleetWatch.Timeout.Duration = time.Millisecond
	w.call = func(ctx context.Context, _ string, _ map[string]any) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	r := w.read(context.Background(), envs[0])
	if len(r.reads) != 9 {
		t.Fatal(len(r.reads))
	}
	for _, read := range r.reads {
		if !errors.Is(read.err, context.DeadlineExceeded) {
			t.Fatal(read.err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.round(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestFleetWatchUpgradeHold(t *testing.T) {
	w, envs := fleetFixture(t, 1)
	raw := `{"kind":"Cluster","metadata":{"name":"workload","labels":{"release.giantswarm.io/version":"2"},"annotations":{"giantswarm.io/last-known-cluster-upgrade-version":"1"}}}`

	// Read via the same complete-list path used for every CAPI source.
	r := fleetReading{env: envs[0], reads: []fleetRead{{key: "clusters.v1beta2.cluster.x-k8s.io", raw: `{"items":[` + raw + `]}`}}}
	for _, kind := range upgrade.Kinds[1:] {
		r.reads = append(r.reads, fleetRead{key: kind, raw: `{"items":[]}`})
	}
	if err := w.apply(r); err != nil {
		t.Fatal(err)
	}
	st, err := w.s.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Holds) != 1 || st.Holds[0].Target != "upgrade:a/workload" {
		t.Fatalf("holds: %+v", st.Holds)
	}
	if err := w.apply(r); err != nil {
		t.Fatal(err)
	}
	r.reads[0].raw = `{"items":[]}`
	if err := w.apply(r); err != nil {
		t.Fatal(err)
	}
	st, err = w.s.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Holds) != 0 {
		t.Fatal(st.Holds)
	}
}

// The central muster fixture routes two installations. Only its second
// backend fails an agent; the watch must publish that failure to the central
// Kubernetes feed, authenticating every request with its exchanged Dex token.
func TestFleetWatchThroughMuster(t *testing.T) {
	w, _ := fleetFixture(t, 2)
	a := identityApp(t)
	ex, err := identity.NewExchanger(a.cfg.Serve.Issuer, a.cfg.Serve.Identity)
	if err != nil {
		t.Fatal(err)
	}
	token, err := ex.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var authenticated atomic.Int32
	m := mcpserver.NewMCPServer("muster", "test")
	m.AddTool(mcp.NewTool("call_tool"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, ok := req.GetArguments()["arguments"].(map[string]any)
		if !ok {
			return mcp.NewToolResultError("missing arguments"), nil
		}
		switch req.GetString("name", "") {
		case "instances":
			if args["cluster"] == "cluster-b" {
				return mcp.NewToolResultText(`{"instances":[{"id":"remote-agent","state":"FAILED"}]}`), nil
			}
			return mcp.NewToolResultText(`{"instances":[]}`), nil
		case "alerts":
			return mcp.NewToolResultText(`{"alerts":[]}`), nil
		default:
			return mcp.NewToolResultText(`{"items":[]}`), nil
		}
	})
	handler := mcpserver.NewStreamableHTTPServer(m, mcpserver.WithStateLess(true))
	httpServer := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(out, "unauthorized", http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		handler.ServeHTTP(out, r)
	}))
	t.Cleanup(httpServer.Close)
	w.call = func(ctx context.Context, tool string, args map[string]any) (string, error) {
		token, err := ex.Token(ctx)
		if err != nil {
			return "", err
		}
		return callMuster(ctx, httpServer.URL+"/mcp", token, tool, args)
	}
	if err := w.round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if authenticated.Load() < 18 {
		t.Fatal("muster requests not authenticated")
	}
	if lines := fleetLines(t, w); !strings.Contains(lines, "AGENT FAILED b remote-agent") {
		t.Fatal(lines)
	}
}

func TestFleetWatchLaneSettles(t *testing.T) {
	w, envs := fleetFixture(t, 1)
	lane := v1alpha1.MergeLane{ObjectMeta: metav1.ObjectMeta{Name: "application"}, Spec: v1alpha1.MergeLaneSpec{Installation: "a", Repositories: []string{"org/application"}}, Status: v1alpha1.MergeLaneStatus{Settling: "org/application#1", Queue: []v1alpha1.LaneEntry{{Repo: "org/application", PR: 1, Phase: state.Settling, Finished: &metav1.Time{Time: time.Now().Add(-time.Hour)}, ReleaseFailed: true}}}}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&envs[0], &lane).WithStatusSubresource(&v1alpha1.Environment{}, &v1alpha1.MergeLane{}).Build()
	w.s.store = kube.New(c)
	notReady := `{"items":[{"metadata":{"namespace":"apps","name":"application"},"spec":{"chart":{"spec":{"chart":"application"}}},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`
	if err := w.applyLanes(envs[0], notReady); err != nil {
		t.Fatal(err)
	}
	st, err := w.s.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 1 {
		t.Fatal("unready lane freed")
	}
	ready := strings.Replace(notReady, `"False"`, `"True"`, 1)
	if err := w.applyLanes(envs[0], ready); err != nil {
		t.Fatal(err)
	}
	st, err = w.s.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Merges) != 0 {
		t.Fatal("ready lane still settling")
	}
	lines := fleetLines(t, w)
	if !strings.Contains(lines, "LANE STALLED") || !strings.Contains(lines, "LANE SETTLED") {
		t.Fatal(lines)
	}
}

func TestFleetWatchInterval(t *testing.T) {
	w, _ := fleetFixture(t, 1)
	interval := 25 * time.Millisecond
	w.s.cfg.Watch.Interval.Duration = interval
	rounds := make(chan time.Time, 4)
	var calls atomic.Int32
	w.call = func(_ context.Context, tool string, _ map[string]any) (string, error) {
		if calls.Add(1)%9 == 1 {
			rounds <- time.Now()
		}
		switch tool {
		case "instances":
			return `{"instances":[]}`, nil
		case "alerts":
			return `{"alerts":[]}`, nil
		default:
			return `{"items":[]}`, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.run(ctx) }()
	var first, second time.Time
	select {
	case first = <-rounds:
	case <-time.After(time.Second):
		t.Fatal("first round did not run")
	}
	select {
	case second = <-rounds:
	case <-time.After(time.Second):
		t.Fatal("second round did not run")
	}
	if second.Sub(first) < interval {
		t.Fatal("round ignored interval")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestFleetWatchGrantExpiry(t *testing.T) {
	w, envs := fleetFixture(t, 1)
	now := time.Now()
	w.s.now = func() time.Time { return now }
	if err := w.s.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Grants = append(st.Grants, state.Grant{Resource: "a", To: state.Party{Name: "agent"}, At: now.Add(-time.Hour)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.expireGrants(envs[0]); err != nil {
		t.Fatal(err)
	}
	st, err := w.s.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Grants) != 0 {
		t.Fatal(st.Grants)
	}
	if !strings.Contains(fleetLines(t, w), "GRANT EXPIRED a") {
		t.Fatal("expiry not published")
	}
}

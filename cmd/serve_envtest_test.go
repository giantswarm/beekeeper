package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/identity/identitytest"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const (
	anaAgent  = "ana-agent"
	backstage = "giantswarm/backstage"
	glean     = "glean"
	// portalLane is the test's MergeLane; teamGroup the Dex group of ourTeam.
	portalLane = "portal"
	teamGroup  = "giantswarm:team-bumblebee"
)

// serveEnv is beekeeper serve over a kube-apiserver with beekeeper's CRDs,
// behind a test Dex.
type serveEnv struct {
	url   string
	iss   *identitytest.Issuer
	store *kube.Store
	srv   *server
	rc    *rest.Config
	log   *syncBuffer
}

func newServeEnv(t *testing.T) *serveEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-envtest")
	}
	_, file, _, _ := runtime.Caller(0)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(filepath.Dir(file), "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	rc, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	store, err := kube.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(rc, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, o := range []ctrlclient.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "beekeeper-bumblebee"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "beekeeper-planeteers"}},
		&v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: graveler}, Spec: v1alpha1.EnvironmentSpec{Installation: graveler, Team: ourTeam}},
		&v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: glean}, Spec: v1alpha1.EnvironmentSpec{Installation: glean, Team: ourTeam}},
		&v1alpha1.MergeLane{ObjectMeta: metav1.ObjectMeta{Name: portalLane}, Spec: v1alpha1.MergeLaneSpec{Installation: "gazelle", Repositories: []string{backstage}}},
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	iss := identitytest.New(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Serve = config.Serve{
		Issuer: iss.URL, ClientIDs: []string{"muster"}, Organization: "giantswarm:giantswarm",
		Teams:       map[string]string{teamGroup: ourTeam, "giantswarm:team-planeteers": "planeteers"},
		Supervisors: map[string]string{ourTeam: "giantswarm:bumblebee-supervisors"},
	}
	ids, err := identity.New(ctx, cfg.Serve)
	if err != nil {
		t.Fatal(err)
	}
	mail, err := mailbox.Open(ctx, mailboxtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mail.Close)
	log := &syncBuffer{}
	ctrllog.SetLogger(logr.Discard())
	s := newServer(cfg, store, mail, ids, slog.New(slog.NewJSONHandler(log, nil)))
	wctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	if err := s.watch(wctx, rc); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return &serveEnv{url: srv.URL + "/mcp", iss: iss, store: store, srv: s, rc: rc, log: log}
}

func (e *serveEnv) token(t *testing.T, email string, groups ...string) string {
	return e.iss.Token(t, "muster", identitytest.Claims{Email: email, EmailVerified: identitytest.Verified(), Groups: append([]string{"giantswarm:giantswarm"}, groups...)})
}

// mcpCaller is one person's MCP client.
type mcpCaller struct {
	t *testing.T
	c *client.Client
}

func (e *serveEnv) as(t *testing.T, token string) *mcpCaller {
	t.Helper()
	c, err := client.NewStreamableHttpClient(e.url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "serve-test", Version: "0"}
	if _, err := c.Initialize(context.Background(), init); err != nil {
		t.Fatal(err)
	}
	return &mcpCaller{t: t, c: c}
}

// call runs a tool and returns its text, its structured content and
// whether it failed.
func (m *mcpCaller) call(name string, args map[string]any) (string, map[string]any, bool) {
	m.t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = name, args
	res, err := m.c.CallTool(context.Background(), req)
	if err != nil {
		m.t.Fatalf("%s: %v", name, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text += tc.Text
		}
	}
	var structured map[string]any
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &structured)
	}
	return text, structured, res.IsError
}

// expect calls a tool and checks its outcome, its text and that it left
// exactly one audit Event and one log line.
func (e *serveEnv) expect(t *testing.T, m *mcpCaller, name string, args map[string]any, fail bool, want string) map[string]any {
	t.Helper()
	before := e.events(t)
	lines := strings.Count(e.log.String(), "\n")
	text, structured, isErr := m.call(name, args)
	if isErr != fail {
		t.Errorf("%s %v: failed %v, want %v: %s", name, args, isErr, fail, text)
	}
	if !strings.Contains(text, want) {
		t.Errorf("%s %v: %q does not say %q", name, args, text, want)
	}
	if structured == nil {
		t.Errorf("%s: no structured content", name)
	}
	if n := e.events(t) - before; n != 1 {
		t.Errorf("%s %v: %d audit Events, want 1", name, args, n)
	}
	if n := strings.Count(e.log.String(), "\n") - lines; n != 1 {
		t.Errorf("%s: %d log lines, want 1", name, n)
	}
	last := e.log.String()[strings.LastIndex(strings.TrimRight(e.log.String(), "\n"), "\n")+1:]
	if !strings.Contains(last, `"tool":"`+name+`"`) {
		t.Errorf("%s: the log line %s names another tool", name, last)
	}
	return structured
}

func (e *serveEnv) events(t *testing.T) int {
	t.Helper()
	evs, err := e.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

func TestEnvtestServeRefusesWithoutAValidToken(t *testing.T) {
	e := newServeEnv(t)
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for name, header := range map[string]string{
		"no token":     "",
		"garbage":      "Bearer x.y.z",
		"not a member": "Bearer " + e.iss.Token(t, "muster", identitytest.Claims{Email: "eve@example.com", EmailVerified: identitytest.Verified(), Groups: []string{teamGroup}}),
	} {
		req, _ := http.NewRequest(http.MethodPost, e.url, body)
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, res.StatusCode)
		}
	}
	if n := strings.Count(e.log.String(), `"outcome":"refused"`); n != 3 {
		t.Errorf("%d refusals logged, want 3:\n%s", n, e.log.String())
	}
}

func TestEnvtestServeTools(t *testing.T) {
	e := newServeEnv(t)
	ana := e.as(t, e.token(t, "ana@example.com", teamGroup))
	bo := e.as(t, e.token(t, "bo@example.com", teamGroup))
	sup := e.as(t, e.token(t, "sup@example.com", teamGroup, "giantswarm:bumblebee-supervisors"))
	pia := e.as(t, e.token(t, "pia@example.com", "giantswarm:team-planeteers"))
	agent := func(args map[string]any) map[string]any {
		args[paramAgent], args[paramHost] = anaAgent, lab
		return args
	}

	// Leases: one holder, the holder or the team's supervisor releases.
	got := e.expect(t, ana, "lease_claim", agent(map[string]any{paramEnvironment: graveler, paramPurpose: "e2e for #123"}), false, "claimed graveler")
	if h, _ := got["holder"].(map[string]any); h["person"] != "ana@example.com" || h["team"] != ourTeam || h["name"] != "ana-agent" {
		t.Errorf("the lease's holder %v", got["holder"])
	}
	e.expect(t, ana, "lease_claim", agent(map[string]any{paramEnvironment: graveler, paramPurpose: "again"}), false, "already yours")
	e.expect(t, bo, "lease_claim", map[string]any{paramEnvironment: graveler, paramPurpose: "mine"}, true, `graveler is held by "ana@example.com/ana-agent"`)
	e.expect(t, bo, "lease_claim", agent(map[string]any{paramEnvironment: graveler, paramPurpose: "same agent name"}), true, `graveler is held by "ana@example.com/ana-agent"`)
	e.expect(t, bo, "lease_claim", map[string]any{paramEnvironment: "nowhere", paramPurpose: "x"}, true, "not an Environment")
	e.expect(t, bo, "lease_release", map[string]any{paramEnvironment: graveler}, true, "only that person or team bumblebee's supervisor role")
	list := e.expect(t, pia, "lease_list", map[string]any{}, false, "free: glean")
	if held, _ := list["held"].([]any); len(held) != 1 {
		t.Errorf("lease_list held %v", list["held"])
	}
	e.expect(t, sup, "lease_release", map[string]any{paramEnvironment: graveler}, false, `released graveler (held by "ana@example.com/ana-agent": e2e for #123)`)
	e.expect(t, ana, "lease_release", map[string]any{paramEnvironment: graveler}, false, "graveler was free")
	e.expect(t, ana, "lease_claim", map[string]any{paramEnvironment: glean, paramPurpose: "proof"}, false, "claimed glean")
	e.expect(t, ana, "lease_release", agent(map[string]any{paramEnvironment: glean}), false, "released glean")

	// Holds: anyone sets one; its setter or the setter's team's supervisor lifts it.
	e.expect(t, ana, "hold_set", map[string]any{paramTarget: backstage, paramReason: "portal upgrade"}, false, "held giantswarm/backstage until lifted: portal upgrade")
	e.expect(t, pia, "hold_set", map[string]any{paramTarget: backstage, paramReason: "mine now"}, true, "ana@example.com's")
	e.expect(t, ana, "hold_list", map[string]any{}, false, "portal upgrade")
	e.expect(t, pia, "hold_lift", map[string]any{paramTarget: backstage}, true, "only that person or team bumblebee's supervisor role")
	e.expect(t, ana, "hold_set", map[string]any{paramTarget: "lane:nowhere", paramReason: "x"}, true, "no lane")
	e.expect(t, ana, "hold_lift", map[string]any{paramTarget: backstage}, false, "lifted the hold on giantswarm/backstage")
	e.expect(t, ana, "hold_lift", map[string]any{paramTarget: backstage}, false, "was not held")

	// Lanes: queue, turn, settle.
	e.expect(t, ana, "lane_queue", map[string]any{paramRepo: backstage, "pr": 1}, false, "queued giantswarm/backstage#1 in lane portal, number 1")
	e.expect(t, bo, "lane_queue", map[string]any{paramRepo: backstage, "pr": 2}, false, "number 2")
	e.expect(t, bo, "lane_queue", map[string]any{paramRepo: backstage, "pr": 2}, false, "already number 2")
	e.expect(t, bo, "lane_turn", map[string]any{paramRepo: backstage, "pr": 2}, false, "behind giantswarm/backstage#1")
	turn := e.expect(t, ana, "lane_turn", map[string]any{paramRepo: backstage, "pr": 1}, false, "your turn")
	if turn["turn"] != true {
		t.Errorf("lane_turn %v", turn)
	}
	e.expect(t, ana, "lane_turn", map[string]any{paramRepo: backstage, "pr": 9}, true, "not queued")
	e.expect(t, ana, "lane_queue", map[string]any{paramRepo: "giantswarm/muster", "pr": 1}, true, "no MergeLane carries giantswarm/muster")
	e.expect(t, bo, "lane_settle", map[string]any{paramRepo: backstage, "pr": 2}, false, "heads lane portal")
	e.expect(t, ana, "lane_settle", map[string]any{paramRepo: backstage, "pr": 1}, true, "lane portal is busy")
	e.expect(t, ana, "lane_turn", map[string]any{paramRepo: backstage, "pr": 1}, false, "behind giantswarm/backstage#2")
	e.expect(t, pia, "lanes", map[string]any{}, false, "giantswarm/backstage#2")

	// Notes: filed into the filer's team; answered by its addressee.
	note := e.expect(t, ana, "note_add", map[string]any{paramText: "which lane for muster?", paramFor: "bo@example.com", paramKind: noteMemo}, false, "note #1")
	if by, _ := note["by"].(map[string]any); by["team"] != ourTeam {
		t.Errorf("note_add %v", note)
	}
	e.expect(t, pia, "note_list", map[string]any{}, false, "which lane for muster?")
	e.expect(t, pia, "note_answer", map[string]any{paramNote: 1, paramText: "the merge lane"}, true, "only its addressee")
	e.expect(t, bo, "note_answer", map[string]any{paramNote: 1, paramText: "the portal lane"}, false, "note #1 answered and closed")
	e.expect(t, bo, "note_answer", map[string]any{paramNote: 1, paramText: "again"}, true, "not open")

	// The roster.
	e.expect(t, ana, "agents_register", map[string]any{paramAgent: anaAgent}, true, "host is required")
	e.expect(t, ana, "agents_register", agent(map[string]any{}), false, "register: ana-agent idle")
	e.expect(t, ana, "agents_register", agent(map[string]any{}), false, "register: ana-agent idle")
	e.expect(t, pia, "agents_register", map[string]any{paramAgent: anaAgent, paramHost: lab}, true, "ana-agent on lab is ana@example.com's agent")
	e.expect(t, pia, "agents_register", map[string]any{paramAgent: anaAgent, paramHost: "laptop"}, false, "register: ana-agent idle")
	e.expect(t, pia, "list_agents", map[string]any{"scope": "team"}, false, "planeteers")
	e.expect(t, ana, "list_agents", map[string]any{}, false, "ana@example.com")

	snap := e.expect(t, ana, "snapshot", map[string]any{}, false, "== Agents")
	for _, k := range []string{"leases", "holds", "lanes", "notes", "agents"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot lacks %s", k)
		}
	}
}

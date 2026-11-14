package fakegithub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
	"github.com/giantswarm/beekeeper/internal/settle"
)

var start = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

const secret = "fixture-webhook-secret"

// Fixtures of the seed and the steps: people, repositories, issues, pull requests and board items,
// board options and checks.
const (
	alice      = "alice"
	bob        = "bob"
	fixtureOrg = "giantswarm"

	repoKlaus      = "giantswarm/klaus"
	repoHappa      = "giantswarm/happa"
	repoRoadmap    = "giantswarm/roadmap"
	repoOR         = "o/r"
	refKlaus55     = "giantswarm/klaus#55"
	refRoadmap90   = "giantswarm/roadmap#90"
	refRoadmap100  = "giantswarm/roadmap#100"
	refRoadmap101  = "giantswarm/roadmap#101"
	refOR1         = "o/r#1"
	itemRoadmap101 = "PVTI_giantswarm_273_giantswarm/roadmap#101"

	optAtlas      = "Atlas 🗺️"
	optBumblebee  = "Bumblebee🐝"
	optUpNext     = "Up Next ➡️"
	optInProgress = "In Progress ⛏️"
	optValidation = "Validation ☑️"
	optStory      = "Story 📑"
	optBug        = "Bug 🐞"
	teamFilter    = `team:"Bumblebee🐝"`

	checkGoTest       = "ci/circleci: go-test"
	conclusionSuccess = "success"
)

// Keys of the seed, variables of the GraphQL queries, and the header the requests authenticate with.
const (
	keyIssues     = "issues"
	keyVisibility = "visibility"

	varOrg    = "org"
	varNumber = "number"
	varFirst  = "first"
	varQuery  = "query"
	varIDs    = "ids"

	headerAuthorization = "Authorization"
)

type delivery struct {
	event, guid, action string
	body                map[string]any
	at                  time.Time
}

type receiver struct {
	mu     sync.Mutex
	clock  *clock.Fake
	got    []delivery
	status int
	badSig int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Header.Get(wire.HeaderSignature) != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		r.badSig++
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	action, _ := m["action"].(string)
	r.got = append(r.got, delivery{event: req.Header.Get(wire.HeaderEvent), guid: req.Header.Get(wire.HeaderDelivery), action: action, body: m, at: r.clock.Now()})
	if r.status != 0 {
		w.WriteHeader(r.status)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (r *receiver) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

func (r *receiver) events() []string {
	var out []string
	for _, d := range r.deliveries() {
		e := d.event
		if d.action != "" {
			e += "." + d.action
		}
		out = append(out, e)
	}
	return out
}

type fx struct {
	t     *testing.T
	clock *clock.Fake
	s     *Server
	recv  *receiver
	token string
}

func seedMap() map[string]any {
	return map[string]any{
		"app":    map[string]any{"id": 100, "installation": 200, "webhookSecret": secret},
		argBoard: map[string]any{"project": 273, "teams": []any{optAtlas, optBumblebee}},
		"repos": []any{
			map[string]any{argName: repoRoadmap, keyVisibility: visibilityPublic},
			map[string]any{argName: repoKlaus, keyVisibility: visibilityPublic, "defaultBranch": "main", "requiredChecks": []any{checkGoTest}, "requiredApprovals": 1},
			map[string]any{argName: repoHappa, keyVisibility: "private", "installed": false},
			map[string]any{argName: "alice/klaus-experiments", keyVisibility: visibilityPublic, "installed": false},
		},
		keyIssues: []any{
			map[string]any{argRef: refRoadmap90, argTitle: "Epic", argAssignees: []any{alice}, argBoard: map[string]any{argStatus: optUpNext, argKind: "Epic 🎯", argTeam: optBumblebee}},
			map[string]any{argRef: refRoadmap100, argTitle: "Blocker"},
			map[string]any{argRef: refRoadmap101, argTitle: "Retry", argBody: "b", argAssignees: []any{alice}, argBoard: map[string]any{argStatus: optInProgress, argKind: optStory, argTeam: optBumblebee}},
			map[string]any{argRef: "giantswarm/happa#1", argTitle: "Private", argBoard: map[string]any{argStatus: optUpNext, argKind: optStory, argTeam: optBumblebee}},
		},
		"pulls": []any{
			map[string]any{argRef: refKlaus55, argAuthor: alice, argHead: "a1", argCloses: []any{refRoadmap101},
				"checks": []any{map[string]any{argName: checkGoTest, argStatus: "in_progress"}}},
		},
	}
}

func newFx(t *testing.T) *fx {
	t.Helper()
	c := clock.NewFake(start)
	recv := &receiver{clock: c}
	hs := httptest.NewServer(recv)
	t.Cleanup(hs.Close)
	s, err := New(Options{Clock: c, Tracker: settle.New(), WebhookURL: hs.URL, WebhookSecret: "options-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Seed(seedMap()); err != nil {
		t.Fatal(err)
	}
	return &fx{t: t, clock: c, s: s, recv: recv}
}

func (f *fx) do(tool string, args map[string]any) map[string]any {
	f.t.Helper()
	res, err := f.s.Do(context.Background(), tool, args)
	if err != nil {
		f.t.Fatalf("%s: %v", tool, err)
	}
	return res.(map[string]any)
}

func (f *fx) calls(args map[string]any) map[string]any { return f.do(toolCalls, args) }

// mint gets an installation token as the client would.
func (f *fx) mint() string {
	f.t.Helper()
	if f.token != "" {
		return f.token
	}
	resp := f.req(http.MethodPost, "/app/installations/200/access_tokens", readOnly(), map[string]string{headerAuthorization: "Bearer " + f.jwt()})
	var it wire.InstallationToken
	if err := json.Unmarshal(resp.body, &it); err != nil || resp.code != http.StatusCreated {
		f.t.Fatalf("minting: %d %s", resp.code, resp.body)
	}
	f.token = it.Token
	return it.Token
}

// readOnly is the body of a mint that asks for read permissions only, as the client sends it.
func readOnly() []byte {
	b, _ := json.Marshal(wire.InstallationTokenRequest{Permissions: wire.ReadPermissions()})
	return b
}

func (f *fx) jwt() string {
	f.t.Helper()
	tok, err := signJWT(f.s.key, "100", f.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (f *fx) req(method, path string, body []byte, headers map[string]string) resp {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.s.URL()+path, bytes.NewReader(body))
	req.Header.Set("User-Agent", "beekeeper/test (engine-1)")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }() // read to the end; closing can't lose anything
	b, _ := io.ReadAll(r.Body)
	return resp{code: r.StatusCode, header: r.Header, body: b}
}

func (f *fx) get(path string, etag string) resp {
	h := map[string]string{headerAuthorization: "token " + f.mint()}
	if etag != "" {
		h["If-None-Match"] = etag
	}
	return f.req(http.MethodGet, path, nil, h)
}

func (f *fx) graphql(op, doc string, vars map[string]any) (resp, map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(wire.GraphQLRequest{Query: doc, OperationName: op, Variables: vars})
	r := f.req(http.MethodPost, "/graphql", b, map[string]string{headerAuthorization: "bearer " + f.mint()})
	var m map[string]any
	_ = json.Unmarshal(r.body, &m)
	return r, m
}

func TestSeedIsStrict(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]map[string]any{
		"unknown top-level key": {"nonsense": 1},
		"unknown issue key":     {keyIssues: []any{map[string]any{argRef: refOR1, "titel": "x"}}},
		"unknown team":          {keyIssues: []any{map[string]any{argRef: refOR1, argBoard: map[string]any{argStatus: optUpNext, argTeam: "Bumblebee 🐝"}}}},
		"unknown status":        {keyIssues: []any{map[string]any{argRef: refOR1, argBoard: map[string]any{argStatus: "Doing"}}}},
		"bad ref":               {keyIssues: []any{map[string]any{argRef: repoOR}}},
		"bad state":             {keyIssues: []any{map[string]any{argRef: refOR1, argState: "gone"}}},
		"merged without sha":    {"pulls": []any{map[string]any{argRef: refOR1, argHead: "a", argState: "merged"}}},
		"unknown API":           {"rateLimit": map[string]any{"search": map[string]any{"limit": 1}}},
	} {
		s, err := New(Options{Clock: clock.NewFake(start)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Seed(seed); err == nil {
			t.Errorf("%s: no error", name)
		}
		s.Close()
	}
}

func TestStateChangesSendSignedWebhooks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
		// check inspects the first delivery's payload.
		check func(t *testing.T, body map[string]any)
	}{
		{tool: toolIssueCreate, args: map[string]any{argRef: "giantswarm/roadmap#130", argTitle: "Bug", argBoard: map[string]any{argStatus: "Inbox 📥", argKind: optBug, argTeam: optBumblebee}},
			want: []string{"issues.opened", "projects_v2_item.created"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "issue.number") != 130.0 || path(b, "repository.full_name") != repoRoadmap || path(b, "installation.id") != 200.0 {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolIssueEdit, args: map[string]any{argRef: refRoadmap101, argTitle: "New", argAssignees: []any{bob}, argLabels: []any{"x"}, argState: stateClosed},
			want: []string{"issues.edited", "issues.unassigned", "issues.assigned", "issues.labeled", "issues.closed"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "changes.title.from") != "Retry" {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolBoardSet, args: map[string]any{argIssue: refRoadmap101, argStatus: optValidation, argPriority: 1, argTeam: optBumblebee},
			want: []string{"projects_v2_item.edited", "projects_v2_item.edited"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "projects_v2_item.node_id") != itemRoadmap101 ||
					path(b, "projects_v2_item.project_node_id") != "PVT_giantswarm_273" ||
					path(b, "projects_v2_item.content_node_id") != "I_giantswarm/roadmap#101" ||
					path(b, "changes.field_value.field_name") != "Status" || path(b, "changes.field_value.to.name") != optValidation ||
					path(b, "changes.field_value.from.name") != optInProgress || b["repository"] != nil {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolBoardAdd, args: map[string]any{argIssue: refRoadmap100, argBoard: map[string]any{argStatus: optUpNext, argTeam: optAtlas}},
			want: []string{"projects_v2_item.created"}},
		{tool: toolBoardArchive, args: map[string]any{argIssue: refRoadmap101}, want: []string{"projects_v2_item.archived"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "projects_v2_item.archived_at") == nil {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolBoardRemove, args: map[string]any{argIssue: refRoadmap101}, want: []string{"projects_v2_item.deleted"}},
		{tool: toolSubIssue, args: map[string]any{argParent: refRoadmap90, argChild: refRoadmap101, argOp: opAdd},
			want: []string{"sub_issues.sub_issue_added"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "parent_issue.number") != 90.0 || path(b, "sub_issue.number") != 101.0 || path(b, "sub_issue_repo.full_name") != repoRoadmap {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolDependency, args: map[string]any{argIssue: refRoadmap101, argBlockedBy: refRoadmap100, argOp: opAdd},
			want: []string{"issue_dependencies.blocked_by_added"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "blocked_issue.number") != 101.0 || path(b, "blocking_issue.number") != 100.0 {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolPROpen, args: map[string]any{argRef: "giantswarm/klaus#56", argAuthor: bob, argHead: "b1", argCloses: []any{refRoadmap101}},
			want: []string{"pull_request.opened"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "pull_request.number") != 56.0 || path(b, "pull_request.head.sha") != "b1" || path(b, "pull_request.base.ref") != "main" {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolPRPush, args: map[string]any{argRef: refKlaus55, argHead: "a2"}, want: []string{"pull_request.synchronize"},
			check: func(t *testing.T, b map[string]any) {
				if b["before"] != "a1" || b["after"] != "a2" || path(b, "pull_request.head.sha") != "a2" {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolPRReview, args: map[string]any{argRef: refKlaus55, argBy: bob, argState: "changes_requested"},
			want: []string{"pull_request_review.submitted"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "review.state") != "changes_requested" || path(b, "review.user.login") != bob || path(b, "pull_request.number") != 55.0 {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolPRMerge, args: map[string]any{argRef: refKlaus55, argMergeSHA: "m1", argBy: alice},
			want: []string{"pull_request.closed", "push", "issues.closed"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "pull_request.merged") != true || path(b, "pull_request.merge_commit_sha") != "m1" {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolPRClose, args: map[string]any{argRef: refKlaus55}, want: []string{"pull_request.closed"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "pull_request.merged") != false {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolCheckRun, args: map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: checkGoTest, argStatus: checkCompleted, argConclusion: conclusionSuccess},
			want: []string{"check_run.completed"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "check_run.head_sha") != "a1" || path(b, "check_run.conclusion") != conclusionSuccess || path(b, "check_run.pull_requests.0.number") != 55.0 {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolCheckRun, args: map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: "lint", argStatus: checkQueued}, want: []string{"check_run.created"}},
		{tool: toolCheckRun, args: map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: checkGoTest, argStatus: "in_progress"}, want: nil},
		{tool: toolStatus, args: map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "ci/circleci: go-build", argState: statusSuccess},
			want: []string{wire.EventStatus},
			check: func(t *testing.T, b map[string]any) {
				if b["sha"] != "a1" || b["context"] != "ci/circleci: go-build" || b["state"] != statusSuccess || b["name"] != repoKlaus {
					t.Fatalf("payload: %v", b)
				}
			}},
		{tool: toolInstallationRepos, args: map[string]any{argAdd: []any{repoHappa}, argRemove: []any{repoKlaus}},
			want: []string{"installation_repositories.added", "installation_repositories.removed"},
			check: func(t *testing.T, b map[string]any) {
				if path(b, "repositories_added.0.full_name") != repoHappa {
					t.Fatalf("payload: %v", b)
				}
			}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.do(tc.tool, tc.args)
			got := f.recv.events()
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("webhooks: %v, want %v", got, tc.want)
			}
			if f.recv.badSig != 0 {
				t.Fatal("a delivery's signature doesn't verify with the seed's secret")
			}
			log := f.s.Deliveries()
			if len(log) != len(tc.want) {
				t.Fatalf("log: %d entries", len(log))
			}
			for i, e := range log {
				if e.StatusCode != http.StatusAccepted || e.Status != "OK" || e.GUID != f.recv.deliveries()[i].guid || !e.DeliveredAt.Equal(start) {
					t.Fatalf("log entry: %+v", e.HookDelivery)
				}
			}
			if tc.check != nil {
				tc.check(t, f.recv.deliveries()[0].body)
			}
		})
	}
}

func path(m map[string]any, p string) any {
	var cur any = m
	for _, k := range strings.Split(p, ".") {
		switch x := cur.(type) {
		case map[string]any:
			cur = x[k]
		case []any:
			i := 0
			for _, c := range k {
				i = i*10 + int(c-'0')
			}
			if i >= len(x) {
				return nil
			}
			cur = x[i]
		default:
			return nil
		}
	}
	return cur
}

func TestMergedPRClosesItsIssuesOnTheDefaultBranchOnly(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.do(toolPROpen, map[string]any{argRef: "giantswarm/klaus#60", argAuthor: bob, argHead: "x1", argBase: "release", argCloses: []any{refRoadmap100}})
	f.do(toolPRMerge, map[string]any{argRef: "giantswarm/klaus#60", argMergeSHA: "mx"})
	if f.s.st.issues[refRoadmap100].state != stateOpen {
		t.Fatal("a merge into another branch closed the issue")
	}
	f.do(toolPRMerge, map[string]any{argRef: refKlaus55, argMergeSHA: "m1"})
	is := f.s.st.issues[refRoadmap101]
	if is.state != stateClosed || is.stateReason != reasonCompleted {
		t.Fatalf("issue: %s %s", is.state, is.stateReason)
	}
}

func TestDeliverModes(t *testing.T) {
	t.Parallel()
	check := map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: checkGoTest, argStatus: checkCompleted, argConclusion: conclusionSuccess}
	with := func(deliver any) map[string]any {
		m := map[string]any{}
		for k, v := range check {
			m[k] = v
		}
		m["deliver"] = deliver
		return m
	}

	t.Run(deliverLost, func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.do(toolCheckRun, with(deliverLost))
		if len(f.recv.deliveries()) != 0 {
			t.Fatal("a lost delivery arrived")
		}
		log := f.s.Deliveries()
		if len(log) != 1 || log[0].StatusCode != 0 || log[0].Event != "check_run" || len(log[0].Request.Payload) == 0 {
			t.Fatalf("log: %+v", log)
		}
	})
	t.Run(deliverNever, func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.do(toolCheckRun, with(deliverNever))
		if len(f.recv.deliveries()) != 0 || len(f.s.Deliveries()) != 0 {
			t.Fatal("a delivery that never happened was sent or logged")
		}
		if f.s.st.checks["giantswarm/klaus@a1"][0].status != checkCompleted {
			t.Fatal("the state didn't change")
		}
	})
	t.Run(deliverDuplicate, func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.do(toolCheckRun, with(deliverDuplicate))
		got := f.recv.deliveries()
		if len(got) != 2 || got[0].guid != got[1].guid || len(f.s.Deliveries()) != 2 {
			t.Fatalf("duplicate: %+v", got)
		}
	})
	t.Run(deliverDelay, func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.do(toolCheckRun, with(map[string]any{deliverDelay: "20m"}))
		if len(f.recv.deliveries()) != 0 || len(f.s.Deliveries()) != 0 {
			t.Fatal("a delayed delivery came early")
		}
		f.clock.Advance(19 * time.Minute)
		if len(f.recv.deliveries()) != 0 {
			t.Fatal("a delayed delivery came early")
		}
		f.clock.Advance(time.Minute)
		got := f.recv.deliveries()
		if len(got) != 1 || !got[0].at.Equal(start.Add(20*time.Minute)) {
			t.Fatalf("delayed: %+v", got)
		}
		if log := f.s.Deliveries(); len(log) != 1 || !log[0].DeliveredAt.Equal(start.Add(20*time.Minute)) {
			t.Fatalf("log: %+v", log)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		for _, d := range []any{"sometimes", map[string]any{deliverDelay: "soon"}, map[string]any{"wait": "1m"}} {
			if _, err := f.s.Do(context.Background(), toolCheckRun, with(d)); err == nil {
				t.Errorf("deliver %v: no error", d)
			}
		}
	})
	t.Run("receiver failure is logged", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.recv.status = http.StatusServiceUnavailable
		f.do(toolCheckRun, check)
		if log := f.s.Deliveries(); len(log) != 1 || log[0].StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("log: %+v", log)
		}
	})
	t.Run("repo outside the installation sends nothing", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.do(toolPROpen, map[string]any{argRef: "alice/klaus-experiments#3", argAuthor: alice, argHead: "c1", argCloses: []any{refRoadmap101}})
		f.do(toolStatus, map[string]any{argRepo: repoHappa, argSHA: "h1", argContext: "c", argState: statusSuccess})
		if len(f.recv.deliveries()) != 0 || len(f.s.Deliveries()) != 0 {
			t.Fatal("a repo outside the installation sent a webhook")
		}
	})
}

func TestDeliveriesFault(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{deliverLost, deliverNever} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newFx(t)
			f.do(toolCheckRun, map[string]any{argRepo: repoKlaus, argSHA: "a1", argName: "early", argStatus: checkQueued, argDeliver: map[string]any{deliverDelay: "10m"}})
			f.do(toolFault, map[string]any{argKind: faultDeliveries, argMode: mode})
			f.do(toolStatus, map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "c", argState: statusSuccess})
			f.do(toolBoardSet, map[string]any{argIssue: refRoadmap101, argStatus: optValidation})
			f.clock.Advance(10 * time.Minute) // the delayed delivery falls in the fault too
			if len(f.recv.deliveries()) != 0 {
				t.Fatal("a delivery arrived during the fault")
			}
			wantLogged := 0
			if mode == deliverLost {
				wantLogged = 3
			}
			if n := len(f.s.Deliveries()); n != wantLogged {
				t.Fatalf("logged: %d, want %d", n, wantLogged)
			}
			f.do(toolFault, map[string]any{argKind: faultClear})
			f.do(toolStatus, map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "c", argState: statusFailure})
			if got := f.recv.events(); len(got) != 1 || got[0] != wire.EventStatus {
				t.Fatalf("after clear: %v", got)
			}
		})
	}
}

func TestFaults(t *testing.T) {
	t.Parallel()
	t.Run("http_error with times and match", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.do(toolFault, map[string]any{argKind: faultHTTP, argStatus: 502, argTimes: 2, argMatch: "/repos/giantswarm/klaus/commits/*/check-runs"})
		for i, want := range []int{502, 502, 200} {
			if r := f.get("/repos/giantswarm/klaus/commits/a1/check-runs", ""); r.code != want {
				t.Fatalf("call %d: %d, want %d", i, r.code, want)
			}
		}
		if r := f.get("/repos/giantswarm/klaus/pulls/55", ""); r.code != 200 {
			t.Fatalf("unmatched path: %d", r.code)
		}
	})
	t.Run("http_error by GraphQL operation and api", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.mint()
		f.do(toolFault, map[string]any{argKind: faultHTTP, argStatus: 504, argMatch: "BoardProbe"})
		f.do(toolFault, map[string]any{argKind: faultHTTP, argStatus: 500, argAPI: forge.APIApp})
		r, _ := f.graphql("BoardProbe", wire.BoardProbeQuery, map[string]any{varOrg: fixtureOrg, varNumber: 273, varQuery: teamFilter, varFirst: 10})
		if r.code != 504 {
			t.Fatalf("probe: %d", r.code)
		}
		if r, _ := f.graphql("BoardSchema", wire.BoardSchemaQuery, map[string]any{varOrg: fixtureOrg, varNumber: 273}); r.code != 200 {
			t.Fatalf("schema: %d", r.code)
		}
		if r := f.req(http.MethodGet, "/app/hook/deliveries", nil, map[string]string{headerAuthorization: "Bearer " + f.jwt()}); r.code != 500 {
			t.Fatalf("app: %d", r.code)
		}
		f.do(toolFault, map[string]any{argKind: faultClear})
		if r := f.req(http.MethodGet, "/app/hook/deliveries", nil, map[string]string{headerAuthorization: "Bearer " + f.jwt()}); r.code != 200 {
			t.Fatalf("after clear: %d", r.code)
		}
	})
	t.Run(faultSecondary, func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.mint()
		f.do(toolFault, map[string]any{argKind: faultSecondary, argRetryAfter: "60s", argTimes: 1})
		r := f.get("/repos/giantswarm/klaus/pulls/55", "")
		if r.code != 403 || r.header.Get("Retry-After") != "60" || !strings.Contains(string(r.body), "secondary rate limit") {
			t.Fatalf("secondary: %d %v %s", r.code, r.header, r.body)
		}
		if c := f.calls(map[string]any{argPath: "/repos/giantswarm/klaus/pulls/55"}); c["rateLimited"] != 1 {
			t.Fatalf("calls: %v", c)
		}
	})
	t.Run("rate_limit models other users and resets", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		f.mint()
		f.do(toolFault, map[string]any{argKind: faultRateLimit, argAPI: forge.APIREST, argRemaining: 1, argResetIn: "30m"})
		r := f.get("/repos/giantswarm/klaus/pulls/55", "")
		if r.code != 200 || r.header.Get(wire.HeaderRateRemaining) != "0" || r.header.Get(wire.HeaderRateReset) != itoa64(start.Add(30*time.Minute).Unix()) {
			t.Fatalf("last call: %d %v", r.code, r.header)
		}
		r = f.get("/repos/giantswarm/klaus/pulls/55", "")
		if r.code != 403 || r.header.Get(wire.HeaderRateRemaining) != "0" {
			t.Fatalf("exhausted: %d", r.code)
		}
		f.do(toolFault, map[string]any{argKind: faultRateLimit, argAPI: forge.APIGraphQL, argRemaining: 0})
		_, m := f.graphql("BoardSchema", wire.BoardSchemaQuery, map[string]any{varOrg: fixtureOrg, varNumber: 273})
		if path(m, "errors.0.type") != wire.ErrorTypeRateLimited {
			t.Fatalf("GraphQL: %v", m)
		}
		if c := f.calls(nil); c["rateLimited"] != 2 {
			t.Fatalf("calls: %v", c)
		}
		f.clock.Advance(30 * time.Minute)
		if r := f.get("/repos/giantswarm/klaus/pulls/55", ""); r.code != 200 || r.header.Get(wire.HeaderRateRemaining) != "4999" {
			t.Fatalf("after reset: %d %s", r.code, r.header.Get(wire.HeaderRateRemaining))
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		f := newFx(t)
		for _, args := range []map[string]any{
			{argKind: "explode"},
			{argKind: faultHTTP, argMode: deliverLost},
			{argKind: faultHTTP, argStatus: 200},
			{argKind: faultRateLimit, argAPI: forge.APIREST},
			{argKind: faultRateLimit, argAPI: "search", argRemaining: 1},
			{argKind: faultDeliveries, argMode: deliverDuplicate},
			{argKind: faultSecondary, argRetryAfter: "soon"},
		} {
			if _, err := f.s.Do(context.Background(), toolFault, args); err == nil {
				t.Errorf("%v: no error", args)
			}
		}
	})
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestETagsAnswer304WithoutCost(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	r := f.get("/repos/giantswarm/klaus/pulls/55", "")
	etag := r.header.Get("ETag")
	rem := r.header.Get(wire.HeaderRateRemaining)
	if r.code != 200 || etag == "" {
		t.Fatalf("first: %d %q", r.code, etag)
	}
	r = f.get("/repos/giantswarm/klaus/pulls/55", etag)
	if r.code != 304 || r.header.Get(wire.HeaderRateRemaining) != rem || len(r.body) != 0 {
		t.Fatalf("conditional: %d remaining %s (was %s)", r.code, r.header.Get(wire.HeaderRateRemaining), rem)
	}
	f.clock.Advance(time.Minute)
	f.do(toolPRReview, map[string]any{argRef: refKlaus55, argBy: bob, argState: "approved"})
	if r = f.get("/repos/giantswarm/klaus/pulls/55", etag); r.code != 200 {
		t.Fatalf("after a change: %d", r.code)
	}
	c := f.calls(map[string]any{argPath: "/repos/giantswarm/klaus/pulls/55"})
	if c["count"] != 3 || c["conditional"] != 2 || c["notModified"] != 1 || c["modified"] != 2 {
		t.Fatalf("calls: %v", c)
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	if r := f.req(http.MethodGet, "/repos/giantswarm/klaus", nil, nil); r.code != 401 {
		t.Fatalf("no token: %d", r.code)
	}
	if r := f.req(http.MethodGet, "/repos/giantswarm/klaus", nil, map[string]string{headerAuthorization: "token forged"}); r.code != 401 {
		t.Fatalf("forged token: %d", r.code)
	}
	if r := f.req(http.MethodGet, "/app/hook/deliveries", nil, map[string]string{headerAuthorization: "token " + f.mint()}); r.code != 401 {
		t.Fatalf("installation token on an App endpoint: %d", r.code)
	}
	if r := f.req(http.MethodPost, "/app/installations/999/access_tokens", readOnly(), map[string]string{headerAuthorization: "Bearer " + f.jwt()}); r.code != 404 {
		t.Fatalf("other installation: %d", r.code)
	}
	tok := f.mint()
	f.clock.Advance(time.Hour)
	if r := f.req(http.MethodGet, "/repos/giantswarm/klaus", nil, map[string]string{headerAuthorization: "token " + tok}); r.code != 401 {
		t.Fatalf("expired token: %d", r.code)
	}
	old, _ := signJWT(f.s.key, "100", start)
	if r := f.req(http.MethodGet, "/app/hook/deliveries", nil, map[string]string{headerAuthorization: "Bearer " + old}); r.code != 401 {
		t.Fatalf("expired JWT: %d", r.code)
	}
	other, _ := signJWT(f.s.key, "101", f.clock.Now())
	if r := f.req(http.MethodGet, "/app/hook/deliveries", nil, map[string]string{headerAuthorization: "Bearer " + other}); r.code != 401 {
		t.Fatalf("other App's JWT: %d", r.code)
	}
}

func TestVisibility(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for p, want := range map[string]int{
		"/repos/giantswarm/happa":                     404,
		"/repos/giantswarm/happa/issues/1":            404,
		"/repos/alice/klaus-experiments":              200,
		"/repos/giantswarm/klaus/rules/branches/main": 200,
	} {
		if r := f.get(p, ""); r.code != want {
			t.Errorf("%s: %d, want %d", p, r.code, want)
		}
	}
	_, m := f.graphql("BoardItemsByNode", wire.BoardItemsByNodeQuery, map[string]any{varIDs: []any{"PVTI_giantswarm_273_giantswarm/happa#1"}})
	if path(m, "data.nodes.0.type") != wire.ItemTypeRedacted || path(m, "data.nodes.0.content") != nil {
		t.Fatalf("redacted item: %v", m)
	}
	f.do(toolInstallationRepos, map[string]any{argAdd: []any{repoHappa}})
	if r := f.get("/repos/giantswarm/happa/issues/1", ""); r.code != 200 {
		t.Fatalf("installed: %d", r.code)
	}
}

func TestGraphQLPointsAndMutations(t *testing.T) {
	t.Parallel()
	const big = "Big" // an operation the fake doesn't know: it answers errors, but costs its points
	f := newFx(t)
	team := map[string]any{
		varOrg: fixtureOrg, varNumber: 273,
		"activeQuery": `team:"Bumblebee🐝" status:"Up Next ➡️" is:open`, "activeFirst": wire.ActivePageSize, "activeAfter": nil, "withActive": true,
		"inboxQuery": `team:"Bumblebee🐝" status:"Inbox 📥" kind:"Bug 🐞" is:open`, "inboxFirst": wire.InboxPageSize, "inboxAfter": nil, "withInbox": true,
	}
	for _, tc := range []struct {
		op, doc string
		vars    map[string]any
		points  float64
	}{
		{"BoardSchema", wire.BoardSchemaQuery, map[string]any{varOrg: fixtureOrg, varNumber: 273}, 1},
		{"BoardTeamItems", wire.BoardTeamItemsQuery, team, 2},
		{"BoardItemsByNode", wire.BoardItemsByNodeQuery, map[string]any{varIDs: []any{itemRoadmap101}}, 1},
		{"BoardProbe", wire.BoardProbeQuery, map[string]any{varOrg: fixtureOrg, varNumber: 273, varQuery: teamFilter, varFirst: 100}, 1},
		// GitHub's rule on a page of 100 items with two nested connections: (1 + 100 + 100) / 100.
		{big, `query Big { organization(login: "giantswarm") { projectV2(number: 273) { items(first: 100) { nodes { content { ... on Issue { labels(first: 50) { nodes { name } } subIssues(first: 50) { nodes { number } } } } } } } } }`, nil, 2},
	} {
		r, m := f.graphql(tc.op, tc.doc, tc.vars)
		if r.code != 200 || path(m, "errors") != nil && tc.op != big {
			t.Fatalf("%s: %d %v", tc.op, r.code, m)
		}
		c := f.calls(map[string]any{argOperation: tc.op})
		if c["graphqlPoints"] != int(tc.points) {
			t.Errorf("%s: %v points, want %v", tc.op, c["graphqlPoints"], tc.points)
		}
		if tc.op != big && path(m, "data.rateLimit.cost") != tc.points {
			t.Errorf("%s: rateLimit.cost %v", tc.op, path(m, "data.rateLimit.cost"))
		}
	}
	// The aliased connection left out with @include costs nothing.
	team["withInbox"] = false
	f.s.MarkStep("one-alias")
	f.graphql("BoardTeamItems", wire.BoardTeamItemsQuery, team)
	if c := f.calls(map[string]any{argSinceStep: "one-alias"}); c["graphqlPoints"] != 2 { // 151 requests
		t.Errorf("one alias: %v", c)
	}
	f.s.MarkStep("mutation")
	r, m := f.graphql("", `mutation { updateProjectV2ItemFieldValue(input: {}) { clientMutationId } }`, nil)
	if r.code != 200 || path(m, "errors.0.message") == nil {
		t.Fatalf("mutation answered %d %v", r.code, m)
	}
	if c := f.calls(map[string]any{argSinceStep: "mutation"}); c["mutating"] != 1 || c[forge.APIGraphQL] != 1 {
		t.Fatalf("mutation calls: %v", c)
	}
}

func TestBoardQueries(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.do(toolIssueCreate, map[string]any{argRef: "giantswarm/roadmap#130", argTitle: "Bug", argBoard: map[string]any{argStatus: "Inbox 📥", argKind: optBug, argTeam: optBumblebee}})
	f.do(toolIssueCreate, map[string]any{argRef: "giantswarm/roadmap#131", argTitle: "Atlas", argBoard: map[string]any{argStatus: optInProgress, argKind: optStory, argTeam: optAtlas}})
	f.do(toolDependency, map[string]any{argIssue: refRoadmap101, argBlockedBy: refRoadmap100, argOp: opAdd})
	f.do(toolSubIssue, map[string]any{argParent: refRoadmap90, argChild: refRoadmap101, argOp: opAdd})
	vars := map[string]any{
		varOrg: fixtureOrg, varNumber: 273,
		"activeQuery": `team:"Bumblebee🐝" status:"Up Next ➡️","In Progress ⛏️" is:open`, "activeFirst": 1, "activeAfter": nil, "withActive": true,
		"inboxQuery": `team:"Bumblebee🐝" status:"Inbox 📥" kind:"Bug 🐞" is:open`, "inboxFirst": 15, "inboxAfter": nil, "withInbox": true,
	}
	var refs []string
	for page := 0; page < 5; page++ {
		_, m := f.graphql("BoardTeamItems", wire.BoardTeamItemsQuery, vars)
		var d struct {
			Data wire.BoardTeamItemsData `json:"data"`
		}
		b, _ := json.Marshal(m)
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		p := d.Data.Organization.ProjectV2
		if page == 0 {
			if p.Inbox == nil || len(p.Inbox.Nodes) != 1 || p.Inbox.Nodes[0].Content.Number != 130 {
				t.Fatalf("inbox: %+v", p.Inbox)
			}
			vars["withInbox"] = false
		}
		for _, n := range p.Active.Nodes {
			if n.Type == wire.ItemTypeRedacted {
				refs = append(refs, "redacted")
				continue
			}
			refs = append(refs, n.Content.Repository.NameWithOwner+"#"+itoa64(int64(n.Content.Number)))
			if n.Content.Number == 101 {
				if n.Content.Parent == nil || n.Content.Parent.Number != 90 || len(n.Content.BlockedBy.Nodes) != 1 || n.Content.ClosedBy.Nodes[0].Number != 55 {
					t.Fatalf("links: %+v", n.Content)
				}
			}
			if n.Content.Number == 90 && len(n.Content.SubIssues.Nodes) != 1 {
				t.Fatalf("sub-issues: %+v", n.Content.SubIssues)
			}
		}
		if p.Active.TotalCount != 3 {
			t.Fatalf("totalCount: %d", p.Active.TotalCount)
		}
		if !p.Active.PageInfo.HasNextPage {
			break
		}
		vars["activeAfter"] = p.Active.PageInfo.EndCursor
	}
	// The item of the private repo outside the installation matches, but comes back redacted.
	if strings.Join(refs, " ") != "giantswarm/roadmap#90 giantswarm/roadmap#101 redacted" {
		t.Fatalf("active: %v", refs)
	}
	// A misspelt option silently matches nothing, as on GitHub.
	vars["activeQuery"], vars["activeAfter"] = `team:"Bumblebee 🐝" status:"Up Next ➡️"`, nil
	_, m := f.graphql("BoardTeamItems", wire.BoardTeamItemsQuery, vars)
	if path(m, "data.organization.projectV2.active.totalCount") != 0.0 {
		t.Fatalf("misspelt: %v", m)
	}
}

func TestFilter(t *testing.T) {
	t.Parallel()
	st := newState()
	st.teams = []string{optBumblebee}
	now := start.Add(48 * time.Hour)
	p := 2.0
	st.repos[repoOR] = &repo{fullName: repoOR, installed: true}
	st.issues[refOR1] = &issue{ref: refOR1, repo: repoOR, title: "Retry errors", state: stateOpen, assignees: []string{alice}}
	it := &boardItem{issue: refOR1, status: optUpNext, team: optBumblebee, kind: optBug, priority: &p, updatedAt: start}
	for q, want := range map[string]bool{
		teamFilter:                        true,
		`team:"Bumblebee 🐝"`:              false,
		`team:Bumblebee`:                  false,
		`status:"Backlog 📦","Up Next ➡️"`: true,
		`-status:"Up Next ➡️"`:            false,
		`kind:"Bug 🐞" is:open`:            true,
		`is:closed`:                       false,
		`no:priority`:                     false,
		`has:priority priority:>1`:        true,
		`priority:<2`:                     false,
		`assignee:alice`:                  true,
		`no:assignee`:                     false,
		`updated:>=2026-11-16`:            true,
		`updated:>2026-11-16`:             false,
		`updated:>=@today-2d`:             true,
		`updated:>=@today-1d`:             false,
		`updated:>2026-11-16T06:00:00Z`:   false,
		`nonsensefield:"zzz"`:             false,
		`retry`:                           true,
		`repo:o/r`:                        true,
	} {
		terms, err := parseFilter(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := st.matches(it, terms, now); got != want {
			t.Errorf("%s: %v, want %v", q, got, want)
		}
	}
}

func TestGitHubCallsFilters(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.get("/repos/giantswarm/klaus/commits/a1/check-runs", "")
	f.s.MarkStep("later")
	f.get("/repos/giantswarm/klaus/commits/a1/statuses", "")
	f.get("/repos/giantswarm/klaus/pulls/55", "")
	f.req(http.MethodPost, "/app/hook/deliveries/1/attempts", nil, map[string]string{headerAuthorization: "Bearer " + f.jwt()})
	f.req(http.MethodDelete, "/repos/giantswarm/klaus", nil, map[string]string{headerAuthorization: "token " + f.mint()})
	for _, tc := range []struct {
		args map[string]any
		want map[string]int
	}{
		{nil, map[string]int{callsCount: 6, forge.APIREST: 4, "app": 2, callsMutating: 2}},
		{map[string]any{argSinceStep: "later"}, map[string]int{callsCount: 4}},
		{map[string]any{argReplica: "engine-1"}, map[string]int{callsCount: 6}},
		{map[string]any{argReplica: "engine-0"}, map[string]int{callsCount: 0}},
		{map[string]any{argPath: "/repos/giantswarm/klaus/commits/*"}, map[string]int{callsCount: 2}},
		{map[string]any{argPath: "/repos/giantswarm/klaus/commits/a1/*", argSinceStep: "later"}, map[string]int{callsCount: 1}},
		{map[string]any{argPath: "/repos/giantswarm/klau"}, map[string]int{callsCount: 0}},
		{map[string]any{argOperation: "checks/list-for-ref"}, map[string]int{callsCount: 1}},
		{map[string]any{argOperation: "repos/list-commit-statuses-for-ref"}, map[string]int{callsCount: 1}},
		{map[string]any{argMethod: "POST"}, map[string]int{callsCount: 2, callsMutating: 1}},
		{map[string]any{argMethod: "POST", argPath: "/app/hook/deliveries"}, map[string]int{callsCount: 1, callsMutating: 1}},
		{map[string]any{argAPI: forge.APIApp, argOperation: forge.OpCreateInstallToken}, map[string]int{callsCount: 1, callsMutating: 0}},
		{map[string]any{argCode: 404}, map[string]int{callsCount: 1}},
		{map[string]any{argAPI: forge.APIREST, argMethod: "GET"}, map[string]int{callsModified: 3}},
	} {
		got := f.calls(tc.args)
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%v: %s = %v, want %d (%v)", tc.args, k, got[k], v, got)
			}
		}
	}
	if _, err := f.s.Do(context.Background(), toolCalls, map[string]any{argSinceStep: "never-marked"}); err == nil {
		t.Error("unknown step: no error")
	}
	if _, err := f.s.Do(context.Background(), toolCalls, map[string]any{"bogus": 1}); err == nil {
		t.Error("unknown filter: no error")
	}
}

func TestDeliveryLogEndpoints(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for i := range 5 {
		f.do(toolStatus, map[string]any{argRepo: repoKlaus, argSHA: "a1", argContext: "c" + itoa64(int64(i)), argState: statusSuccess,
			argDeliver: map[bool]string{true: deliverLost, false: "delivered"}[i == 2]})
	}
	auth := map[string]string{headerAuthorization: "Bearer " + f.jwt()}
	r := f.req(http.MethodGet, "/app/hook/deliveries?per_page=2", nil, auth)
	var page []wire.HookDelivery
	_ = json.Unmarshal(r.body, &page)
	if r.code != 200 || len(page) != 2 || page[0].ID <= page[1].ID || !strings.Contains(r.header.Get("Link"), `rel="next"`) {
		t.Fatalf("first page: %d %+v %s", r.code, page, r.header.Get("Link"))
	}
	next := strings.TrimPrefix(strings.SplitN(r.header.Get("Link"), ">", 2)[0], "<"+f.s.URL())
	r = f.req(http.MethodGet, next, nil, auth)
	var page2 []wire.HookDelivery
	_ = json.Unmarshal(r.body, &page2)
	if len(page2) != 2 || page2[0].ID >= page[1].ID {
		t.Fatalf("second page: %+v", page2)
	}
	r = f.req(http.MethodGet, "/app/hook/deliveries?status=failure", nil, auth)
	var failed []wire.HookDelivery
	_ = json.Unmarshal(r.body, &failed)
	if len(failed) != 1 || failed[0].StatusCode != 0 {
		t.Fatalf("failures: %+v", failed)
	}
	r = f.req(http.MethodGet, "/app/hook/deliveries/"+itoa64(failed[0].ID), nil, auth)
	var d wire.HookDeliveryDetail
	_ = json.Unmarshal(r.body, &d)
	if r.code != 200 || d.GUID != failed[0].GUID || path(map[string]any{"p": jsonMap(d.Request.Payload)}, "p.context") != "c2" ||
		d.Request.Headers[wire.HeaderEvent] != wire.EventStatus {
		t.Fatalf("detail: %d %+v", r.code, d)
	}
}

func jsonMap(b []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func TestGraphQLCostParser(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		doc  string
		vars map[string]any
		want int
	}{
		{`{ viewer { login } }`, nil, 1},
		// 1 + 100×3 requests: one per item for each nested connection.
		{`query Q($n: Int = 100) { a: items(first: $n) { nodes { x(first: 10) { y } z(last: 5) { y } w(first: 1) { y } } } }`, nil, 3},
		{`query Q($n: Int = 100) { a: items(first: $n) { nodes { x(first: 10) { y } z(last: 5) { y } w(first: 1) { y } } } }`, map[string]any{"n": 50.0}, 2},
		{`query Q($n: Int = 100) { a: items(first: $n) { nodes { x(first: 10) { y } } } }`, map[string]any{"n": 20.0}, 1},
		{`query Q($s: Boolean!) { a: items(first: 100) @skip(if: $s) { nodes { x(first: 10) { y } y(first: 10) { y } } } }`, map[string]any{"s": true}, 1},
		{`query Q($s: Boolean!) { a: items(first: 100) @skip(if: $s) { nodes { x(first: 10) { y } y(first: 10) { y } } } }`, map[string]any{"s": false}, 2},
		// 1 + 50 + 50×50 requests.
		{`query Q { ...F } fragment F on Query { items(last: 50) { nodes { a(first: 50) { b(first: 4) { c } } } } }`, nil, 26},
	} {
		doc, err := parseGraphQL(tc.doc)
		if err != nil {
			t.Fatalf("%s: %v", tc.doc, err)
		}
		op, _ := doc.operation("")
		if got := doc.cost(op, tc.vars); got != tc.want {
			t.Errorf("%s: cost %d, want %d", tc.doc, got, tc.want)
		}
	}
	for _, bad := range []string{`{ a(`, `query { a { b }`, `"unterminated`, ``} {
		if _, err := parseGraphQL(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestToolsRejectBadArguments(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for tool, args := range map[string]map[string]any{
		toolIssueCreate:   {argRef: refRoadmap101}, // exists
		toolIssueEdit:     {argRef: refRoadmap101, "colour": "red"},
		toolBoardSet:      {argIssue: refRoadmap100, argStatus: optUpNext}, // not on the board
		toolSubIssue:      {argParent: refRoadmap90, argChild: refRoadmap101, argOp: "swap"},
		toolPRReview:      {argRef: refKlaus55, argBy: bob, argState: "dismissed"},
		toolPRMerge:       {argRef: refKlaus55},
		toolCheckRun:      {argRepo: repoKlaus, argSHA: "a1", argName: "x", argStatus: checkCompleted},
		toolStatus:        {argRepo: repoKlaus, argSHA: "a1", argContext: "x", argState: "green"},
		toolBoardArchive:  {argIssue: refRoadmap100},
		"github_nonsense": {},
	} {
		if _, err := f.s.Do(context.Background(), tool, args); err == nil {
			t.Errorf("%s %v: no error", tool, args)
		}
	}
	// Every tool the contract lists is known.
	for tool := range Tools {
		if _, err := f.s.Do(context.Background(), tool, map[string]any{"unknown-arg": 1}); err == nil || strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("%s: %v", tool, err)
		}
	}
}

func TestDeterministicNodeIDs(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	_, m := f.graphql("BoardItemsByNode", wire.BoardItemsByNodeQuery, map[string]any{varIDs: []any{itemRoadmap101, "nope"}})
	if path(m, "data.nodes.0.id") != itemRoadmap101 || path(m, "data.nodes.0.content.id") != "I_giantswarm/roadmap#101" ||
		path(m, "data.nodes.1") != nil || path(m, "errors.0.type") != wire.ErrorTypeNotFound {
		t.Fatalf("nodes: %v", m)
	}
	r := f.get("/repos/giantswarm/klaus/pulls/55", "")
	if path(jsonMap(r.body), "node_id") != "PR_giantswarm/klaus#55" {
		t.Fatalf("PR: %s", r.body)
	}
}

func signJWT(key *rsa.PrivateKey, iss string, now time.Time) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    iss,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(key)
}

func TestClientDocumentsAreQueries(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		forge.OpBoardSchema:       wire.BoardSchemaQuery,
		forge.OpBoardTeamItems:    wire.BoardTeamItemsQuery,
		forge.OpBoardItemsByNode:  wire.BoardItemsByNodeQuery,
		forge.OpBoardProbe:        wire.BoardProbeQuery,
		forge.OpPullClosingIssues: wire.PullClosingIssuesQuery,
	} {
		d, err := parseGraphQL(doc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, op := range d.ops {
			if op.typ != "query" || op.name != name {
				t.Errorf("%s: a %s named %q", name, op.typ, op.name)
			}
		}
	}
}

func TestTokenMintAsksForReadPermissionsOnly(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	mint := func(body []byte) resp {
		return f.req(http.MethodPost, "/app/installations/200/access_tokens", body, map[string]string{headerAuthorization: "Bearer " + f.jwt()})
	}
	if r := mint(readOnly()); r.code != http.StatusCreated {
		t.Fatalf("read-only mint: %d %s", r.code, r.body)
	}
	write := wire.ReadPermissions()
	write["issues"] = "write"
	writeBody, _ := json.Marshal(wire.InstallationTokenRequest{Permissions: write})
	for name, body := range map[string][]byte{
		"no body":        nil,
		"no permissions": []byte(`{}`),
		"a write":        writeBody,
		"admin":          []byte(`{"permissions":{"organization_projects":"admin"}}`),
	} {
		r := mint(body)
		if r.code != http.StatusUnprocessableEntity || !strings.Contains(string(r.body), "read permissions only") {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	// A refused mint is still token minting, not a mutating call.
	if c := f.calls(map[string]any{argOperation: forge.OpCreateInstallToken}); c["count"] != 5 || c["mutating"] != 0 {
		t.Fatalf("mints: %v", c)
	}
}

func TestProbeAnswersInBoardOrderAndPages(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for i := range 120 {
		f.do(toolIssueCreate, map[string]any{argRef: "giantswarm/roadmap#" + itoa64(int64(500+i)), argTitle: "item",
			argBoard: map[string]any{argStatus: "Backlog 📦", argKind: optStory, argTeam: optBumblebee}})
	}
	f.clock.Advance(time.Hour)
	// The oldest item on the board is the most recently updated one: GitHub still answers it where it
	// sits on the board, not first.
	f.do(toolBoardSet, map[string]any{argIssue: refRoadmap101, argStatus: optValidation})
	vars := map[string]any{varOrg: fixtureOrg, varNumber: 273, varQuery: `team:"Bumblebee🐝" updated:>=2026-11-16`, varFirst: 100, "after": nil}
	var ids []string
	var updated []string
	for page := 0; page < 3; page++ {
		_, m := f.graphql("BoardProbe", wire.BoardProbeQuery, vars)
		var d struct {
			Data wire.BoardProbeData `json:"data"`
		}
		b, _ := json.Marshal(m)
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		items := d.Data.Organization.ProjectV2.Items
		if items.TotalCount != 123 {
			t.Fatalf("totalCount: %d", items.TotalCount)
		}
		for _, n := range items.Nodes {
			ids = append(ids, n.ID)
			updated = append(updated, n.UpdatedAt.Format(time.RFC3339))
		}
		if !items.PageInfo.HasNextPage {
			break
		}
		vars["after"] = items.PageInfo.EndCursor
	}
	if len(ids) != 123 {
		t.Fatalf("paged %d items", len(ids))
	}
	// Seeded first: #90, #101, then happa#1 (redacted), then the 120 created ones.
	if ids[0] != "PVTI_giantswarm_273_giantswarm/roadmap#90" || ids[1] != itemRoadmap101 ||
		updated[1] != start.Add(time.Hour).Format(time.RFC3339) || ids[122] != "PVTI_giantswarm_273_giantswarm/roadmap#619" {
		t.Fatalf("order: %v ... %v", ids[:3], ids[120:])
	}
	if c := f.calls(map[string]any{argOperation: forge.OpBoardProbe}); c["count"] != 2 || c["graphqlPoints"] != 2 {
		t.Fatalf("probe pages: %v", c)
	}
}

func TestPullClosingIssues(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.do(toolPROpen, map[string]any{argRef: "giantswarm/klaus#56", argAuthor: alice, argHead: "b1",
		argCloses: []any{refRoadmap100, "giantswarm/happa#1"}})
	vars := func(repo string, n int) map[string]any {
		owner, name, _ := strings.Cut(repo, "/")
		return map[string]any{"owner": owner, argName: name, varNumber: n, varFirst: wire.ClosingIssuesPageSize, "after": nil}
	}
	_, m := f.graphql("PullClosingIssues", wire.PullClosingIssuesQuery, vars(repoKlaus, 56))
	if path(m, "errors") != nil || path(m, "data.repository.pullRequest.closingIssuesReferences.totalCount") != 2.0 ||
		path(m, "data.repository.pullRequest.closingIssuesReferences.nodes.0.number") != 100.0 ||
		path(m, "data.repository.pullRequest.closingIssuesReferences.nodes.1.repository.nameWithOwner") != repoHappa {
		t.Fatalf("closing issues: %v", m)
	}
	if c := f.calls(map[string]any{argOperation: forge.OpPullClosingIssues}); c["graphqlPoints"] != 1 {
		t.Fatalf("cost: %v", c)
	}
	_, m = f.graphql("PullClosingIssues", wire.PullClosingIssuesQuery, vars(repoKlaus, 999))
	if path(m, "data.repository.pullRequest") != nil || path(m, "errors.0.type") != wire.ErrorTypeNotFound {
		t.Fatalf("no such PR: %v", m)
	}
	_, m = f.graphql("PullClosingIssues", wire.PullClosingIssuesQuery, vars(repoHappa, 7))
	if path(m, "data.repository") != nil || path(m, "errors.0.type") != wire.ErrorTypeNotFound {
		t.Fatalf("private repo outside the installation: %v", m)
	}
}

func TestLinkSummariesCountEveryLink(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for i := range 25 {
		ref := "giantswarm/roadmap#" + itoa64(int64(700+i))
		f.do(toolIssueCreate, map[string]any{argRef: ref, argTitle: "sub"})
		f.do(toolSubIssue, map[string]any{argParent: refRoadmap90, argChild: ref, argOp: opAdd})
	}
	f.do(toolIssueEdit, map[string]any{argRef: "giantswarm/roadmap#700", argState: stateClosed})
	f.do(toolDependency, map[string]any{argIssue: refRoadmap90, argBlockedBy: refRoadmap100, argOp: opAdd})
	_, m := f.graphql("BoardItemsByNode", wire.BoardItemsByNodeQuery, map[string]any{varIDs: []any{"PVTI_giantswarm_273_giantswarm/roadmap#90"}})
	ct := "data.nodes.0.content."
	if n, _ := path(m, ct+"subIssues.nodes").([]any); len(n) != 20 {
		t.Fatalf("sub-issues listed: %d", len(n))
	}
	if path(m, ct+"subIssuesSummary.total") != 25.0 || path(m, ct+"subIssuesSummary.completed") != 1.0 ||
		path(m, ct+"issueDependenciesSummary.totalBlockedBy") != 1.0 || path(m, ct+"issueDependenciesSummary.blockedBy") != 1.0 {
		t.Fatalf("summaries: %v", path(m, "data.nodes.0.content"))
	}
}

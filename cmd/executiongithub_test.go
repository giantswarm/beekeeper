//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	workerGitHubKeyRef          = "op://Shared/worker-app/private-key"
	workerGitHubCompleted       = "completed"
	workerGitHubPrivateRepo     = "example/private"
	workerGitHubWrongGeneration = "wrong generation"
)

const workerTokenFixture = "fixture-worker-installation-token"

func workerGitHubTestApp(t *testing.T) (*app, execution.StartRequest) {
	t.Helper()
	a, r := executionTestApp(t)
	r.GitHub = true
	a.cfg.Identity.Host = r.Coordinator
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg, fixtureOrg}
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{AppID: fixtureAppID, PrivateKey: workerGitHubKeyRef, Installations: map[string]int64{defaultOrg: 42, fixtureOrg: 43}}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, r
}

func TestWorkerGitHubScopedIssuanceAndRevocation(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	sa, tools, _ := secretApp(t)
	a.cfg.Secret = sa.cfg.Secret
	_, pem := fixtureKey()
	ref := workerGitHubKeyRef
	tools.Vault[ref] = pem
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{AppID: fixtureAppID, PrivateKey: ref, Installations: map[string]int64{defaultOrg: 42}}
	issued, revoked := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/app/installations/42/access_tokens":
			if !(&fakeGitHub{}).appToken(req.Header.Get("Authorization")) {
				t.Error("request not signed by coordinator App")
			}
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if json.NewDecoder(req.Body).Decode(&body) != nil || len(body.Repositories) != 1 || body.Repositories[0] != pluginName || body.Permissions["contents"] != workerGitHubWrite || body.Permissions["checks"] != workerGitHubRead || body.Permissions["actions"] != workerGitHubRead || body.Permissions["administration"] != "" {
				t.Error("token scope widened or required read permissions absent")
			}
			issued++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(workerGitHubToken{Token: workerTokenFixture, Expires: time.Now().Add(time.Hour)})
		case workerGitHubRevokePath:
			if req.Method != http.MethodDelete || req.Header.Get("Authorization") != "Bearer "+workerTokenFixture {
				t.Error("wrong revocation")
			}
			revoked++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	previous := githubAPI
	githubAPI = server.URL
	t.Cleanup(func() { githubAPI = previous })
	var token workerGitHubToken
	request := workerGitHubRequest{Repository: r.Repository, Write: true}
	for range 2 {
		if err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &token, false); err != nil {
			t.Fatal(err)
		}
	}
	if issued != 1 || token.Token != workerTokenFixture {
		t.Fatal("credential not issued/cached per worker scope")
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), workerTokenFixture) {
		t.Fatal("token leaked into scheduling state")
	}
	info, err := os.Stat(a.workerGitHubPath(r.Ref))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("credential ledger is not private")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts[0].Execution.Completed = true; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &token, false); err == nil {
		t.Fatal("completed worker received token")
	}
	var result struct{}
	if err := coordinatorTestCall(t, a, r, workerGitHubRevoke, nil, &result, false); err != nil {
		t.Fatal(err)
	}
	if issued != 1 || revoked != 1 {
		t.Fatalf("issued %d revoked %d", issued, revoked)
	}
	tokens, err := readWorkerGitHub(a.workerGitHubPath(r.Ref))
	if err != nil || len(tokens) != 0 {
		t.Fatal("revoked credential retained")
	}
}

func TestWorkerGitHubRejectsAuthorityAndScopeChanges(t *testing.T) {
	for _, change := range []string{"wrong key", workerGitHubWrongGeneration, bgStopCmd, workerGitHubCompleted, "outside owner", "other write", deviceFlowDisabled} {
		t.Run(change, func(t *testing.T) {
			a, r := workerGitHubTestApp(t)
			request := workerGitHubRequest{Repository: r.Repository, Write: true}
			switch change {
			case "wrong key":
				r.ControlKey, _ = execution.NewControlKey()
			case workerGitHubWrongGeneration:
				r.Ref.Generation++
			case "outside owner":
				request.Repository = "outside/repo"
			case "other write":
				request.Repository = "example/other"
			default:
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					switch change {
					case bgStopCmd:
						st.Stop = &state.Stop{Reason: remoteMCPFixture}
					case workerGitHubCompleted:
						st.Starts[0].Execution.Completed = true
					case deviceFlowDisabled:
						st.Starts[0].Execution.Request.GitHub = false
					}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			// A populated ledger must not bypass current authority checks.
			path := a.workerGitHubPath(r.Ref)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeWorkerGitHub(path, map[string]workerGitHubToken{request.Repository + "/write": {Token: workerTokenFixture, Expires: time.Now().Add(time.Hour)}}); err != nil {
				t.Fatal(err)
			}
			var token workerGitHubToken
			if err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &token, false); err == nil {
				t.Fatal("unauthorized cached token issued")
			}
		})
	}
}

func TestWorkerGitHubFailedRevocationRetainsLedger(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	path := a.workerGitHubPath(r.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerGitHub(path, map[string]workerGitHubToken{remoteMCPFixture: {Token: workerTokenFixture, Expires: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	previous := githubAPI
	githubAPI = server.URL
	t.Cleanup(func() { githubAPI = previous })
	if err := a.revokeWorkerGitHub(context.Background(), r.Ref); err == nil {
		t.Fatal("failed revocation reported success")
	}
	tokens, err := readWorkerGitHub(path)
	if err != nil || len(tokens) != 1 {
		t.Fatal("failed revocation lost credential")
	}
}

func TestWorkerGitHubStartRequiresHostAdapter(t *testing.T) {
	a, brief := sshStartTestApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{AppID: 42, PrivateKey: workerGitHubKeyRef, Installations: map[string]int64{defaultOrg: 42}}
	err := a.startSSHExecution(context.Background(), "fixture worker", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "GitHub adapter") {
		t.Fatalf("old host not refused before reservation: %v", err)
	}
}

func TestWorkerGitHubExpiredLeaseRevokesWithoutHostContact(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec := st.Starts[0].Execution
		rec.Request.LeaseRequired = true
		rec.Lease = &execution.LeaseGrant{ExpiresAt: time.Now().Add(-time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	path := a.workerGitHubPath(r.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerGitHub(path, map[string]workerGitHubToken{r.Repository + workerGitHubReadSuffix: {Token: workerTokenFixture, Expires: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	revoked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodDelete || req.URL.Path != workerGitHubRevokePath {
			t.Error("unexpected revocation request")
		}
		revoked++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	previous := githubAPI
	githubAPI = server.URL
	t.Cleanup(func() { githubAPI = previous })
	var token workerGitHubToken
	if err := coordinatorTestCall(t, a, r, workerGitHubIssue, workerGitHubRequest{Repository: r.Repository}, &token, false); err == nil {
		t.Fatal("expired execution lease obtained cached token")
	}
	a.executionFailure(r.Ref, errors.New("fixture host unreachable"))
	if revoked != 1 {
		t.Fatal("unreachable expired worker retained its credential")
	}
	tokens, err := readWorkerGitHub(path)
	if err != nil || len(tokens) != 0 {
		t.Fatal("revoked expired-worker credential retained")
	}
}

func TestWorkerGitHubRevocationFailureStillStopsHost(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	path := a.workerGitHubPath(r.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerGitHub(path, map[string]workerGitHubToken{r.Repository + workerGitHubReadSuffix: {Token: workerTokenFixture, Expires: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	previous := githubAPI
	githubAPI = server.URL
	t.Cleanup(func() { githubAPI = previous })
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	handle := st.Starts[0].Execution.Handle
	stopped := false
	fake := &testExecutionTransport{
		stop: func(_ context.Context, _ execution.Ref, q execution.StopRequest) (execution.Ack, error) {
			stopped = true
			return execution.Ack{ID: q.ID, Stage: "applied"}, nil
		},
		inspect: func(context.Context, execution.Ref) (execution.Observation, error) {
			return execution.Observation{Handle: handle, ProcessState: execution.Exited}, nil
		},
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	if err := a.stopExecution(context.Background(), r.Ref, "stop fixture execution"); err == nil {
		t.Fatal("failed revocation was not reported")
	}
	if !stopped {
		t.Fatal("GitHub outage prevented host termination")
	}
	tokens, err := readWorkerGitHub(path)
	if err != nil || len(tokens) != 1 {
		t.Fatal("failed revocation lost its retry ledger")
	}
}

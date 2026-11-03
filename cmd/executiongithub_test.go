//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	workerGitHubClientIDFixture = "Iv23liFixtureClientID"
	workerGitHubRefreshRef      = "op://Shared/worker-app/password"
	workerRefreshTokenFixture   = "ghr_fixture_refresh_initial" //nolint:gosec // synthetic test fixture
	workerGitHubCompleted       = "completed"
	workerGitHubPrivateRepo     = "example/private"
	workerGitHubWrongGeneration = "wrong generation"
)

const workerTokenFixture = "fixture-worker-user-token" //nolint:gosec // synthetic test fixture

func workerGitHubTestApp(t *testing.T) (*app, execution.StartRequest) {
	t.Helper()
	a, r := executionTestApp(t)
	sa, tools, _ := secretApp(t)
	a.cfg.Secret = sa.cfg.Secret
	tools.Vault[workerGitHubRefreshRef] = workerRefreshTokenFixture
	r.GitHub = true
	a.cfg.Identity.Host = r.Coordinator
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg, fixtureOrg}
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{ClientID: workerGitHubClientIDFixture, RefreshToken: workerGitHubRefreshRef}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, r
}

func writeTestOAuthToken(w http.ResponseWriter, access, refresh string) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:gosec // synthetic OAuth response in unit test server
		"access_token":                access,
		"token_type":                  "bearer",
		"expires_in":                  28800,
		workerGitHubRefreshTokenField: refresh,
	})
}

func TestWorkerGitHubScopedIssuanceAndRevocation(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	rotations := 0
	wantRefresh := workerRefreshTokenFixture
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != workerGitHubOAuthTokenPath {
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if req.Form.Get(workerGitHubClientIDField) != workerGitHubClientIDFixture || req.Form.Get("grant_type") != workerGitHubRefreshTokenField || req.Form.Get(workerGitHubRefreshTokenField) != wantRefresh {
			t.Errorf("unexpected refresh form: %v (want refresh_token %s)", req.Form, wantRefresh)
		}
		rotations++
		wantRefresh = "ghr_fixture_refresh_" + strconv.Itoa(rotations)
		tok := workerTokenFixture
		if rotations > 1 {
			tok = workerTokenFixture + "-" + strconv.Itoa(rotations)
		}
		writeTestOAuthToken(w, tok, wantRefresh)
	}))
	defer server.Close()
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = server.URL, server.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })
	var token workerGitHubToken
	for _, request := range []workerGitHubRequest{
		{Repository: r.Repository, Write: true},
		{Repository: r.Repository, Write: true},
		{Repository: "example/other", Write: true},
		{Repository: "outside/readable", Write: false},
	} {
		if err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &token, false); err != nil {
			t.Fatalf("%+v: %v", request, err)
		}
	}
	if rotations != 1 || token.Token != workerTokenFixture {
		t.Fatalf("credential not issued/cached across repositories: rotations=%d token=%q", rotations, token.Token)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), workerTokenFixture) || strings.Contains(string(raw), wantRefresh) {
		t.Fatal("token leaked into scheduling state")
	}
	info, err := os.Stat(a.workerGitHubPath(r.Ref))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("credential ledger is not private")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts[0].Execution.Completed = true; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, r, workerGitHubIssue, workerGitHubRequest{Repository: r.Repository, Write: true}, &token, false); err == nil {
		t.Fatal("completed worker received token")
	}
	var result struct{}
	if err := coordinatorTestCall(t, a, r, workerGitHubRevoke, nil, &result, false); err != nil {
		t.Fatal(err)
	}
	if rotations != 2 {
		t.Fatalf("revocation did not rotate refresh token: rotations=%d", rotations)
	}
	tokens, err := readWorkerGitHub(a.workerGitHubPath(r.Ref))
	if err != nil || len(tokens) != 0 {
		t.Fatal("revoked credential retained")
	}
}

func TestWorkerGitHubDeviceFlowLoginAndSOPSPersistence(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	sopsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sopsDir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sopsFile := filepath.Join(sopsDir, "worker-github.sops.yaml")
	if err := os.WriteFile(sopsFile, secrettest.Encrypt("refresh_token: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sopsRef := "sops://" + sopsFile + "#refresh_token"
	a.cfg.Secret.Files = []string{sopsFile}
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{ClientID: workerGitHubClientIDFixture, RefreshToken: sopsRef}

	prevSleep := workerGitHubPollSleep
	workerGitHubPollSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { workerGitHubPollSleep = prevSleep })

	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case workerGitHubDeviceCodePath:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-code-1",
				"user_code":        "ABCD-1234",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       900,
				"interval":         1,
			})
		case workerGitHubOAuthTokenPath:
			_ = req.ParseForm()
			if req.Form.Get("grant_type") == workerGitHubDeviceGrant {
				polls++
				if polls == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
					return
				}
				writeTestOAuthToken(w, workerTokenFixture, "ghr_from_device_flow")
				return
			}
			if req.Form.Get(workerGitHubRefreshTokenField) != "ghr_from_device_flow" {
				t.Errorf("unexpected SOPS refresh token: %q", req.Form.Get(workerGitHubRefreshTokenField))
			}
			writeTestOAuthToken(w, workerTokenFixture+"-rotated", "ghr_rotated_sops")
		case "/user":
			if req.Header.Get("Authorization") != "Bearer "+workerTokenFixture {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "fixture-operator"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = server.URL, server.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })

	var out bytes.Buffer
	if err := a.authorizeWorkerGitHub(context.Background(), "", "", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ABCD-1234") || !strings.Contains(out.String(), "authorized fixture-operator") || strings.Contains(out.String(), "ghr_") || strings.Contains(out.String(), workerTokenFixture) {
		t.Fatalf("unexpected login output: %s", out.String())
	}
	var token workerGitHubToken
	if err := coordinatorTestCall(t, a, r, workerGitHubIssue, workerGitHubRequest{Repository: r.Repository, Write: true}, &token, false); err != nil {
		t.Fatal(err)
	}
	if token.Token != workerTokenFixture {
		t.Fatalf("got %q, want %q", token.Token, workerTokenFixture)
	}
	if err := a.revokeWorkerGitHub(context.Background(), r.Ref); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerGitHubRejectsAuthorityAndScopeChanges(t *testing.T) {
	for _, change := range []string{"wrong key", workerGitHubWrongGeneration, bgStopCmd, workerGitHubCompleted, "outside owner", deviceFlowDisabled} {
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
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = server.URL, server.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })
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
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{ClientID: workerGitHubClientIDFixture, RefreshToken: workerGitHubRefreshRef}
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
		if req.Method != http.MethodPost || req.URL.Path != workerGitHubOAuthTokenPath {
			t.Error("unexpected revocation request")
		}
		revoked++
		writeTestOAuthToken(w, workerTokenFixture+"-rotated", "ghr_rotated_after_lease")
	}))
	defer server.Close()
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = server.URL, server.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })
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
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = server.URL, server.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })
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

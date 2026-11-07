//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
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
		testExpiresIn:                 28800,
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
	if err := os.WriteFile(filepath.Join(sopsDir, ".sops.yaml"), []byte("creation_rules:\n  - path_regex: .*\n"), 0o600); err != nil {
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
				testExpiresIn:      900,
				intervalKey:        1,
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

	// A competing issuer or revoker must not enter while login replaces
	// the refresh token and the matching coordinator cache.
	if err := os.MkdirAll(filepath.Dir(a.workerGitHubOAuthPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	persisted := false
	previousRun := secretRun
	t.Cleanup(func() { secretRun = previousRun })
	secretRun = func(ctx context.Context, dir string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
		if name == "sops" && slices.Contains(args, "encrypt") {
			persisted = true
			lock := flock.New(a.workerGitHubOAuthPath() + ".lock")
			locked, err := lock.TryLock()
			if err != nil {
				t.Fatal(err)
			}
			if locked {
				_ = lock.Unlock()
				t.Error("login persisted refresh token without the shared OAuth lock")
			}
		}
		return previousRun(ctx, dir, env, in, name, args...)
	}

	var out bytes.Buffer
	if err := a.authorizeWorkerGitHub(context.Background(), "", "", &out); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("login did not persist refresh token")
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

// Both executions are live together and GitHub invalidates old access tokens
// on every refresh, matching the provider rather than just counting callbacks.
func TestWorkerGitHubConcurrentExecutionCompletion(t *testing.T) {
	a, first := workerGitHubTestApp(t)
	second := first
	second.Ref.Generation++
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		record := *st.Starts[0].Execution
		record.Request = second
		record.Handle.Ref = second.Ref
		record.RequestDigest = second.Digest()
		st.Starts = append(st.Starts, state.Start{Execution: &record})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	rotations := 0
	current := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == workerGitHubOAuthTokenPath {
			rotations++
			current = workerTokenFixture + strconv.Itoa(rotations)
			writeTestOAuthToken(w, current, "ghr_next_fixture")
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+current {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": remoteMCPFixture})
	}))
	defer server.Close()
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = server.URL, server.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })
	request := workerGitHubRequest{Repository: first.Repository, Write: true}
	var issued workerGitHubToken
	for _, r := range []execution.StartRequest{first, second} {
		if err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &issued, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Completed = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.revokeWorkerGitHub(context.Background(), first.Ref); err != nil {
		t.Fatal(err)
	}
	if rotations != 1 {
		t.Fatal("completion invalidated another live execution's credential")
	}
	if err := coordinatorTestCall(t, a, second, workerGitHubIssue, request, &issued, false); err != nil {
		t.Fatal(err)
	}
	var user struct{ Login string }
	if err := githubJSON(context.Background(), http.MethodGet, "/user", "Bearer "+issued.Token, http.StatusOK, &user); err != nil {
		t.Fatal(err)
	}
	// Simulate refresh by another request. The second execution's ledger still
	// has eight hours left but must no longer hand out its invalidated token.
	if _, err := a.refreshWorkerGitHubToken(context.Background(), a.workerGitHubOAuthPath()); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, second, workerGitHubIssue, request, &issued, false); err != nil {
		t.Fatal(err)
	}
	if err := githubJSON(context.Background(), http.MethodGet, "/user", "Bearer "+issued.Token, http.StatusOK, &user); err != nil {
		t.Fatal(err)
	}
	// A third live execution has never fetched a token. It must still
	// trigger deferred revocation when it is the last one to finish.
	third := second
	third.Ref.Generation++
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		record := *st.Starts[1].Execution
		record.Request = third
		record.Handle.Ref = third.Ref
		record.RequestDigest = third.Digest()
		st.Starts = append(st.Starts, state.Start{Execution: &record})
		st.Starts[1].Execution.Completed = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.revokeWorkerGitHub(context.Background(), second.Ref); err != nil {
		t.Fatal(err)
	}
	if rotations != 2 {
		t.Fatal("rotation while an execution is still live")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[2].Execution.Completed = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.revokeWorkerGitHub(context.Background(), third.Ref); err != nil {
			t.Fatal(err)
		}
	}
	if rotations != 3 {
		t.Fatalf("last execution did not revoke shared authority: %d", rotations)
	}
}

func TestWorkerGitHubSourceRefsRejectedBeforeNetwork(t *testing.T) {
	a, _ := workerGitHubTestApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("read-only reference reached GitHub")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	prevWeb := githubWeb
	githubWeb = server.URL
	t.Cleanup(func() { githubWeb = prevWeb })
	for _, ref := range []string{"store://worker", "file:///tmp/worker-token", "cmd://producer#token", "k8s://context/namespace/secret/name#token"} {
		a.cfg.GitHub.WorkerApp.RefreshToken = ref
		if err := a.authorizeWorkerGitHub(context.Background(), "", "", &bytes.Buffer{}); err == nil {
			t.Fatal("source accepted for authorization")
		}
		if _, err := a.refreshWorkerGitHubToken(context.Background(), ""); err == nil {
			t.Fatal("source accepted for refresh")
		}
	}
}

func TestWorkerGitHubEmptyAllowlistDeniesStarts(t *testing.T) {
	a, brief := sshStartTestApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{}
	a.cfg.GitHub.WorkerApp = &config.WorkerGitHub{ClientID: workerGitHubClientIDFixture, RefreshToken: workerGitHubRefreshRef}
	err := a.startSSHExecution(context.Background(), "fixture worker", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "github.allowedOrgs") {
		t.Fatalf("empty allowlist permitted start: %v", err)
	}
}

func TestWorkerGitHubEmptyAllowlistDeniesWrites(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{}
	if err := a.workerGitHubLive(r.Ref, workerGitHubRequest{Repository: r.Repository, Write: true}); err == nil {
		t.Fatal("empty allowlist permitted writes")
	}
	if err := a.workerGitHubLive(r.Ref, workerGitHubRequest{Repository: r.Repository}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerGitHubRefreshPersistenceFailureExplainsRecovery(t *testing.T) {
	a, _ := workerGitHubTestApp(t)
	previousRun := secretRun
	secretRun = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "op" && len(args) > 1 && args[0] == "item" && (args[1] == "edit" || args[1] == "create") {
			return nil, errors.New("fixture vault locked")
		}
		return previousRun(ctx, dir, env, stdin, name, args...)
	}
	t.Cleanup(func() { secretRun = previousRun })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestOAuthToken(w, workerTokenFixture, "ghr_rotated_fixture")
	}))
	defer server.Close()
	prevWeb := githubWeb
	githubWeb = server.URL
	t.Cleanup(func() { githubWeb = prevWeb })
	_, err := a.refreshWorkerGitHubToken(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "authorization is lost") || !strings.Contains(err.Error(), "beekeeper agents hosts github-login") {
		t.Fatalf("missing persistence recovery instructions: %v", err)
	}
}

func TestReview3325DirectPushAssignment(t *testing.T) {
	for _, scenario := range []string{review3325Authorized, review3325SourceOnly, review3325DefaultTrunk, claudeLoginExpired, review3325WrongRepo, review3325WrongBranch, review3325WrongSession, review3325Superseded, workerGitHubCompleted} {
		t.Run(scenario, func(t *testing.T) {
			a, r := workerGitHubTestApp(t)
			const branch = "fixture/foreign-source"
			originalRead := readLoopPull
			t.Cleanup(func() { readLoopPull = originalRead })
			readLoopPull = func(context.Context, string) (superloop.Pull, error) {
				return superloop.Pull{Ref: r.Repository + "#7", State: superloop.Open, Branch: branch}, nil
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				start := &st.Starts[0]
				start.Execution.Request.GitRef = branch
				start.Execution.Handle.Commit = strings.Repeat("a", 40)
				start.Execution.Lease = &execution.LeaseGrant{ExpiresAt: time.Now().Add(time.Hour)}
				round := state.DeskReview{ID: "assigned-push", Ref: r.Repository + "#7", Head: start.Execution.Handle.Commit, Round: 1, Reviewer: start.Party, AssignedAt: time.Now()}
				switch scenario {
				case review3325DefaultTrunk:
					start.Execution.Request.GitRef = "trunk"
				case claudeLoginExpired:
					start.Execution.Lease.ExpiresAt = time.Now().Add(-time.Second)
				case review3325WrongBranch:
					start.Execution.Request.GitRef = "other/branch"
				case review3325WrongSession:
					round.Reviewer = state.Party{Session: "other-session"}
				case workerGitHubCompleted:
					round.Result = &state.DeskReviewResult{}
				}
				if scenario != review3325SourceOnly && scenario != review3325DefaultTrunk {
					st.DeskReviews = append(st.DeskReviews, round)
				}
				if scenario == review3325Superseded {
					next := round
					next.ID = "next-round"
					next.Round++
					next.Reviewer = state.Party{Session: "next-session"}
					st.DeskReviews = append(st.DeskReviews, next)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			request := workerGitHubRequest{Repository: r.Repository, Write: true, ResolvePush: true}
			if scenario == review3325WrongRepo {
				request.Repository = "example/private"
			}
			var target workerGitHubPushTarget
			err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &target, false)
			switch scenario {
			case review3325Authorized:
				if err != nil || target.Branch != branch || target.ReviewID == "" {
					t.Fatalf("assigned branch refused: %+v %v", target, err)
				}
			case review3325SourceOnly, review3325DefaultTrunk, review3325WrongSession:
				if err == nil && target.Branch != execution.GitHubBranch(r.Ref) {
					t.Fatalf("source or name granted foreign write: %+v", target)
				}
			default:
				if err == nil {
					t.Fatalf("invalid assignment admitted: %+v", target)
				}
			}
			// Revalidation on credential use must reject a branch other than the
			// resolved target, before any credential mint or network write.
			if scenario == review3325Authorized {
				if err := a.workerGitHubLive(r.Ref, workerGitHubRequest{Repository: r.Repository, Write: true, PushBranch: branch, PushReview: target.ReviewID}); err != nil {
					t.Fatalf("current round push refused: %v", err)
				}
				if err := a.workerGitHubLive(r.Ref, workerGitHubRequest{Repository: r.Repository, Write: true, PushBranch: branch, PushReview: "different-round"}); err == nil {
					t.Fatal("another round borrowed the branch grant")
				}
			}
			request.ResolvePush = false
			request.PushBranch = "unrelated/branch"
			var ignored workerGitHubToken
			if err := coordinatorTestCall(t, a, r, workerGitHubIssue, request, &ignored, false); err == nil {
				t.Fatal("mismatched branch reached credential issuance")
			}
		})
	}
}

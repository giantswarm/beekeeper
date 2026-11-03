package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/secret"
)

// Only these responses, over the authenticated SSH callback, carry an App
// installation token. They never enter journals, control receipts or logs.
type workerGitHubToken struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires_at"`
}

type workerGitHubRequest struct {
	Repository string `json:"repository"`
	Write      bool   `json:"write"`
}

const (
	workerGitHubConfigOption = "--config"
	workerGitHubSearch       = "search"
	workerGitHubRead         = "read"
	workerGitHubWrite        = "write"
	workerGitHubCreate       = "create"
	workerGitHubPush         = "push"
	workerGitHubOrigin       = "origin"
)

const workerGitHubRevokePath = "/installation/token"
const workerGitHubReadSuffix = "/read"

const workerGitHubIssue = "github-token"
const workerGitHubRevoke = "github-revoke"
const workerGitHubBudget = "github-budget"

type workerBudgetRequest struct {
	Gate    bool     `json:"gate"`
	JSON    bool     `json:"json"`
	Buckets []string `json:"buckets,omitempty"`
}
type workerBudgetResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
}

func (a *app) workerGitHubPath(ref execution.Ref) string {
	return filepath.Join(a.cfg.StateDir, "execution-github", string(ref.Host)+"-"+string(ref.Execution)+"-"+strconv.FormatUint(ref.Generation, 10)+".json")
}

func (a *app) workerGitHubLive(ref execution.Ref, request workerGitHubRequest) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	r, _ := executionRecord(st, ref)
	if r == nil || !r.Request.GitHub || r.Request.Coordinator != a.cfg.Identity.Host || r.StopRequest != nil || r.Terminal() || r.Completed || st.Stop != nil {
		return errors.New("worker GitHub access revoked")
	}
	if r.Request.LeaseRequired && (r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now())) {
		return errors.New("worker GitHub execution lease expired")
	}
	h, ok := a.cfg.Agents.Hosts[r.Selector]
	if !ok || h.Identity != ref.Host {
		return errors.New("worker host is no longer configured")
	}
	for _, hold := range st.Holds {
		if hold.Target == holdGitHub && hold.Active(time.Now()) {
			return errors.New("worker GitHub access held")
		}
	}
	owner, _, ok := strings.Cut(request.Repository, "/")
	if !ok || execution.RepositoryIdentity("https://github.com/"+request.Repository) != request.Repository || !slices.Contains(a.cfg.GitHub.AllowedOrgs, owner) {
		return errors.New("worker GitHub repository is outside configured owners")
	}
	if a.cfg.GitHub.WorkerApp == nil || a.cfg.GitHub.WorkerApp.Installations[owner] <= 0 {
		return errors.New("worker GitHub App installation is no longer configured")
	}
	if request.Write && !strings.EqualFold(request.Repository, r.Request.Repository) {
		return errors.New("worker may write only its task repository")
	}
	return nil
}

func (a *app) executionGitHub(ctx context.Context, host string, req execution.CallbackRequest) (any, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	r, _ := executionRecord(st, req.Ref)
	if r == nil || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
		return nil, errors.New("unauthenticated execution callback")
	}
	if req.Operation == workerGitHubBudget {
		if err := a.workerGitHubLive(req.Ref, workerGitHubRequest{Repository: strings.ToLower(r.Request.Repository)}); err != nil {
			return nil, err
		}
		var request workerBudgetRequest
		if execution.Decode(req.Payload, &request) != nil {
			return nil, errors.New("invalid worker budget request")
		}
		copyApp := *a
		var output bytes.Buffer
		copyApp.out = &output
		copyApp.json = request.JSON
		command := copyApp.budgetCmd()
		args := []string{}
		if request.Gate {
			args = append(args, "--gate")
		}
		for _, bucket := range request.Buckets {
			args = append(args, "--bucket", bucket)
		}
		command.SetArgs(args)
		command.SilenceUsage, command.SilenceErrors = true, true
		command.SetOut(&output)
		command.SetErr(&output)
		result := workerBudgetResult{}
		if err := command.ExecuteContext(ctx); err != nil {
			result.Code, result.Error = Code(err), err.Error()
		}
		result.Output = output.String()
		return result, nil
	}
	if req.Operation == workerGitHubRevoke {
		return struct{}{}, a.revokeWorkerGitHub(ctx, req.Ref)
	}
	var request workerGitHubRequest
	if req.Operation != workerGitHubIssue || execution.Decode(req.Payload, &request) != nil {
		return nil, errors.New("invalid worker GitHub request")
	}
	// Serialize issuance and revocation independently of the scheduling store;
	// GitHub and vault I/O never hold its lock.
	path := a.workerGitHubPath(req.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	if ok, err := lock.TryLockContext(ctx, 20*time.Millisecond); err != nil || !ok {
		return nil, errors.New("worker GitHub credentials busy")
	}
	defer func() { _ = lock.Unlock() }()
	if err := a.workerGitHubLive(req.Ref, request); err != nil {
		return nil, err
	}
	tokens, err := readWorkerGitHub(path)
	if err != nil {
		return nil, err
	}
	key := request.Repository + workerGitHubReadSuffix
	if request.Write {
		key = request.Repository + "/write"
	}
	if token, ok := tokens[key]; ok && time.Until(token.Expires) > 5*time.Minute {
		return token, nil
	}
	token, err := a.mintWorkerGitHub(ctx, request)
	if err != nil {
		return nil, err
	}
	// Keep old tokens until revoked: replacing a token must not lose authority
	// to revoke credentials a command may still be using.
	tokens[key+"/"+strconv.FormatInt(time.Now().UnixNano(), 10)] = token
	tokens[key] = token
	if err := writeWorkerGitHub(path, tokens); err != nil {
		_ = revokeWorkerGitHubToken(ctx, token.Token)
		return nil, err
	}
	// A stop may have been recorded while the vault or GitHub answered.
	if err := a.workerGitHubLive(req.Ref, request); err != nil {
		_ = revokeWorkerGitHubToken(ctx, token.Token)
		return nil, err
	}
	return token, nil
}

func readWorkerGitHub(path string) (map[string]workerGitHubToken, error) {
	tokens := map[string]workerGitHubToken{}
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return tokens, nil
	}
	if err != nil {
		return nil, errors.New("cannot read worker GitHub credential ledger")
	}
	if json.Unmarshal(b, &tokens) != nil {
		return nil, errors.New("invalid worker GitHub credential ledger")
	}
	return tokens, nil
}

func writeWorkerGitHub(path string, tokens map[string]workerGitHubToken) error {
	b, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".github-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (a *app) revokeWorkerGitHub(ctx context.Context, ref execution.Ref) error {
	path := a.workerGitHubPath(ref)
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	lock := flock.New(path + ".lock")
	if ok, err := lock.TryLockContext(ctx, 20*time.Millisecond); err != nil || !ok {
		return errors.New("worker GitHub credentials busy during revocation")
	}
	defer func() { _ = lock.Unlock() }()
	tokens, err := readWorkerGitHub(path)
	if err != nil {
		return err
	}
	remaining := map[string]workerGitHubToken{}
	// Values may have two ledger keys. Deduplicate in memory only, never log.
	seen := map[string]error{}
	for key, token := range tokens {
		if !time.Now().Before(token.Expires) {
			continue
		}
		err, ok := seen[token.Token]
		if !ok {
			err = revokeWorkerGitHubToken(ctx, token.Token)
			seen[token.Token] = err
		}
		if err != nil {
			remaining[key] = token
		}
	}
	if len(tokens) == 0 {
		return nil
	}
	if err := writeWorkerGitHub(path, remaining); err != nil {
		return err
	}
	if len(remaining) > 0 {
		return errors.New("worker GitHub revocation failed; ledger retained for retry")
	}
	return nil
}

func revokeWorkerGitHubToken(ctx context.Context, token string) error {
	resp, err := githubRequest(ctx, http.MethodDelete, workerGitHubRevokePath, "Bearer "+token)
	if err != nil {
		return errors.New("worker GitHub revocation transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("worker GitHub revocation: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *app) mintWorkerGitHub(ctx context.Context, request workerGitHubRequest) (workerGitHubToken, error) {
	var token workerGitHubToken
	app := a.cfg.GitHub.WorkerApp
	owner, repo, _ := strings.Cut(request.Repository, "/")
	if app == nil || app.AppID <= 0 || app.Installations[owner] <= 0 {
		return token, errors.New("configure github.workerApp and its owner installation before starting GitHub-enabled workers")
	}
	ref, err := secret.ParseRef(app.PrivateKey)
	if err != nil {
		return token, errors.New("invalid github.workerApp.privateKey vault reference")
	}
	if err := a.sandboxFiles([]secret.Ref{ref}, nil); err != nil {
		return token, err
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return token, err
	}
	err = ops.Use(ctx, ref, func(pem string) error {
		key, err := parseAppKey(pem)
		if err != nil {
			return errors.New("invalid worker GitHub App key")
		}
		jwt, err := appJWT(app.AppID, key, time.Now())
		if err != nil {
			return errors.New("worker GitHub App signing failed")
		}
		permissions := map[string]string{"contents": workerGitHubRead, "issues": workerGitHubRead, "pull_requests": workerGitHubRead, "checks": workerGitHubRead, "actions": workerGitHubRead, "statuses": workerGitHubRead, "metadata": workerGitHubRead}
		if request.Write {
			permissions["contents"] = workerGitHubWrite
			permissions["workflows"] = workerGitHubWrite
			permissions["pull_requests"] = workerGitHubWrite
		}
		body, _ := json.Marshal(map[string]any{"repositories": []string{repo}, "permissions": permissions})
		endpoint := githubAPI + "/app/installations/" + strconv.FormatInt(app.Installations[owner], 10) + "/access_tokens"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return errors.New("invalid worker GitHub token request")
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := githubClient.Do(req)
		if err != nil {
			return errors.New("worker GitHub token transport failed")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("worker GitHub token refused: HTTP %d", resp.StatusCode)
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || token.Token == "" || !token.Expires.After(time.Now()) || time.Until(token.Expires) > time.Hour+time.Minute {
			return errors.New("invalid worker GitHub token response")
		}
		return nil
	})
	return token, err
}

func (a *app) remoteGitHubBudget(ctx context.Context, request workerBudgetRequest) error {
	s, j, err := a.executionCaller()
	if err != nil {
		return err
	}
	co := s.Config.Coordinators[j.Request.Coordinator]
	var result workerBudgetResult
	if err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), workerGitHubBudget, j.Handle.Ref, request, j.Request.ControlKey, &result, s.Callback); err != nil {
		return err
	}
	if _, err := io.WriteString(a.out, result.Output); err != nil {
		return err
	}
	if result.Code != 0 {
		return &exitError{code: result.Code, msg: result.Error}
	}
	return nil
}

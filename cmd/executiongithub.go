package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Only these responses, over the authenticated SSH callback, carry a short-lived
// user-to-server token. They never enter journals, control receipts or logs.
type workerGitHubToken struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires_at"`
}

type workerGitHubRequest struct {
	Repository  string `json:"repository"`
	Write       bool   `json:"write"`
	ResolvePush bool   `json:"resolve_push,omitempty"`
	PushBranch  string `json:"push_branch,omitempty"`
}

type workerOAuthTokenResponse struct {
	AccessToken      string    `json:"access_token"`
	TokenType        string    `json:"token_type"`
	ExpiresIn        int64     `json:"expires_in"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	Error            string    `json:"error"`
	ErrorDescription string    `json:"error_description"`
}

const (
	workerGitHubConfigOption = "--config"
	workerGitHubWrite        = "write"
	workerGitHubCreate       = "create"
	workerGitHubPush         = "push"
	workerGitHubOrigin       = "origin"
)

const (
	workerGitHubOAuthTokenPath    = "/login/oauth/access_token" //nolint:gosec // G101: GitHub OAuth endpoint path, not a credential
	workerGitHubDeviceCodePath    = "/login/device/code"
	workerGitHubDeviceGrant       = "urn:ietf:params:oauth:grant-type:device_code"
	workerGitHubClientIDField     = "client_id"
	workerGitHubRefreshTokenField = "refresh_token"
	workerGitHubCurrentKey        = "current"
	workerGitHubOutstandingKey    = "outstanding"
	workerGitHubReadSuffix        = "/read"
)

var workerGitHubPollSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	workerGitHubIssueGroup    = "issue"
	workerGitHubAPIGroup      = "api"
	workerGitHubRepoGroup     = "repo"
	workerGitHubRunGroup      = "run"
	workerGitHubWorkflowGroup = "workflow"
	workerGitHubReleaseGroup  = "release"
)

const workerGitHubClone = "clone"
const workerGitHubWindowsOS = "windows"

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

func (a *app) workerGitHubOAuthPath() string {
	return filepath.Join(a.cfg.StateDir, "execution-github", "oauth.json")
}

// executionEnded says why the coordinator serves an execution no more, "" for
// one it serves.
func (a *app) executionEnded(st *state.State, r *state.ExecutionRecord) string {
	switch {
	case r == nil:
		return "this coordinator holds no record of the execution"
	case r.Request.Coordinator != a.cfg.Identity.Host:
		return "the execution belongs to another coordinator"
	case r.StopRequest != nil:
		return "the execution has a stop request"
	case r.Terminal() || r.Completed:
		return "the execution has ended; a new execution gets its own access"
	case st.Stop != nil:
		return "beekeeper is stopped; access returns when the stop lifts"
	}
	return ""
}

func (a *app) workerGitHubLive(ref execution.Ref, request workerGitHubRequest) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	r, _ := executionRecord(st, ref)
	if why := a.executionEnded(st, r); why != "" {
		return errors.New("worker GitHub access revoked: " + why)
	}
	if !r.Request.GitHub {
		return errors.New("worker has no scoped GitHub credential: github.workerApp was not configured on the coordinator when this execution started; gh uses the host's own login")
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
	if !ok || execution.RepositoryIdentity("https://github.com/"+request.Repository) != request.Repository {
		return errors.New("invalid worker GitHub repository")
	}
	if request.Write && !slices.Contains(a.cfg.GitHub.AllowedOrgs, owner) {
		return errors.New("worker GitHub repository is outside configured owners")
	}
	if request.PushBranch != "" {
		branch := executionOwnedBranch(r)
		if assignedExecutionReviewRound(st, ref) != nil {
			if request.Repository != r.Request.Repository {
				return refused("review push repository mismatch")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			branch, err = executionReviewWriteBranch(ctx, st, ref)
			if err != nil {
				return err
			}
		}
		if !request.Write || request.PushBranch != branch {
			return refused("push branch authorization changed")
		}
	}
	if a.cfg.GitHub.WorkerApp == nil || a.cfg.GitHub.WorkerApp.Validate() != nil {
		return errors.New("worker GitHub App is no longer configured")
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
		// The budget is the coordinator's, so reading it needs a live execution,
		// not a scoped credential: a host's own gh login spends it the same.
		if why := a.executionEnded(st, r); why != "" {
			return nil, errors.New("worker GitHub access revoked: " + why)
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
	if request.ResolvePush {
		if !request.Write {
			return nil, refused("push must target the execution repository")
		}
		if err := a.workerGitHubLive(req.Ref, request); err != nil {
			return nil, err
		}
		branch := executionOwnedBranch(r)
		if assignedExecutionReviewRound(st, req.Ref) != nil {
			if request.Repository != r.Request.Repository {
				return nil, refused("review push repository mismatch")
			}
			branch, err = executionReviewWriteBranch(ctx, st, req.Ref)
			if err != nil {
				return nil, err
			}
		}
		return workerGitHubPushTarget{Branch: branch}, nil
	}
	// Serialize issuance and revocation independently of the scheduling store;
	// GitHub and vault I/O never hold its lock.
	path := a.workerGitHubPath(req.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	if ok, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil || !ok {
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
		key = request.Repository + "/" + workerGitHubWrite
	}
	// Always consult the coordinator cache: refresh invalidates every older
	// access token, including unexpired entries in execution ledgers.
	token, err := a.mintWorkerGitHub(ctx, request)
	if err != nil {
		return nil, err
	}
	if cached, ok := tokens[key]; ok && cached == token {
		if err := a.workerGitHubLive(req.Ref, request); err != nil {
			_ = a.revokeWorkerGitHubToken(ctx, req.Ref, token.Token)
			return nil, err
		}
		return token, nil
	}
	// Keep old tokens until revoked: replacing a token must not lose authority
	// to revoke credentials a command may still be using.
	tokens[key+"/"+strconv.FormatInt(time.Now().UnixNano(), 10)] = token
	tokens[key] = token
	if err := writeWorkerGitHub(path, tokens); err != nil {
		_ = a.revokeWorkerGitHubToken(ctx, req.Ref, token.Token)
		return nil, err
	}
	// A stop may have been recorded while the vault or GitHub answered.
	if err := a.workerGitHubLive(req.Ref, request); err != nil {
		_ = a.revokeWorkerGitHubToken(ctx, req.Ref, token.Token)
		return nil, err
	}
	return token, nil
}

func readWorkerGitHub(path string) (map[string]workerGitHubToken, error) {
	tokens := map[string]workerGitHubToken{}
	b, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // coordinator state path derived only from the authenticated, validated execution ref
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
	defer func() { _ = os.Remove(f.Name()) }() //nolint:gosec // name returned by CreateTemp in the private coordinator ledger directory
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
	if err := os.Rename(f.Name(), path); err != nil { //nolint:gosec // temp and destination share the private coordinator directory, keyed by validated execution identity
		return err
	}
	// Persist the rename before a token is released to its worker. Windows
	// cannot sync directory handles; execution hosts themselves require Linux.
	if runtime.GOOS == workerGitHubWindowsOS {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path)) //nolint:gosec // same operator-owned ledger directory, never a worker-selected path
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (a *app) revokeWorkerGitHub(ctx context.Context, ref execution.Ref) error {
	if err := ref.Validate(); err != nil {
		return errors.New("invalid worker GitHub credential identity")
	}
	path := a.workerGitHubPath(ref)
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) { //nolint:gosec // operator-owned coordinator state directory, not a worker-selected path
		return nil
	}
	lock := flock.New(path + ".lock")
	if ok, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil || !ok {
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
			err = a.revokeWorkerGitHubToken(ctx, ref, token.Token)
			seen[token.Token] = err
		}
		if err != nil {
			remaining[key] = token
		}
	}
	if len(tokens) == 0 {
		// A previous execution may have deferred rotation while this one
		// was live, even if this execution never requested a credential.
		return a.revokeWorkerGitHubToken(ctx, ref, "")
	}
	if err := writeWorkerGitHub(path, remaining); err != nil {
		return err
	}
	if len(remaining) > 0 {
		return errors.New("worker GitHub revocation failed; ledger retained for retry")
	}
	// This ledger may contain only an old token, while another execution
	// handed out the current shared credential before completing.
	return a.revokeWorkerGitHubToken(ctx, ref, "")
}

// Shared authority can only be rotated when no other live execution uses it.
// Stopped executions immediately lose callback access; their already-issued
// credential remains valid until the last live execution finishes or it expires.
func (a *app) revokeWorkerGitHubToken(ctx context.Context, ref execution.Ref, token string) error {
	oauthPath := a.workerGitHubOAuthPath()
	if err := os.MkdirAll(filepath.Dir(oauthPath), 0o700); err != nil { //nolint:gosec // operator-owned coordinator state directory
		return err
	}
	lock := flock.New(oauthPath + ".lock")
	if ok, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil || !ok {
		return errors.New("worker GitHub credentials busy during revocation")
	}
	defer func() { _ = lock.Unlock() }()
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	for _, start := range st.Starts {
		r := start.Execution
		if r != nil && r.Request.Ref != ref && r.Request.GitHub && r.Request.Coordinator == a.cfg.Identity.Host && !r.Completed && !r.Terminal() && r.StopRequest == nil && st.Stop == nil {
			if !r.Request.LeaseRequired || (r.Lease != nil && r.Lease.ExpiresAt.After(time.Now())) {
				return nil
			}
		}
	}
	cached, err := readWorkerGitHub(oauthPath)
	if err != nil {
		return err
	}
	if token == "" {
		outstanding, ok := cached[workerGitHubOutstandingKey]
		if !ok || outstanding.Token == "" {
			return nil
		}
		token = outstanding.Token
	}
	if current, ok := cached[workerGitHubCurrentKey]; ok && current.Token != "" && current.Token != token && time.Now().Before(current.Expires) {
		return nil
	}
	delete(cached, workerGitHubCurrentKey)
	_ = writeWorkerGitHub(oauthPath, cached)
	_, err = a.refreshWorkerGitHubToken(ctx, oauthPath)
	return err
}

func (a *app) mintWorkerGitHub(ctx context.Context, _ workerGitHubRequest) (workerGitHubToken, error) {
	oauthPath := a.workerGitHubOAuthPath()
	if err := os.MkdirAll(filepath.Dir(oauthPath), 0o700); err != nil { //nolint:gosec // operator-owned coordinator state directory
		return workerGitHubToken{}, err
	}
	lock := flock.New(oauthPath + ".lock")
	if ok, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil || !ok {
		return workerGitHubToken{}, errors.New("worker GitHub credentials busy")
	}
	defer func() { _ = lock.Unlock() }()
	cached, err := readWorkerGitHub(oauthPath)
	if err != nil {
		return workerGitHubToken{}, err
	}
	token, ok := cached[workerGitHubCurrentKey]
	if !ok || token.Token == "" || time.Until(token.Expires) <= 5*time.Minute {
		token, err = a.refreshWorkerGitHubToken(ctx, oauthPath)
		if err != nil {
			return workerGitHubToken{}, err
		}
		cached = map[string]workerGitHubToken{workerGitHubCurrentKey: token}
	}
	// Remember shared authority independently of any execution ledger so the
	// last execution can revoke it even if it never fetched a token itself.
	if outstanding, ok := cached[workerGitHubOutstandingKey]; !ok || outstanding != token {
		cached[workerGitHubOutstandingKey] = token
		if err := writeWorkerGitHub(oauthPath, cached); err != nil {
			return workerGitHubToken{}, err
		}
	}
	return token, nil
}

func (a *app) refreshWorkerGitHubToken(ctx context.Context, oauthPath string) (workerGitHubToken, error) {
	var token workerGitHubToken
	w := a.cfg.GitHub.WorkerApp
	if w == nil || w.Validate() != nil {
		return token, errors.New("configure github.workerApp with clientID and refreshToken before starting GitHub-enabled workers")
	}
	ref, err := secret.ParseRef(w.RefreshToken)
	if err != nil || !ref.Single() || ref.IsSource() {
		return token, errors.New("invalid github.workerApp.refreshToken reference")
	}
	if err := a.sandboxFiles([]secret.Ref{ref}, nil); err != nil {
		return token, err
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return token, err
	}
	var nextRefresh string
	err = ops.Use(ctx, ref, func(refreshToken string) error {
		refreshToken = strings.TrimSpace(refreshToken)
		if refreshToken == "" {
			return errors.New("empty worker GitHub refresh token")
		}
		resp, err := githubSend(ctx, http.MethodPost, githubWeb+workerGitHubOAuthTokenPath, "application/json", "", url.Values{
			workerGitHubClientIDField:     {w.ClientID},
			"grant_type":                  {workerGitHubRefreshTokenField},
			workerGitHubRefreshTokenField: {refreshToken},
		})
		if err != nil {
			return errors.New("worker GitHub token transport failed")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("worker GitHub token refused: HTTP %d", resp.StatusCode)
		}
		var out workerOAuthTokenResponse
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil {
			return errors.New("invalid worker GitHub token response")
		}
		if out.Error != "" {
			return fmt.Errorf("worker GitHub token refused: %s", bounded(out.Error, phraseRunes))
		}
		expires := out.ExpiresAt
		if expires.IsZero() && out.ExpiresIn > 0 {
			expires = time.Now().UTC().Add(time.Duration(out.ExpiresIn) * time.Second)
		}
		if out.AccessToken == "" || out.RefreshToken == "" || !expires.After(time.Now()) || time.Until(expires) > 8*time.Hour+time.Minute {
			return errors.New("invalid worker GitHub token response")
		}
		token = workerGitHubToken{Token: out.AccessToken, Expires: expires}
		nextRefresh = out.RefreshToken
		return nil
	})
	if err != nil {
		return workerGitHubToken{}, err
	}
	if err := a.storeWorkerRefreshToken(ctx, ref, nextRefresh); err != nil {
		return workerGitHubToken{}, errors.New("cannot persist rotated GitHub refresh token; authorization is lost: run beekeeper agents hosts github-login again")
	}
	if oauthPath != "" {
		if err := writeWorkerGitHub(oauthPath, map[string]workerGitHubToken{workerGitHubCurrentKey: token}); err != nil {
			return workerGitHubToken{}, err
		}
	}
	return token, nil
}

func (a *app) storeWorkerRefreshToken(ctx context.Context, ref secret.Ref, value string) error {
	switch {
	case ref.Op != "":
		_, err := a.storeValue(ctx, ref, value)
		return err
	case ref.File != "" && ref.Path != "":
		if err := a.sandboxFiles(nil, []secret.Ref{ref}); err != nil {
			return err
		}
		ops, err := a.secretOpsKeyed()
		if err != nil {
			return err
		}
		return ops.SetSOPSValue(ctx, ref, value)
	default:
		return errors.New("github.workerApp.refreshToken must name an op:// or sops:// secret reference")
	}
}

func (a *app) authorizeWorkerGitHub(ctx context.Context, clientID, refreshRef string, out io.Writer) error {
	if a.managedExecutionID() != "" {
		return refused("managed workers cannot authorize coordinator GitHub credentials")
	}
	if w := a.cfg.GitHub.WorkerApp; w != nil {
		if clientID == "" {
			clientID = w.ClientID
		}
		if refreshRef == "" {
			refreshRef = w.RefreshToken
		}
	}
	w := &config.WorkerGitHub{ClientID: strings.TrimSpace(clientID), RefreshToken: strings.TrimSpace(refreshRef)}
	if err := w.Validate(); err != nil {
		return usageErr("%v", err)
	}
	ref, err := secret.ParseRef(w.RefreshToken)
	if err != nil || !ref.Single() || ref.IsSource() {
		return usageErr("invalid github.workerApp.refreshToken reference")
	}
	resp, err := githubSend(ctx, http.MethodPost, githubWeb+workerGitHubDeviceCodePath, "application/json", "", url.Values{workerGitHubClientIDField: {w.ClientID}})
	if err != nil {
		return fmt.Errorf("requesting GitHub device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&dc) != nil {
		return fmt.Errorf("requesting GitHub device code: HTTP %d, no JSON", resp.StatusCode)
	}
	if dc.Error != "" {
		return fmt.Errorf("requesting GitHub device code: %s", bounded(dc.Error, phraseRunes))
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return fmt.Errorf("requesting GitHub device code: HTTP %d returned no code", resp.StatusCode)
	}
	uri := dc.VerificationURI
	if uri == "" {
		uri = githubWeb + "/login/device"
	}
	if _, err := fmt.Fprintf(out, "Open %s and enter code: %s\n", uri, dc.UserCode); err != nil {
		return err
	}
	interval := time.Duration(max(dc.Interval, 1)) * time.Second
	expiresIn := dc.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	for time.Now().Before(deadline) {
		if err := workerGitHubPollSleep(ctx, interval); err != nil {
			return err
		}
		pollResp, err := githubSend(ctx, http.MethodPost, githubWeb+workerGitHubOAuthTokenPath, "application/json", "", url.Values{
			workerGitHubClientIDField: {w.ClientID},
			"device_code":             {dc.DeviceCode},
			"grant_type":              {workerGitHubDeviceGrant},
		})
		if err != nil {
			return fmt.Errorf("polling GitHub device flow: %w", err)
		}
		var tok workerOAuthTokenResponse
		decodeErr := json.NewDecoder(io.LimitReader(pollResp.Body, 1<<20)).Decode(&tok)
		_ = pollResp.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("polling GitHub device flow: HTTP %d, no JSON", pollResp.StatusCode)
		}
		switch tok.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "":
		default:
			return fmt.Errorf("GitHub device flow: %s", bounded(tok.Error, phraseRunes))
		}
		expires := tok.ExpiresAt
		if expires.IsZero() && tok.ExpiresIn > 0 {
			expires = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
		}
		if tok.AccessToken == "" || tok.RefreshToken == "" || !expires.After(time.Now()) {
			return errors.New("GitHub device flow returned no expiring user-to-server token; enable user token expiration on the GitHub App")
		}
		var user struct {
			Login string `json:"login"`
		}
		if err := githubJSON(ctx, http.MethodGet, "/user", "Bearer "+tok.AccessToken, http.StatusOK, &user); err != nil {
			return fmt.Errorf("verifying GitHub user token: %w", err)
		}
		// Device flow may take minutes: lock only its commit, using the same
		// lock as issuance and revocation for the refresh token/cache pair.
		oauthPath := a.workerGitHubOAuthPath()
		if err := os.MkdirAll(filepath.Dir(oauthPath), 0o700); err != nil { //nolint:gosec // operator-owned coordinator state directory
			return err
		}
		lock := flock.New(oauthPath + ".lock")
		if ok, err := lock.TryLockContext(ctx, 100*time.Millisecond); err != nil || !ok {
			return errors.New("worker GitHub credentials busy during authorization")
		}
		defer func() { _ = lock.Unlock() }()
		if err := a.storeWorkerRefreshToken(ctx, ref, tok.RefreshToken); err != nil {
			return errors.New("cannot persist GitHub refresh token: run beekeeper agents hosts github-login again")
		}
		if err := writeWorkerGitHub(oauthPath, map[string]workerGitHubToken{workerGitHubCurrentKey: {Token: tok.AccessToken, Expires: expires}}); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "authorized %s for github.workerApp (%s)\n", user.Login, ref.String())
		return err
	}
	return errors.New("GitHub device flow timed out before authorization")
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

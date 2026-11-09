package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const executionPublish = "publish"
const publishBundleLimit = 64 << 20

// publishCreateTimeout bounds the pull request's creation and the read that
// settles its outcome, each on its own: the transfer before them may have
// spent the attempt's time, and a retry would spend it the same way.
const publishCreateTimeout = 15 * time.Second

// publicationTitle and publicationBody name a pull request's fields in a
// publication's result.
const (
	publicationTitle = "title"
	publicationBody  = "body"
)

const (
	publicationWriteDenied = "GitHub denied the coordinator's credential write access to the repository"
	publicationUnreachable = "the coordinator could not reach GitHub"
)

type executionPublication struct {
	Head         string `json:"head"`
	BundleDigest string `json:"bundleDigest,omitempty"`
	Title        string `json:"title,omitempty"`
	Body         string `json:"body,omitempty"`
	SyncBase     bool   `json:"syncBase,omitempty"`
	Shallow      bool   `json:"shallow,omitempty"`
}
type executionPublicationResult struct {
	Branch       string `json:"branch"`
	URL          string `json:"url,omitempty"`
	Head         string `json:"head"`
	BundleDigest string `json:"bundleDigest,omitempty"`
	BundleSize   int64  `json:"bundleSize,omitempty"`
	// Updated names the fields of an existing pull request this publication
	// changed (title, body).
	Updated []string `json:"updated,omitempty"`
}

// publicationPull is the owned pull request as GitHub lists it.
type publicationPull struct {
	URL   string `json:"url"`
	State string `json:"state"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func executionBranch(ref execution.Ref) string {
	return execution.GitHubBranch(ref)
}

func executionOwnedBranch(r *state.ExecutionRecord) string {
	if r != nil {
		if r.Request.GitHub {
			return execution.GitHubBranch(r.Handle.Ref)
		}
		return executionBranch(r.Handle.Ref)
	}
	return ""
}

func assignedExecutionReviewRound(st *state.State, ref execution.Ref) *state.DeskReview {
	if st == nil {
		return nil
	}
	_, i := executionRecord(st, ref)
	if i < 0 || i >= len(st.Starts) {
		return nil
	}
	for n := len(st.DeskReviews) - 1; n >= 0; n-- {
		candidate := &st.DeskReviews[n]
		if sameReviewSession(candidate.Reviewer, st.Starts[i].Party) {
			return candidate
		}
	}
	return nil
}

// executionReviewWriteBranch derives the write grant from the latest durable
// review assignment and its concrete execution session. Source selection alone
// never grants write access. Callers also require a live execution lease.
func executionReviewWriteBranch(ctx context.Context, st *state.State, ref execution.Ref) (string, error) {
	r, i := executionRecord(st, ref)
	round := assignedExecutionReviewRound(st, ref)
	if r == nil || r.Completed || r.Terminal() || r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()) || i < 0 || round == nil || round.ID == "" || round.Round <= 0 || round.Result != nil || round.Head != r.Handle.Commit {
		return "", refused("no active review branch assignment")
	}
	for n := len(st.DeskReviews) - 1; n >= 0; n-- {
		candidate := &st.DeskReviews[n]
		if strings.EqualFold(candidate.Ref, round.Ref) {
			if candidate.ID != round.ID {
				return "", refused("review branch assignment was superseded")
			}
			break
		}
	}
	pr, ok := parseRef(round.Ref)
	if !ok || !strings.EqualFold(pr.Repo, r.Request.Repository) || !sameReviewSession(round.Reviewer, st.Starts[i].Party) {
		return "", refused("review branch assignment identity mismatch")
	}
	pull, err := readLoopPull(ctx, round.Ref)
	if err != nil {
		return "", err
	}
	if !r.Lease.ExpiresAt.After(time.Now()) {
		return "", refused("review branch assignment expired during revalidation")
	}
	if pull.State != superloop.Open || !strings.EqualFold(pull.Ref, round.Ref) || pull.Branch == "" || pull.Branch != r.Request.GitRef {
		return "", refused("review branch assignment no longer matches the open pull request")
	}
	return pull.Branch, nil
}

func publicationCommit(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 20
}

// No shell, global Git configuration, hooks, filters or alternate transport
// selected by a worker. Only the coordinator's gh credential helper is used.
var executionPublishGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runPublicationGit(ctx, dir, nil, args...)
}

func runPublicationGit(ctx context.Context, dir string, identity []string, args ...string) ([]byte, error) {
	// No detached maintenance either: it outlives the command and races the
	// removal of the transfer directory.
	prefix := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}
	c := exec.CommandContext(ctx, "git", append(prefix, args...)...) //nolint:gosec // fixed Git subcommands, validated commit/repository identities and component-owned paths; no shell
	c.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			c.Env = append(c.Env, kv)
		}
	}
	c.Env = append(c.Env, identity...)
	c.Env = append(c.Env, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, publicationGitFailure(ctx, args, stderr.String())
	}
	return out, nil
}

// publicationGitStages are the Git subcommands publication runs; any other
// argument is never echoed back as a stage.
var publicationGitStages = []string{"init", "fetch", "push", gitBundle, "cat-file", "rev-parse", "rev-list", "merge-base", "log", gitMerge, "diff", "update-ref"}

const (
	gitBundle = "bundle"
	gitMerge  = "merge"
	gitVerify = "verify"
)

// publicationGitReasons map a known Git or GitHub rejection to a fixed
// diagnosis. The raw output may carry URLs, paths or remote text, so only the
// diagnosis leaves the coordinator. The first match wins.
var publicationGitReasons = []struct{ match, reason string }{
	{"create or update workflow", "GitHub refused a workflow file change: the coordinator's GitHub credential lacks the workflow scope or permission; an authorization barrier, not a transfer, ancestry or execution-policy error, and a retry fails the same until the credential is granted it"},
	{"GH006", "GitHub refused the push: a protected branch rule rejects it"},
	{"GH013", "GitHub refused the push: a repository rule rejects it"},
	{"non-fast-forward", "the remote branch moved: the push is not a fast-forward of it"},
	{"fetch first", "the remote branch moved: the push is not a fast-forward of it"},
	{"Authentication failed", "the coordinator's GitHub credential was not accepted"},
	{"could not read Username", "the coordinator has no usable GitHub credential"},
	{"terminal prompts disabled", "the coordinator has no usable GitHub credential"},
	{"Permission to", publicationWriteDenied},
	{"returned error: 403", publicationWriteDenied},
	{"Repository not found", "GitHub did not find the repository for the coordinator's credential"},
	{"Could not resolve host", publicationUnreachable},
	{"Failed to connect", publicationUnreachable},
	{"prerequisite commits", "the repository lacks prerequisite commits required by the bundle"},
	{"vorausgesetzte Commits", "the repository lacks prerequisite commits required by the bundle"},
	{"did not send all necessary objects", "the bundle is missing ancestor objects required to connect history"},
	{"Failed to traverse parents", "the bundle is missing ancestor objects required to connect history"},
	{"unrelated histories", "merge refused unrelated histories because the common base commit is missing"},
	{"shallow", "shallow repository history is incomplete for this operation"},
	{"Refusing to create empty bundle", "no commits to bundle beyond the base commit"},
	{"empty bundle", "no commits to bundle beyond the base commit"},
	{"couldn't find remote ref", "the requested Git ref does not exist in the remote repository"},
	{"not a valid object name", publicationMissingHistoryReason},
	{"bad object", publicationMissingHistoryReason},
	{"bad revision", publicationMissingHistoryReason},
	{"unknown revision", publicationMissingHistoryReason},
	{"CONFLICT", "merging the upstream default branch produced conflicts"},
	{"Automatic merge failed", "merging the upstream default branch produced conflicts"},
}

const publicationMissingHistoryReason = "a required commit or ref is missing from local Git history"

// publicationGHReasons map a known gh or GitHub answer to a fixed diagnosis,
// as publicationGitReasons do for Git. The first match wins.
var publicationGHReasons = []struct{ match, reason string }{
	{"No commits between", "GitHub sees no commits between the default branch and the owned branch"},
	{"submitted too quickly", "GitHub's secondary rate limit refused it"},
	{"rate limit", "GitHub's rate limit refused it"},
	{"HTTP 401", "the coordinator's GitHub credential was not accepted"},
	{"HTTP 403", publicationWriteDenied},
	{"Resource not accessible", publicationWriteDenied},
	{"HTTP 404", "GitHub did not find the repository for the coordinator's credential"},
	{"HTTP 422", "GitHub rejected the request as invalid"},
	{"HTTP 5", "GitHub answered with a server error"},
	{"could not connect", publicationUnreachable},
	{"connection", publicationUnreachable},
	{timeoutComm, "GitHub did not answer in time"},
}

// publicationGHReason names why a gh call failed, never its raw output.
func publicationGHReason(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "GitHub did not answer in time"
	}
	for _, r := range publicationGHReasons {
		if strings.Contains(err.Error(), r.match) {
			return r.reason
		}
	}
	return "unclassified GitHub failure"
}

// publicationGitFailure names the failed stage and a sanitized reason, never
// the command's raw output, so an operator can tell an authorization barrier
// from a transfer fault without exposing credentials.
func publicationGitFailure(ctx context.Context, args []string, stderr string) error {
	stage := "operation"
	if len(args) > 0 && slices.Contains(publicationGitStages, args[0]) {
		stage = args[0]
		if stage == gitBundle && len(args) > 1 && slices.Contains([]string{"create", gitVerify, "list-heads"}, args[1]) {
			stage += " " + args[1]
		}
	}
	reason := "unclassified Git failure"
	if ctx.Err() != nil {
		reason = "the publication attempt ran out of time"
	} else {
		for _, r := range publicationGitReasons {
			if strings.Contains(stderr, r.match) {
				reason = r.reason
				break
			}
		}
	}
	return fmt.Errorf("publication Git %s failed: %s", stage, reason)
}

var executionPublishGH = github.RunGH

func publishGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := executionPublishGit(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// ownedPulls lists the pull requests of the owned branch, in any state.
func ownedPulls(ctx context.Context, repository, branch string) ([]publicationPull, error) {
	raw, err := executionPublishGH(ctx, "pr", "list", loopRepoFlag, repository, "--head", branch, "--state", "all", "--json", "url,state,"+publicationTitle+","+publicationBody)
	if err != nil {
		// gh pr list uses GraphQL. Its failure must not strand a pushed
		// branch when the REST pull endpoint can settle the same outcome.
		return ownedPullsREST(ctx, repository, branch)
	}
	var pulls []publicationPull
	if json.Unmarshal(raw, &pulls) != nil || len(pulls) > 1 {
		return nil, errors.New("ambiguous owned pull request")
	}
	return pulls, nil
}

// ownedPullsREST uses a separate read deadline: the GraphQL attempt may
// have exhausted its context. This fallback never grants authority to write.
func ownedPullsREST(ctx context.Context, repository, branch string) ([]publicationPull, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishCreateTimeout)
	defer cancel()
	owner, _, _ := strings.Cut(repository, "/")
	endpoint := "repos/" + repository + "/pulls?state=all&per_page=2&head=" + url.QueryEscape(owner+":"+branch)
	raw, err := executionPublishGH(readCtx, "api", endpoint)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		URL   string `json:"html_url"`
		State string `json:"state"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) > 1 {
		return nil, errors.New("ambiguous owned pull request")
	}
	pulls := make([]publicationPull, 0, len(rows))
	for _, row := range rows {
		if row.URL == "" || (row.State != "open" && row.State != "closed") {
			return nil, errors.New("invalid owned pull request")
		}
		pulls = append(pulls, publicationPull{URL: row.URL, State: strings.ToUpper(row.State), Title: row.Title, Body: row.Body})
	}
	return pulls, nil
}

// settleCreatedPull answers a failed gh pr create by the owned branch: a pull
// request found there is the lost answer's, its URL returned. None found, the
// failure is GitHub's and said as such; only an unreadable branch stays
// uncertain. A read may outlive the lease and ctx, which may be why create
// failed; a write may not.
func settleCreatedPull(ctx context.Context, repository, branch, reason string) (string, error) {
	listCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishCreateTimeout)
	defer cancel()
	pulls, err := ownedPulls(listCtx, repository, branch)
	switch {
	case err != nil:
		return "", fmt.Errorf("PR creation outcome uncertain: %s, and the owned branch's pull request could not be read: %s; retry publication to reconcile the owned branch", reason, publicationGHReason(listCtx, err))
	case len(pulls) == 1:
		return pulls[0].URL, nil
	}
	return "", fmt.Errorf("GitHub created no pull request for the owned branch: %s; the branch is pushed, and an identical publication creates it once that is resolved", reason)
}

// publicationWrite rechecks a publication's authority and gives one GitHub
// write its own time, within the lease: a push may have spent the attempt's.
type publicationWrite func() (context.Context, context.CancelFunc, error)

// updatePublishedPull applies the title and body a republication names to its
// existing pull request. A flag left out keeps the field; one that matches
// changes nothing, so an identical retry writes nothing.
func updatePublishedPull(repository, dir string, pull publicationPull, p executionPublication, write publicationWrite) ([]string, error) {
	var updated []string
	// REST, not gh pr edit: that reads review requests, which an App token may
	// not reach, and fails the edit with them.
	args := []string{workerGitHubAPIGroup, "-X", "PATCH", "repos/" + repository + "/pulls/" + path.Base(pull.URL), "--silent"}
	if title := strings.TrimSpace(p.Title); title != "" && title != strings.TrimSpace(pull.Title) {
		args = append(args, "--raw-field", publicationTitle+"="+p.Title)
		updated = append(updated, publicationTitle)
	}
	if strings.TrimSpace(p.Body) != "" && strings.TrimSpace(p.Body) != strings.TrimSpace(pull.Body) {
		bodyPath := filepath.Join(dir, "body.md")
		if err := os.WriteFile(bodyPath, []byte(p.Body), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--field", publicationBody+"=@"+bodyPath)
		updated = append(updated, publicationBody)
	}
	if len(updated) == 0 {
		return nil, nil
	}
	editCtx, cancel, err := write()
	if err != nil {
		return nil, err
	}
	defer cancel()
	if _, err := executionPublishGH(editCtx, args...); err != nil {
		return nil, fmt.Errorf("commits are pushed to %s, but its %s was not updated: %s; retry publication to update it", pull.URL, strings.Join(updated, " and "), publicationGHReason(editCtx, err))
	}
	return updated, nil
}

func (a *app) executionPublicationRecoverable(st *state.State, ref execution.Ref) (*state.ExecutionRecord, error) {
	r, _ := executionRecord(st, ref)
	if r == nil || !execution.SupportedHarness(r.Request.Harness) || !r.Request.LeaseRequired || r.Released() || r.StopRequest != nil || st.Stop != nil {
		return nil, refused("publication requires a registered, unstopped execution")
	}
	owner, _, _ := strings.Cut(r.Request.Repository, "/")
	if !slices.Contains(a.cfg.GitHub.AllowedOrgs, owner) {
		return nil, refused("publication repository is outside the coordinator allowlist")
	}
	for _, hold := range st.Holds {
		if hold.Target == holdGitHub && hold.Active(time.Now()) {
			return nil, refused("GitHub publication is held")
		}
	}
	if !publicationCommit(r.Handle.Commit) {
		return nil, refused("execution has no pinned source commit")
	}
	return r, nil
}

func (a *app) executionPublicationLive(st *state.State, ref execution.Ref) (*state.ExecutionRecord, error) {
	r, err := a.executionPublicationRecoverable(st, ref)
	if err != nil {
		return nil, err
	}
	if r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()) || r.Completed || r.Terminal() {
		return nil, refused("publication requires a live, leased execution")
	}
	return r, nil
}

// Called while the coordinating state lock is held, serializing stop and lease
// revocation with a bounded synchronous write. A retry reconciles the remote
// branch and PR before mutating; push is always a normal fast-forward push.
func (a *app) publishExecution(ctx context.Context, st *state.State, ref execution.Ref, p executionPublication) (executionPublicationResult, error) {
	return a.publishExecutionWithMode(ctx, st, ref, p, false)
}

func (a *app) publishExecutionWithMode(ctx context.Context, st *state.State, ref execution.Ref, p executionPublication, recovered bool) (executionPublicationResult, error) {
	var result executionPublicationResult
	checkAuthority := a.executionPublicationLive
	if recovered {
		checkAuthority = a.executionPublicationRecoverable
	}
	r, err := checkAuthority(st, ref)
	if err != nil {
		return result, err
	}
	if !recovered {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, r.Lease.ExpiresAt)
		defer cancel()
	}
	if !publicationCommit(p.Head) || len(p.Title) > 256 || len(p.Body) > 64<<10 {
		return result, refused("invalid or oversized publication")
	}
	if p.SyncBase && (p.BundleDigest != "" || p.Title != "" || p.Body != "") {
		return result, refused("base synchronization cannot publish a PR")
	}
	reviewRound := assignedExecutionReviewRound(st, ref)
	if reviewRound != nil && (strings.TrimSpace(p.Title) != "" || strings.TrimSpace(p.Body) != "") {
		return result, refused("reviewers may push self-fix commits to the assigned pull request branch, not create or edit pull request metadata")
	}
	write := func() (context.Context, context.CancelFunc, error) {
		if _, err := checkAuthority(st, ref); err != nil {
			return nil, nil, err
		}
		deadline := time.Now().Add(publishCreateTimeout)
		if !recovered && r.Lease.ExpiresAt.Before(deadline) {
			deadline = r.Lease.ExpiresAt
		}
		writeCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		return writeCtx, cancel, nil
	}
	root := filepath.Join(a.cfg.StateDir, "execution-publications")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp(root, "transfer-")
	if err != nil {
		return result, err
	}
	// Keep failed transfers for inspection. Successful transfers contain only
	// public repository objects, and are removed after their result is known.
	success := false
	defer func() {
		if success {
			_ = os.RemoveAll(dir)
		}
	}()
	repo := filepath.Join(dir, "repository.git")
	if _, err = publishGit(ctx, dir, "init", "--bare", repo); err != nil {
		return result, err
	}
	sourceBundle := filepath.Join(a.cfg.StateDir, "execution-sources", string(ref.Execution), "source.bundle")
	if data, readErr := readPublicationBundle(sourceBundle); readErr == nil && (r.Request.SourceDigest == "" || execution.Digest(data) == r.Request.SourceDigest) {
		_, _ = publishGit(ctx, repo, "fetch", "--no-tags", "--", sourceBundle, "refs/heads/execution-source:refs/heads/source")
	}
	remote := "https://github.com/" + r.Request.Repository + ".git"
	// Resolve the default branch from GitHub, never a worker-supplied ref.
	raw, err := executionPublishGH(ctx, "repo", "view", r.Request.Repository, "--json", "defaultBranchRef")
	if err != nil {
		return result, err
	}
	var metadata struct {
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if json.Unmarshal(raw, &metadata) != nil || metadata.DefaultBranchRef.Name == "" {
		return result, errors.New("cannot determine repository default branch")
	}
	base := metadata.DefaultBranchRef.Name
	if _, err = publishGit(ctx, repo, "fetch", "--no-tags", remote, "refs/heads/"+base+":refs/heads/base"); err != nil {
		return result, err
	}
	bundlePath := filepath.Join(dir, "commits.bundle")
	result.Branch = executionOwnedBranch(r)
	ensureCommit := func(commit string) {
		if !publicationCommit(commit) {
			return
		}
		if _, err := publishGit(ctx, repo, "cat-file", "-e", commit+"^{commit}"); err == nil {
			return
		}
		if result.Branch != "" {
			_, _ = publishGit(ctx, repo, "fetch", "--no-tags", "--", remote, "refs/heads/"+result.Branch+":refs/heads/published")
			if _, err := publishGit(ctx, repo, "cat-file", "-e", commit+"^{commit}"); err == nil {
				return
			}
		}
		_, _ = publishGit(ctx, repo, "fetch", "--no-tags", "--", remote, commit)
	}
	ensureCommit(r.Handle.Commit)
	if r.Published != nil && r.Published.Head != "" {
		ensureCommit(r.Published.Head)
	}
	if p.SyncBase {
		ensureCommit(p.Head)
		_, hasHeadErr := publishGit(ctx, repo, "cat-file", "-e", p.Head+"^{commit}")
		if hasHeadErr != nil && p.Head != r.Handle.Commit && !p.Shallow {
			return result, refused("base synchronization head is not in the default branch history")
		}
		result.Head, err = publishGit(ctx, repo, "rev-parse", "refs/heads/base")
		if err != nil {
			return result, err
		}
		if result.Head != p.Head || p.Shallow || hasHeadErr != nil {
			bundleArgs := []string{"bundle", "create", bundlePath, "refs/heads/base"}
			if !p.Shallow && hasHeadErr == nil {
				bundleArgs = append(bundleArgs, "^"+p.Head)
			} else if _, err := publishGit(ctx, repo, "cat-file", "-e", r.Handle.Commit+"^{commit}"); err == nil {
				if _, err := publishGit(ctx, repo, "update-ref", "refs/heads/source", r.Handle.Commit); err == nil {
					bundleArgs = append(bundleArgs, "refs/heads/source")
				}
			}
			if _, err = publishGit(ctx, repo, bundleArgs...); err != nil {
				return result, err
			}
			data, readErr := readPublicationBundle(bundlePath)
			if readErr != nil {
				return result, readErr
			}
			result.BundleDigest, err = a.storePublicationTransfer(ref, data)
			result.BundleSize = int64(len(data))
			if err != nil {
				return result, err
			}
		}
	} else {
		if p.BundleDigest == "" {
			return result, refused("publication requires a commit bundle")
		}
		data, transferErr := a.loadPublicationTransfer(ref, p.BundleDigest)
		if transferErr != nil {
			return result, transferErr
		}
		if err = os.WriteFile(bundlePath, data, 0o600); err != nil {
			return result, err
		}
		heads, headErr := publishGit(ctx, repo, "bundle", "list-heads", bundlePath)
		if headErr != nil || heads != p.Head+" HEAD" {
			return result, refused("bundle must advertise only the requested HEAD")
		}
		if _, err = publishGit(ctx, repo, "bundle", "verify", bundlePath); err != nil {
			if result.Branch != "" {
				_, _ = publishGit(ctx, repo, "fetch", "--no-tags", "--", remote, "refs/heads/"+result.Branch+":refs/heads/published")
			}
			if _, err = publishGit(ctx, repo, "bundle", "verify", bundlePath); err != nil {
				return result, err
			}
		}
		if _, err = publishGit(ctx, repo, "fetch", "--no-tags", bundlePath, "HEAD:refs/heads/publication"); err != nil {
			return result, err
		}
		if _, err = publishGit(ctx, repo, "merge-base", "--is-ancestor", r.Handle.Commit, p.Head); err != nil {
			return result, refused("publication does not descend from the execution's pinned commit")
		}
		// Scan only what this publication adds: commits a synced base brought in
		// are already public on the default branch and are not the worker's.
		messages, messageErr := publishGit(ctx, repo, "log", "--no-patch", "--format=%B", r.Handle.Commit+".."+p.Head, "^refs/heads/base", "--")
		if messageErr != nil {
			return result, messageErr
		}
		patch, patchErr := publishGit(ctx, repo, "log", "-p", "--no-color", "--no-ext-diff", "--no-textconv", "-U0", "--format=", r.Handle.Commit+".."+p.Head, "^refs/heads/base", "--")
		if patchErr != nil {
			return result, patchErr
		}
		var added strings.Builder
		inHunk := false
		for line := range strings.SplitSeq(patch, "\n") {
			switch {
			case strings.HasPrefix(line, "diff --git "):
				inHunk = false
			case strings.HasPrefix(line, "@@ "):
				inHunk = true
			}
			// Only patch deletion lines are omitted. Commit messages are
			// scanned separately and never passed through this parser.
			if inHunk && strings.HasPrefix(line, "-") {
				continue
			}
			added.WriteString(line)
			added.WriteByte('\n')
		}
		if reason := outboundGuard(a.cfg.Outbound).PublicationRefusal(p.Title+"\n"+p.Body, messages+"\n"+added.String()); reason != "" {
			return result, refused("%s", reason)
		}
		if reviewRound != nil {
			result.Branch, err = executionReviewWriteBranch(ctx, st, ref)
			if err != nil {
				return result, err
			}
		}
		// Reconcile a lost create response by the exact owned branch. An existing
		// PR takes only the title and body this publication names.
		pulls, listErr := ownedPulls(ctx, r.Request.Repository, result.Branch)
		if listErr != nil {
			return result, listErr
		}
		if reviewRound != nil {
			if len(pulls) != 1 || pulls[0].State != github.Open {
				return result, refused("reviewer publication requires the assigned open pull request")
			}
			if prRef, ok := parseRef(pulls[0].URL); !ok || !strings.EqualFold(refName(prRef), reviewRound.Ref) {
				return result, refused("reviewer publication must target the assigned pull request")
			}
		}
		if len(pulls) == 1 && pulls[0].State != github.Open {
			return result, refused("owned pull request is already closed; a new execution is required")
		}
		if len(pulls) == 0 && (strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Body) == "") {
			return result, refused("first publication requires --title and --body-file")
		}
		var absorbs, closing []string
		if len(pulls) == 0 {
			absorbs = superloop.ParseAbsorbedPulls(r.Request.Repository+"#0", p.Title, p.Body)
			if len(absorbs) > 0 {
				absorbedIssues, absorbErr := absorbedClosingIssues(ctx, st, absorbs)
				if absorbErr != nil {
					return result, absorbErr
				}
				p.Body = superloop.AppendClosingKeywords(r.Request.Repository+"#0", p.Body, absorbedIssues)
			}
			closing = superloop.ParseClosingIssues(r.Request.Repository+"#0", p.Title+"\n"+p.Body)
			if len(closing) > 0 {
				openPulls, listErr := readRepoOpenPullsWithGH(ctx, executionPublishGH, r.Request.Repository)
				if listErr != nil {
					return result, refused("open pull requests for %s cannot be read (%v); retry publication once GitHub is readable", r.Request.Repository, listErr)
				}
				matched, prURL, findErr := findOpenPullClosingIssues(ctx, executionPublishGH, r.Request.Repository, openPulls, result.Branch, closing, absorbs)
				if findErr != nil {
					return result, refused("open pull requests for %s cannot be read (%v); retry publication once GitHub is readable", r.Request.Repository, findErr)
				}
				if matched != "" {
					return result, refused("%s already has an open pull request %s; update %s or combine with it instead of opening a duplicate PR", matched, prURL, prURL)
				}
			}
		}
		if _, liveErr := checkAuthority(st, ref); liveErr != nil {
			return result, liveErr
		}
		if reviewRound != nil {
			branch, authErr := executionReviewWriteBranch(ctx, st, ref)
			if authErr != nil || branch != result.Branch {
				return result, refused("review branch authorization changed")
			}
		}
		if _, err = publishGit(ctx, repo, "push", remote, p.Head+":refs/heads/"+result.Branch); err != nil {
			return result, err
		}
		result.Head = p.Head
		if len(pulls) == 1 {
			result.URL = pulls[0].URL
			if result.Updated, err = updatePublishedPull(r.Request.Repository, dir, pulls[0], p, write); err != nil {
				return result, err
			}
		} else {
			bodyPath := filepath.Join(dir, "body.md")
			if err = os.WriteFile(bodyPath, []byte(p.Body), 0o600); err != nil {
				return result, err
			}
			createCtx, cancelCreate, writeErr := write()
			if writeErr != nil {
				return result, writeErr
			}
			defer cancelCreate()
			raw, err = executionPublishGH(createCtx, "pr", "create", loopRepoFlag, r.Request.Repository, "--base", base, "--head", result.Branch, "--title", p.Title, "--body-file", bodyPath)
			if err != nil {
				if result.URL, err = settleCreatedPull(ctx, r.Request.Repository, result.Branch, publicationGHReason(createCtx, err)); err != nil {
					return result, err
				}
			} else {
				result.URL = strings.TrimSpace(string(raw))
			}
			if len(closing) > 0 {
				if _, idx := executionRecord(st, ref); idx >= 0 && idx < len(st.Starts) {
					party := st.Starts[idx].Party
					_ = recordSessionIssues(st, party, party, time.Now(), closing)
				}
			}
		}
	}
	success = true
	return result, nil
}

func (a *app) findStrandedPublication(ctx context.Context, r *state.ExecutionRecord) (executionPublication, bool) {
	if r == nil || r.StopRequest != nil || r.Released() {
		return executionPublication{}, false
	}
	if p := r.PendingPublication; p != nil && publicationCommit(p.Head) && p.BundleDigest != "" {
		if r.Published == nil || r.Published.Head != p.Head {
			return executionPublication{Head: p.Head, BundleDigest: p.BundleDigest, Title: p.Title, Body: p.Body}, true
		}
	}
	if r.Published == nil {
		return executionPublication{}, false
	}
	transferDir := filepath.Join(a.cfg.StateDir, "execution-publications", "transfers", string(r.Handle.Ref.Execution), fmt.Sprint(r.Handle.Ref.Generation))
	entries, err := os.ReadDir(transferDir)
	if err != nil {
		return executionPublication{}, false
	}
	var newestName string
	var newestTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) != 64 {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		if newestName == "" || info.ModTime().After(newestTime) {
			newestName, newestTime = entry.Name(), info.ModTime()
		}
	}
	if newestName == "" {
		return executionPublication{}, false
	}
	digest := "sha256:" + newestName
	if _, err := a.loadPublicationTransfer(r.Handle.Ref, digest); err != nil {
		return executionPublication{}, false
	}
	heads, err := publishGit(ctx, transferDir, "bundle", "list-heads", filepath.Join(transferDir, newestName))
	if err != nil {
		return executionPublication{}, false
	}
	head, ok := strings.CutSuffix(heads, " HEAD")
	if !ok || !publicationCommit(head) || head == r.Published.Head || head == r.Handle.Commit {
		return executionPublication{}, false
	}
	return executionPublication{Head: head, BundleDigest: digest}, true
}

func (a *app) hasUnrecoveredPublication(r *state.ExecutionRecord) bool {
	if r == nil || r.StopRequest != nil || r.Released() {
		return false
	}
	if p := r.PendingPublication; p != nil && publicationCommit(p.Head) && p.BundleDigest != "" && p.BundleDigest != r.RecoveredTransfer {
		return r.Published == nil || r.Published.Head != p.Head
	}
	if r.Published == nil {
		return false
	}
	transferDir := filepath.Join(a.cfg.StateDir, "execution-publications", "transfers", string(r.Handle.Ref.Execution), fmt.Sprint(r.Handle.Ref.Generation))
	entries, err := os.ReadDir(transferDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && len(entry.Name()) == 64 && "sha256:"+entry.Name() != r.RecoveredTransfer {
			return true
		}
	}
	return false
}

func (a *app) recoverExecutionPublication(ctx context.Context, ref execution.Ref, auto bool) (executionPublicationResult, bool, error) {
	var result executionPublicationResult
	var found bool
	var publishErr error
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, ref)
		if r == nil {
			return nil, refused("execution is not owned by this coordinator")
		}
		p, ok := a.findStrandedPublication(ctx, r)
		if !ok {
			if r.PendingPublication != nil && r.Published != nil && r.Published.Head == r.PendingPublication.Head {
				r.PendingPublication = nil
			}
			if r.Published != nil {
				transferDir := filepath.Join(a.cfg.StateDir, "execution-publications", "transfers", string(ref.Execution), fmt.Sprint(ref.Generation))
				if entries, readErr := os.ReadDir(transferDir); readErr == nil {
					for _, e := range entries {
						if !e.IsDir() && len(e.Name()) == 64 {
							r.RecoveredTransfer = "sha256:" + e.Name()
						}
					}
				}
			}
			return nil, nil
		}
		if auto && p.BundleDigest == r.RecoveredTransfer {
			return nil, nil
		}
		found = true
		r.RecoveredTransfer = p.BundleDigest
		publishCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		result, publishErr = a.publishExecutionWithMode(publishCtx, st, ref, p, true)
		if publishErr != nil {
			return []state.Event{event(st.Starts[i].Party, "execution.publish-denied", "%s: %s", p.Head, publishErr)}, nil
		}
		r.Published = &state.ExecutionPublication{At: a.now.UTC(), Branch: result.Branch, Head: result.Head, URL: result.URL}
		r.PendingPublication = nil
		return []state.Event{event(st.Starts[i].Party, "execution.published", "%s at %s", result.URL, result.Head)}, nil
	})
	if err != nil {
		return result, found, err
	}
	return result, found, publishErr
}

func readPublicationBundle(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > publishBundleLimit {
		return nil, refused("commit bundle exceeds the publication transfer limit (64 MiB)")
	}
	return os.ReadFile(filepath.Clean(path))
}

func isShallowWorkspace(ctx context.Context, workspace string) bool {
	out, err := publishGit(ctx, workspace, "rev-parse", "--is-shallow-repository")
	ok, parseErr := strconv.ParseBool(out)
	return err == nil && parseErr == nil && ok
}

func tryUnshallowWorkspace(ctx context.Context, workspace string, revs ...string) {
	common, err := publishGit(ctx, workspace, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return
	}
	shallowPath := filepath.Join(common, "shallow")
	raw, err := os.ReadFile(filepath.Clean(shallowPath))
	if err != nil {
		return
	}
	if err := os.Remove(shallowPath); err != nil {
		return
	}
	args := append([]string{"rev-list", "--objects"}, revs...)
	if _, err := publishGit(ctx, workspace, args...); err != nil {
		_ = os.WriteFile(shallowPath, raw, 0o600) //nolint:gosec // shallow file inside the validated execution workspace git common dir
	}
}

func healShallowWorkspace(ctx context.Context, s *execution.Server, j execution.Journal, workspace string) {
	if !isShallowWorkspace(ctx, workspace) {
		if _, err := publishGit(ctx, workspace, "cat-file", "-e", j.Handle.Commit+"^{commit}"); err == nil {
			return
		}
	}
	sourceBundle := filepath.Join(s.Root, "sources", string(j.Handle.Ref.Execution), "source.bundle")
	if data, err := readPublicationBundle(sourceBundle); err == nil && (j.Request.SourceDigest == "" || execution.Digest(data) == j.Request.SourceDigest) {
		_, _ = publishGit(ctx, workspace, "fetch", "--no-tags", "--", sourceBundle, "refs/heads/execution-source")
	}
	tryUnshallowWorkspace(ctx, workspace, "HEAD", j.Handle.Commit)
}

func fetchSyncBaseBundle(ctx context.Context, s *execution.Server, j execution.Journal, workspace, bundlePath string, shallow bool) (executionPublicationResult, error) {
	var result executionPublicationResult
	p := executionPublication{Head: j.Handle.Commit, SyncBase: true, Shallow: shallow}
	co := s.Config.Coordinators[j.Request.Coordinator]
	if err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), executionPublish, j.Handle.Ref, p, j.Request.ControlKey, &result, s.Callback); err != nil {
		return result, err
	}
	if result.BundleSize > 0 {
		data, readErr := downloadPublication(ctx, s, j, result.BundleDigest, result.BundleSize)
		if readErr != nil {
			return result, readErr
		}
		if err := os.WriteFile(bundlePath, data, 0o600); err != nil {
			return result, err
		}
		heads, _ := publishGit(ctx, workspace, "bundle", "list-heads", bundlePath)
		if strings.Contains(heads, " refs/heads/source") {
			_, _ = publishGit(ctx, workspace, "fetch", "--no-tags", bundlePath, "refs/heads/source")
		}
		if _, err := publishGit(ctx, workspace, "fetch", "--no-tags", bundlePath, "refs/heads/base"); err != nil {
			return result, err
		}
		tryUnshallowWorkspace(ctx, workspace, "HEAD", j.Handle.Commit, "FETCH_HEAD")
	}
	return result, nil
}

func (a *app) executionPublishCmd() *cobra.Command {
	var title, bodyPath string
	var syncBase bool
	c := &cobra.Command{Use: "publish [agent-or-execution]", Short: "Push the owned SSH execution branch and create or recover its pull request", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			if a.managedExecutionID() != "" || syncBase || title != "" || bodyPath != "" {
				return usageErr("coordinator publication recovery accepts only <agent-or-execution>")
			}
			ref, err := a.executionByName(args[0])
			if err != nil {
				return err
			}
			result, ok, err := a.recoverExecutionPublication(cmd.Context(), ref, false)
			if err != nil {
				return err
			}
			if !ok {
				return refused("execution has no stranded publication transfer to recover")
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}
		s, j, err := a.executionCaller()
		if err != nil {
			return err
		}
		policy, err := s.InstalledPolicy()
		if err != nil || policy.Digest() != j.BoundPolicy() || j.StopRequest != nil || j.Completed || !execution.SupportedHarness(j.Request.Harness) {
			return refused("publication policy changed or execution is unavailable; after an approved host-policy update the coordinator rebinds this execution with agents recover")
		}
		ctx := cmd.Context()
		workspace := j.Handle.Workspace
		healShallowWorkspace(ctx, s, j, workspace)
		head, err := publishGit(ctx, workspace, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if cmd.Flags().Changed(publicationTitle) && strings.TrimSpace(title) == "" {
			return refused("--title is empty: a pull request needs a title, and nothing was published")
		}
		p := executionPublication{Head: head, Title: title, SyncBase: syncBase}
		if syncBase {
			if title != "" || bodyPath != "" {
				return refused("--sync-base cannot publish a PR")
			}
			// The pinned commit is known at the coordinator even before the first
			// publication. Include all upstream changes since that common ancestor.
			p.Head = j.Handle.Commit
		} else if bodyPath != "" {
			resolved, pathErr := filepath.EvalSymlinks(bodyPath)
			rel, relErr := filepath.Rel(workspace, resolved)
			if pathErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return refused("PR body must be a file in the owned workspace")
			}
			b, readErr := os.ReadFile(filepath.Clean(resolved))
			if readErr != nil {
				return readErr
			}
			// An empty file looks like no flag at the coordinator, which would
			// keep the old body without a word.
			if strings.TrimSpace(string(b)) == "" {
				return refused("--body-file is empty: a pull request's body cannot be cleared, and nothing was published")
			}
			p.Body = string(b)
		}
		dir, err := os.MkdirTemp(filepath.Join(workspace, ".beekeeper-scratch"), "publication-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		bundlePath := filepath.Join(dir, "commits.bundle")
		if syncBase {
			shallow := isShallowWorkspace(ctx, workspace)
			if _, err := publishGit(ctx, workspace, "cat-file", "-e", j.Handle.Commit+"^{commit}"); err != nil {
				shallow = true
			}
			result, syncErr := fetchSyncBaseBundle(ctx, s, j, workspace, bundlePath, shallow)
			if syncErr != nil && !shallow {
				result, syncErr = fetchSyncBaseBundle(ctx, s, j, workspace, bundlePath, true)
			}
			if syncErr != nil {
				return syncErr
			}
			if result.BundleSize > 0 {
				if err = mergePublicationBase(ctx, workspace); err != nil {
					return err
				}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}
		if _, err := publishGit(ctx, workspace, "merge-base", "--is-ancestor", j.Handle.Commit, "HEAD"); err != nil && isShallowWorkspace(ctx, workspace) {
			_, _ = fetchSyncBaseBundle(ctx, s, j, workspace, bundlePath, true)
		}
		if _, err = publishGit(ctx, workspace, "bundle", "create", bundlePath, "HEAD", "^"+j.Handle.Commit); err != nil {
			return err
		}
		data, readErr := readPublicationBundle(bundlePath)
		if readErr != nil {
			return readErr
		}
		p.BundleDigest, err = uploadPublication(ctx, s, j, data)
		if err != nil {
			return err
		}
		var result executionPublicationResult
		co := s.Config.Coordinators[j.Request.Coordinator]
		if err = execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), executionPublish, j.Handle.Ref, p, j.Request.ControlKey, &result, s.Callback); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	c.Flags().StringVar(&title, publicationTitle, "", "Conventional Commit title; on a republication it updates the existing pull request")
	c.Flags().StringVar(&bodyPath, "body-file", "", "PR body file inside the owned workspace; on a republication it updates the existing pull request")
	c.Flags().BoolVar(&syncBase, "sync-base", false, "fetch the default branch through the coordinator and merge it into the workspace")
	return c
}

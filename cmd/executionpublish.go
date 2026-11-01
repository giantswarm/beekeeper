package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const executionPublish = "publish"
const publishBundleLimit = 64 << 20

type executionPublication struct {
	Head         string `json:"head"`
	BundleDigest string `json:"bundleDigest,omitempty"`
	Title        string `json:"title,omitempty"`
	Body         string `json:"body,omitempty"`
	SyncBase     bool   `json:"syncBase,omitempty"`
}
type executionPublicationResult struct {
	Branch       string `json:"branch"`
	URL          string `json:"url,omitempty"`
	Head         string `json:"head"`
	BundleDigest string `json:"bundleDigest,omitempty"`
	BundleSize   int64  `json:"bundleSize,omitempty"`
}

func executionBranch(ref execution.Ref) string {
	return fmt.Sprintf("beekeeper-execution/%s/%d", ref.Execution, ref.Generation)
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
	prefix := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}
	c := exec.CommandContext(ctx, "git", append(prefix, args...)...) //nolint:gosec // fixed Git subcommands, validated commit/repository identities and component-owned paths; no shell
	c.Dir = dir
	c.Env = append(os.Environ(), identity...)
	c.Env = append(c.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := c.Output()
	if err != nil {
		return nil, errors.New("publication Git operation failed; inspect the execution publication before retrying")
	}
	return out, nil
}

var executionPublishGH = github.RunGH

func publishGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := executionPublishGit(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

func (a *app) executionPublicationLive(st *state.State, ref execution.Ref) (*state.ExecutionRecord, error) {
	r, _ := executionRecord(st, ref)
	if r == nil || r.Request.Harness != execution.HarnessClaude || !r.Request.LeaseRequired || r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()) || r.StopRequest != nil || r.Completed || r.Terminal() || st.Stop != nil {
		return nil, refused("publication requires a live, leased execution")
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

// Called while the coordinating state lock is held, serializing stop and lease
// revocation with a bounded synchronous write. A retry reconciles the remote
// branch and PR before mutating; push is always a normal fast-forward push.
func (a *app) publishExecution(ctx context.Context, st *state.State, ref execution.Ref, p executionPublication) (executionPublicationResult, error) {
	var result executionPublicationResult
	r, err := a.executionPublicationLive(st, ref)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithDeadline(ctx, r.Lease.ExpiresAt)
	defer cancel()
	if !publicationCommit(p.Head) || len(p.Title) > 256 || len(p.Body) > 64<<10 {
		return result, refused("invalid or oversized publication")
	}
	if p.SyncBase && (p.BundleDigest != "" || p.Title != "" || p.Body != "") {
		return result, refused("base synchronization cannot publish a PR")
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
	result.Branch = executionBranch(ref)
	if p.SyncBase {
		if _, err = publishGit(ctx, repo, "cat-file", "-e", p.Head+"^{commit}"); err != nil {
			return result, refused("base synchronization head is not in the default branch history")
		}
		result.Head, err = publishGit(ctx, repo, "rev-parse", "refs/heads/base")
		if err != nil {
			return result, err
		}
		if result.Head != p.Head {
			if _, err = publishGit(ctx, repo, "bundle", "create", bundlePath, "refs/heads/base", "^"+p.Head); err != nil {
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
			return result, err
		}
		if _, err = publishGit(ctx, repo, "fetch", "--no-tags", bundlePath, "HEAD:refs/heads/publication"); err != nil {
			return result, err
		}
		if _, err = publishGit(ctx, repo, "merge-base", "--is-ancestor", r.Handle.Commit, p.Head); err != nil {
			return result, refused("publication does not descend from the execution's pinned commit")
		}
		messages, messageErr := publishGit(ctx, repo, "log", "--no-patch", "--format=%B", r.Handle.Commit+".."+p.Head, "--")
		if messageErr != nil {
			return result, messageErr
		}
		patch, patchErr := publishGit(ctx, repo, "log", "-p", "--no-color", "--no-ext-diff", "--no-textconv", "-U0", "--format=", r.Handle.Commit+".."+p.Head, "--")
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
		// Reconcile a lost create response by the exact owned branch. An existing
		// PR is returned without editing a reviewer's title or discussion.
		raw, err = executionPublishGH(ctx, "pr", "list", "--repo", r.Request.Repository, "--head", result.Branch, "--state", "all", "--json", "url,state")
		if err != nil {
			return result, err
		}
		var pulls []struct {
			URL   string `json:"url"`
			State string `json:"state"`
		}
		if json.Unmarshal(raw, &pulls) != nil || len(pulls) > 1 {
			return result, errors.New("ambiguous owned pull request")
		}
		if len(pulls) == 1 && pulls[0].State != "OPEN" {
			return result, refused("owned pull request is already closed; a new execution is required")
		}
		if len(pulls) == 0 && (strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Body) == "") {
			return result, refused("first publication requires --title and --body-file")
		}
		if _, liveErr := a.executionPublicationLive(st, ref); liveErr != nil {
			return result, liveErr
		}
		if _, err = publishGit(ctx, repo, "push", remote, p.Head+":refs/heads/"+result.Branch); err != nil {
			return result, err
		}
		result.Head = p.Head
		if len(pulls) == 1 {
			result.URL = pulls[0].URL
		} else {
			bodyPath := filepath.Join(dir, "body.md")
			if err = os.WriteFile(bodyPath, []byte(p.Body), 0o600); err != nil {
				return result, err
			}
			if _, liveErr := a.executionPublicationLive(st, ref); liveErr != nil {
				return result, liveErr
			}
			raw, err = executionPublishGH(ctx, "pr", "create", "--repo", r.Request.Repository, "--base", base, "--head", result.Branch, "--title", p.Title, "--body-file", bodyPath)
			if err != nil {
				return result, errors.New("PR creation outcome uncertain; retry publication to reconcile the owned branch")
			}
			result.URL = strings.TrimSpace(string(raw))
		}
	}
	success = true
	return result, nil
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

func (a *app) executionPublishCmd() *cobra.Command {
	var title, bodyPath string
	var syncBase bool
	c := &cobra.Command{Use: "publish", Short: "Push the owned SSH execution branch and create or recover its pull request", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, j, err := a.executionCaller()
		if err != nil {
			return err
		}
		policy, err := s.InstalledPolicy()
		if err != nil || policy.Digest() != j.Request.PolicyDigest || j.StopRequest != nil || j.Completed || j.Request.Harness != execution.HarnessClaude {
			return refused("publication policy changed or execution is unavailable")
		}
		ctx := cmd.Context()
		workspace := j.Handle.Workspace
		head, err := publishGit(ctx, workspace, "rev-parse", "HEAD")
		if err != nil {
			return err
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
			path, pathErr := filepath.EvalSymlinks(bodyPath)
			rel, relErr := filepath.Rel(workspace, path)
			if pathErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return refused("PR body must be a file in the owned workspace")
			}
			b, readErr := os.ReadFile(filepath.Clean(path))
			if readErr != nil {
				return readErr
			}
			p.Body = string(b)
		}
		dir, err := os.MkdirTemp(filepath.Join(workspace, ".beekeeper-scratch"), "publication-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		bundlePath := filepath.Join(dir, "commits.bundle")
		if !syncBase {
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
		}
		var result executionPublicationResult
		co := s.Config.Coordinators[j.Request.Coordinator]
		if err = execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), executionPublish, j.Handle.Ref, p, j.Request.ControlKey, &result, s.Callback); err != nil {
			return err
		}
		if syncBase && result.BundleSize > 0 {
			data, readErr := downloadPublication(ctx, s, j, result.BundleDigest, result.BundleSize)
			if readErr != nil {
				return readErr
			}
			if err = os.WriteFile(bundlePath, data, 0o600); err != nil {
				return err
			}
			if _, err = publishGit(ctx, workspace, "fetch", "--no-tags", bundlePath, "refs/heads/base"); err != nil {
				return err
			}
			if err = mergePublicationBase(ctx, workspace); err != nil {
				return err
			}
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	c.Flags().StringVar(&title, "title", "", "Conventional Commit title for the first publication")
	c.Flags().StringVar(&bodyPath, "body-file", "", "PR body file inside the owned workspace")
	c.Flags().BoolVar(&syncBase, "sync-base", false, "fetch the default branch through the coordinator and merge it into the workspace")
	return c
}

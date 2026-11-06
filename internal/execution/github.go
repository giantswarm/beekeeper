package execution

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

const (
	workerGitHubPush        = "push"
	workerGitHubHeadPrefix  = "HEAD:refs/heads/"
	workerGitHubRepoFlag    = "--repo"
	workerGitHubBinDir      = "github-bin"
	workerGitHubClone       = "clone"
	workerGitHubHTTPSPrefix = "https://github.com/"

	GitHubOrigin      = "origin"
	GitHubGitTool     = "git"
	workerGitHubFetch = "fetch"
)

// GitHubBranch is the one upstream branch assigned to this execution.
func GitHubBranch(ref Ref) string {
	return fmt.Sprintf("beekeeper-execution/%s/%d", ref.Execution, ref.Generation)
}

// GitHubEnvironment installs only dispatch scripts, never tokens. The helper
// asks the coordinator on each command; no credential is saved on the host.
func (s *Server) GitHubEnvironment(j Journal, env []string) ([]string, error) {
	if !j.Request.GitHub {
		return env, nil
	}
	dir := filepath.Join(s.Root, workerGitHubBinDir, string(j.Handle.Ref.Execution))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }
	for _, name := range []string{"gh", GitHubGitTool} {
		verb := "github-cli"
		if name == GitHubGitTool {
			verb = "github-git"
		}
		// These commands disable Cobra flag parsing to preserve client flags.
		// A preceding --config would therefore leak into their client argv.
		body := []byte("#!/bin/sh\nBEEKEEPER_CONFIG=" + quote(s.ConfigPath) + " exec " + quote(s.Executable) + " agents " + verb + " -- \"$@\"\n")
		path := filepath.Join(dir, name)
		if err := atomicFile(path, body); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // executable dispatch script in the owned component directory
			return nil, err
		}
	}
	out := append([]string{}, env...)
	for i, kv := range out {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			out[i] = "PATH=" + dir + string(filepath.ListSeparator) + v
		}
	}
	return out, nil
}

// RevokeGitHub runs on normal exit and termination, independently of the
// runner's cancelled context. Coordinator stop/inspection also revoke, so
// losing the host cannot keep a worker's credentials alive.
func (s *Server) RevokeGitHub(j Journal) error {
	if !j.Request.GitHub {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	co := s.Config.Coordinators[j.Request.Coordinator]
	var result struct{}
	return CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), "github-revoke", j.Handle.Ref, struct{}{}, j.Request.ControlKey, &result, s.Callback)
}

const (
	workerGitHubIssueGroup = "issue"
	workerGitHubPRGroup    = "pr"

	workerGitHubRepoGroup     = "repo"
	workerGitHubRunGroup      = "run"
	workerGitHubWorkflowGroup = "workflow"
	workerGitHubReleaseGroup  = "release"

	workerGitHubList     = "list"
	workerGitHubLs       = "ls"
	workerGitHubView     = "view"
	workerGitHubStatus   = "status"
	workerGitHubDownload = "download"
	workerGitHubCreate   = "create"
	workerGitHubEdit     = "edit"
	workerGitHubComment  = "comment"
	workerGitHubClose    = "close"
	workerGitHubGet      = "GET"

	workerGitHubRepoPrefix  = "--repo="
	workerGitHubMissingRepo = "missing GitHub repository"

	workerGitHubMethodFlag   = "method"
	workerGitHubFieldFlag    = "field"
	workerGitHubRawFieldFlag = "raw-field"
	workerGitHubHeaderFlag   = "header"
)

var (
	workerGitHubReads = map[string][]string{
		workerGitHubIssueGroup:    {workerGitHubList, workerGitHubLs, workerGitHubView, workerGitHubStatus},
		workerGitHubPRGroup:       {workerGitHubList, workerGitHubLs, workerGitHubView, workerGitHubStatus, "checks", "diff", "checkout"},
		workerGitHubRepoGroup:     {workerGitHubList, workerGitHubLs, workerGitHubView, workerGitHubClone},
		workerGitHubRunGroup:      {workerGitHubList, workerGitHubLs, workerGitHubView, "watch", workerGitHubDownload},
		workerGitHubWorkflowGroup: {workerGitHubList, workerGitHubLs, workerGitHubView},
		workerGitHubReleaseGroup:  {workerGitHubList, workerGitHubLs, workerGitHubView, workerGitHubDownload, "verify", "verify-asset"},
	}
	// `gh pr close` stays out: its --delete-branch reaches branches a worker
	// cannot push to.
	workerGitHubWrites = map[string][]string{
		workerGitHubIssueGroup: {workerGitHubCreate, workerGitHubEdit, workerGitHubComment, workerGitHubClose, "reopen"},
		workerGitHubPRGroup:    {workerGitHubCreate, workerGitHubEdit, workerGitHubComment, "review"},
	}
	// gh api's flags and whether each takes a value.
	workerGitHubAPIFlags = map[string]bool{
		"allow-escape-sequences": false, "include": false, "paginate": false, "silent": false, "slurp": false, "verbose": false,
		"cache": true, workerGitHubFieldFlag: true, workerGitHubHeaderFlag: true, "jq": true, workerGitHubMethodFlag: true, "preview": true, workerGitHubRawFieldFlag: true, "template": true,
	}
	workerGitHubAPIShorthands = map[byte]string{'F': workerGitHubFieldFlag, 'H': workerGitHubHeaderFlag, 'X': workerGitHubMethodFlag, 'f': workerGitHubRawFieldFlag, 'i': "include", 'p': "preview", 'q': "jq", 't': "template"}
)

// GitHubRepository determines the scope of one gh invocation. Cross-owner
// calls must name their repository explicitly; global auth/search commands
// have no safely bounded repository scope.
func GitHubRepository(args []string, fallback string) (string, error) {
	repo := fallback
	for i, arg := range args {
		switch {
		case arg == workerGitHubRepoFlag || arg == "-R":
			if i+1 >= len(args) {
				return "", errors.New(workerGitHubMissingRepo)
			}
			repo = args[i+1]
		case strings.HasPrefix(arg, workerGitHubRepoPrefix):
			repo = strings.TrimPrefix(arg, workerGitHubRepoPrefix)
		case strings.HasPrefix(arg, workerGitHubHTTPSPrefix):
			parts := strings.Split(strings.TrimPrefix(arg, workerGitHubHTTPSPrefix), "/")
			if len(parts) >= 2 {
				repo = strings.Join(parts[:2], "/")
			}
		case strings.HasPrefix(arg, "repos/"):
			parts := strings.Split(arg, "/")
			if len(parts) >= 3 {
				repo = strings.Join(parts[1:3], "/")
			}
		}
	}
	repo = strings.ToLower(repo)
	if RepositoryIdentity(workerGitHubHTTPSPrefix+repo) != repo {
		return "", errors.New("invalid worker GitHub repository")
	}
	return repo, nil
}

// GitHubWriteRepository is the single repository a write acts on. gh prefers a
// URL target over --repo, so GitHubRepository's last-match reading could vouch
// for one owner while gh writes to another: every target must agree. Text that
// starts with a GitHub URL of another repository goes through --body-file.
func GitHubWriteRepository(args []string, fallback string) (string, error) {
	named := ""
	for i, arg := range args {
		repo := ""
		switch {
		case arg == workerGitHubRepoFlag || arg == "-R":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", errors.New(workerGitHubMissingRepo)
			}
			repo = args[i+1]
		case arg == workerGitHubRepoPrefix:
			return "", errors.New(workerGitHubMissingRepo)
		case strings.HasPrefix(arg, workerGitHubRepoPrefix):
			repo = strings.TrimPrefix(arg, workerGitHubRepoPrefix)
		case len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.Contains(arg, "R"):
			return "", errors.New("worker gh writes name their repository as --repo owner/repo")
		default:
			repo = gitHubURLRepository(arg)
		}
		if repo = strings.ToLower(repo); repo == "" {
			continue
		}
		if named != "" && repo != named {
			return "", errors.New("worker gh write names more than one repository")
		}
		named = repo
	}
	if named == "" {
		named = strings.ToLower(fallback)
	}
	if RepositoryIdentity(workerGitHubHTTPSPrefix+named) != named {
		return "", errors.New("invalid worker GitHub repository")
	}
	return named, nil
}

// gitHubURLRepository reads an argument as gh does: either scheme, any case,
// and subdomains that gh normalizes to github.com.
func gitHubURLRepository(arg string) string {
	u, err := url.Parse(arg)
	if err != nil || (u.Scheme != httpsScheme && u.Scheme != "http") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:2], "/")
}

// GitHubWrite classifies one gh subcommand. The token behind a read is as
// write-capable as any other, so a subcommand named in neither table, a later
// gh release's included, is refused rather than dispatched as a read.
func GitHubWrite(args []string) (bool, error) {
	if len(args) > 1 && slices.Contains(workerGitHubWrites[args[0]], args[1]) {
		return true, nil
	}
	if len(args) > 1 && slices.Contains(workerGitHubReads[args[0]], args[1]) {
		return false, nil
	}
	return false, errors.New("worker gh does not support this command: " + strings.Join(args[:min(len(args), 2)], " "))
}

// ValidateGitHubAPIRead parses the arguments after `gh api` the way gh does, so
// that only GET and HEAD reach the shared token. A field makes gh default to
// POST and stays a query parameter only under an explicit read method; a flag
// not listed here, --input and --hostname among them, is refused. GraphQL's POST
// passes only as a read that is literal in full (github.GraphQLRead).
func ValidateGitHubAPIRead(args []string) error {
	if github.GraphQLRead(args) {
		return nil
	}
	unsupported := errors.New("worker gh api supports reads only")
	endpoints, fields, method := 0, false, false
	check := func(name, value string) error {
		switch name {
		case workerGitHubMethodFlag:
			if !strings.EqualFold(value, workerGitHubGet) && !strings.EqualFold(value, "HEAD") {
				return unsupported
			}
			method = true
		case workerGitHubFieldFlag, workerGitHubRawFieldFlag:
			fields = true
		case workerGitHubHeaderFlag:
			if strings.Contains(strings.ToLower(value), "method-override") {
				return unsupported
			}
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			endpoints += len(args) - i - 1
			i = len(args)
		case strings.HasPrefix(arg, "--"):
			name, value, attached := strings.Cut(arg[2:], "=")
			takesValue, known := workerGitHubAPIFlags[name]
			if !known {
				return unsupported
			}
			if !takesValue {
				continue
			}
			if !attached {
				if i++; i >= len(args) {
					return unsupported
				}
				value = args[i]
			}
			if err := check(name, value); err != nil {
				return err
			}
		case len(arg) > 1 && arg[0] == '-':
			for j := 1; j < len(arg); j++ {
				name, known := workerGitHubAPIShorthands[arg[j]]
				if !known {
					return unsupported
				}
				if !workerGitHubAPIFlags[name] {
					continue
				}
				value := strings.TrimPrefix(arg[j+1:], "=")
				if value == "" {
					if i++; i >= len(args) {
						return unsupported
					}
					value = args[i]
				}
				if err := check(name, value); err != nil {
					return err
				}
				break
			}
		default:
			endpoints++
		}
	}
	if endpoints != 1 || (fields && !method) {
		return unsupported
	}
	return nil
}

func ValidateWorkerPush(args []string, branch string) error {
	// No implicit refspec, tags, deletion, alternate remote, force or options
	// that can widen the write. Ordinary fetch/status/commit remain regular git.
	if len(args) == 0 || args[0] != workerGitHubPush {
		return nil
	}
	rest := args[1:]
	if len(rest) > 0 && (rest[0] == "-u" || rest[0] == "--set-upstream") {
		rest = rest[1:]
	}
	if len(rest) != 2 || rest[0] != GitHubOrigin || rest[1] != workerGitHubHeadPrefix+branch {
		return errors.New("worker push requires origin HEAD:refs/heads/" + branch)
	}
	return nil
}

// GitHubGitVerb skips git's global options so unauthenticated local commands
// retain normal semantics. Network operations reject such overrides separately.
func GitHubGitVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "-C" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace" || arg == "--config-env" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

// GitHubReadRepository scopes explicit read transports to their named repository.
// Clone destinations and fetch refspecs stay client arguments, never authority.
func GitHubReadRepository(args []string) (string, []string, bool) {
	if len(args) == 0 || (args[0] != workerGitHubClone && args[0] != workerGitHubFetch && args[0] != "ls-remote" && args[0] != "pull") {
		return "", args, false
	}
	for i, arg := range args[1:] {
		if repo := RepositoryIdentity(arg); repo != "" {
			normalized := append([]string{}, args...)
			normalized[i+1] = workerGitHubHTTPSPrefix + repo + ".git"
			return repo, normalized, true
		}
	}
	return "", args, false
}

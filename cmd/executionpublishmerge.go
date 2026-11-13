package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
)

var publicationIdent = regexp.MustCompile(`^(.*) <([^<>\r\n]+)> [0-9]+ [+-][0-9]{4}$`)

// Query only Git's resolved author/committer metadata using the worker's normal
// configuration. git var performs no checkout, hooks, signing or network I/O.
// Copy the identities, not the global configuration, into the sanitized merge.
// A harness may run under a managed HOME of its own (agy does) that holds no
// Git configuration; the execution user's home, as the host names it, is the
// one whose global configuration carries the worker's identity. A host with
// none, or a workspace prepared before its identity was set, falls back to the
// one a prepared workspace carries (execution.WorkerGitName).
func publicationIdentity(ctx context.Context, workspace string) ([]string, error) {
	env := os.Environ()
	if home := os.Getenv(execution.EnvHome); filepath.IsAbs(home) {
		env = append(env, "HOME="+filepath.Clean(home))
	}
	var identity []string
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		c := proc.CommandContext(ctx, "git", "-c", "core.hooksPath="+os.DevNull, "-c", "core.fsmonitor=false", "var", "GIT_"+role+"_IDENT") //nolint:gosec // fixed git var identity queries; role is an internal enumeration, no shell
		c.Dir = workspace
		c.Env = env
		out, err := c.Output()
		if err != nil {
			identity = append(identity, "GIT_"+role+"_NAME="+execution.WorkerGitName, "GIT_"+role+"_EMAIL="+execution.WorkerGitEmail)
			continue
		}
		fields := publicationIdent.FindStringSubmatch(strings.TrimSpace(string(out)))
		if len(fields) != 3 || fields[1] == "" {
			return nil, errors.New("invalid worker Git commit identity for base merge")
		}
		identity = append(identity, "GIT_"+role+"_NAME="+fields[1], "GIT_"+role+"_EMAIL="+fields[2])
	}
	return identity, nil
}

func mergePublicationBase(ctx context.Context, workspace string) error {
	identity, err := publicationIdentity(ctx, workspace)
	if err != nil {
		return err
	}
	if _, err := runPublicationGit(ctx, workspace, identity, "merge", "--no-edit", "-m", "Merge upstream default branch", "FETCH_HEAD"); err != nil {
		conflicts, readErr := publishGit(ctx, workspace, "diff", "--name-only", "--diff-filter=U")
		if readErr == nil && conflicts != "" {
			return errors.New("base merge has conflicts in the owned workspace; resolve, commit and publish")
		}
		return errors.New("base merge failed in the owned workspace; inspect Git state before retrying")
	}
	return nil
}

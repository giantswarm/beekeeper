package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var publicationIdent = regexp.MustCompile(`^(.*) <([^<>\r\n]+)> [0-9]+ [+-][0-9]{4}$`)

// Query only Git's resolved author/committer metadata using the worker's normal
// configuration. git var performs no checkout, hooks, signing or network I/O.
// Copy the identities, not the global configuration, into the sanitized merge.
func publicationIdentity(ctx context.Context, workspace string) ([]string, error) {
	var identity []string
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		c := exec.CommandContext(ctx, "git", "-c", "core.hooksPath="+os.DevNull, "-c", "core.fsmonitor=false", "var", "GIT_"+role+"_IDENT") //nolint:gosec // fixed git var identity queries; role is an internal enumeration, no shell
		c.Dir = workspace
		out, err := c.Output()
		if err != nil {
			return nil, errors.New("cannot resolve worker Git commit identity for base merge; configure user.name and user.email")
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
	if _, err := runPublicationGit(ctx, workspace, identity, "merge", "--no-edit", "FETCH_HEAD"); err != nil {
		conflicts, readErr := publishGit(ctx, workspace, "diff", "--name-only", "--diff-filter=U")
		if readErr == nil && conflicts != "" {
			return errors.New("base merge has conflicts in the owned workspace; resolve, commit and publish")
		}
		return errors.New("base merge failed in the owned workspace; inspect Git state before retrying")
	}
	return nil
}

package cmd

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// sourceBehindCommits is how many default-branch commits a host source may
// lack before hosts check calls it too far behind: every start there ships
// them all in its source bundle.
const sourceBehindCommits = 1000

// hostSourceReports compares each mapped source on h with its repository's
// default branch on GitHub. A source behind still starts; only the start's
// bundle grows, so the report names a problem without failing the check.
func (a *app) hostSourceReports(ctx context.Context, h execution.Host) []execution.SourceReport {
	repos := make([]string, 0, len(h.Repositories))
	for repo := range h.Repositories {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	reader, ok := a.executionClient(h).(execution.SourceBasisReader)
	var out []execution.SourceReport
	for _, repo := range repos {
		report := execution.SourceReport{Repository: repo}
		switch basis, err := sourceBasis(ctx, reader, ok, h, repo); {
		case err != nil:
			report.Problem = "host source basis unavailable: " + err.Error()
		case len(basis.Commits) == 0:
			report.Problem = "host source has no branch to start from"
		default:
			report.Head = basis.Commits[0]
			report.Branch, report.Missing, err = sourceLag(ctx, repo, report.Head)
			switch {
			case err != nil:
				report.Problem = "cannot compare the host source with GitHub: " + err.Error()
			case report.Missing > sourceBehindCommits:
				report.Problem = fmt.Sprintf("host source lacks %d commits of %s; every start ships them, so bring the source up to date on the host", report.Missing, report.Branch)
			}
		}
		out = append(out, report)
	}
	return out
}

func sourceBasis(ctx context.Context, reader execution.SourceBasisReader, ok bool, h execution.Host, repo string) (execution.SourceBasis, error) {
	if !ok {
		return execution.SourceBasis{}, refused("execution host has no source basis route")
	}
	return reader.SourceBasis(ctx, execution.Ref{Host: h.Identity}, repo)
}

// sourceLag counts the default-branch commits head lacks.
func sourceLag(ctx context.Context, repo, head string) (string, int, error) {
	raw, err := executionPublishGH(ctx, "api", "repos/"+repo, "--jq", ".default_branch")
	if err != nil {
		return "", 0, err
	}
	branch := strings.TrimSpace(string(raw))
	if branch == "" {
		return "", 0, refused("repository has no default branch")
	}
	raw, err = executionPublishGH(ctx, "api", "repos/"+repo+"/compare/"+head+"..."+branch, "--jq", ".ahead_by")
	if err != nil {
		return branch, 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return branch, n, err
}

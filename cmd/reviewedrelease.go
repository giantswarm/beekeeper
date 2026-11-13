package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
)

const reviewedMergeRoute = "reviewed merge"

// The durable child emits the same release envelope as devctl pr merge.
// Keep the ordinary plain-squash document's legacy shape unchanged.
type reviewedMergeResult struct {
	github.SquashDoc
	Release *reviewedRelease `json:"release"`
}

type reviewedRelease struct {
	Verdict string `json:"verdict"`
	Tag     string `json:"tag,omitempty"`
}

var waitReviewedRelease = func(ctx context.Context, repo string, number int) ([]byte, error) {
	c := proc.CommandContext(ctx, merge.Tool, "release", "wait", repo, "--pr", fmt.Sprint(number)) //nolint:gosec // validated repository and PR, no shell
	c.Env = devctlCommandEnv("")
	c.Stderr = os.Stderr
	return c.Output()
}

// completeReviewedRelease never repeats the merge. Waiting in the mutation
// child saves the release verdict with its outcome even if the gate exits.
func completeReviewedRelease(ctx context.Context, doc github.SquashDoc, wait bool) reviewedMergeResult {
	result := reviewedMergeResult{SquashDoc: doc}
	if !wait || doc.MergeCommitSha == "" {
		return result
	}
	// The line marks the merge's release wait (mergeProgress), which a
	// gate called --until-merged leaves to this child.
	_, _ = fmt.Fprintf(os.Stderr, "release: waiting for the release of %s#%d, merged as %s (%s release wait %s --pr %d)\n",
		doc.Repo, doc.Number, shortSHA(doc.MergeCommitSha), merge.Tool, doc.Repo, doc.Number)
	ctx, cancel := context.WithTimeout(ctx, releasePRWait)
	defer cancel()
	raw, err := waitReviewedRelease(ctx, doc.Repo, doc.Number)
	out, reason, ok := merge.ParseReleaseWait(raw)
	result.Release = &reviewedRelease{Verdict: verdictUnconfirmed}
	switch {
	case !ok:
		result.ExitCode = 9
		result.Reason = "merged; release wait did not return a usable document"
		if err != nil {
			result.Reason += " (" + err.Error() + ")"
		}
	case out.NoRelease:
		result.Release.Verdict = "no_release"
		result.ExitCode = 0
	case out.ReleaseFailed:
		result.Release.Verdict = "ci_failed"
		result.Release.Tag = out.Release
		result.ExitCode = 6
	case out.Unconfirmed || out.Release == "":
		result.ExitCode = 9
	default:
		result.Release.Verdict = verdictAvailable
		result.Release.Tag = out.Release
		result.ExitCode = 0
	}
	if reason != "" {
		result.Reason = reason
	}
	return result
}

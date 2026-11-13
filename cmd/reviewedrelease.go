package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
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
	c := exec.CommandContext(ctx, merge.Tool, "release", "wait", repo, "--pr", fmt.Sprint(number)) //nolint:gosec // validated repository and PR, no shell
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
	ctx, cancel := context.WithTimeout(ctx, gate.ReleasePRWait)
	defer cancel()
	raw, err := waitReviewedRelease(ctx, doc.Repo, doc.Number)
	out, reason, ok := merge.ParseReleaseWait(raw)
	result.Release = &reviewedRelease{Verdict: gate.VerdictUnconfirmed}
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
		result.Release.Verdict = gate.VerdictAvailable
		result.Release.Tag = out.Release
		result.ExitCode = 0
	}
	if reason != "" {
		result.Reason = reason
	}
	return result
}

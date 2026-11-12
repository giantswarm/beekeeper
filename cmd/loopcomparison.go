package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/proc"
)

const (
	loopComparisonTimeout      = 2 * time.Minute
	loopContinuationTransition = "continuation"
)

// errDryRunIncomplete is a dry-run pass that could not observe the desk: its
// GitHub reads were held or missing, so its transitions say nothing.
var errDryRunIncomplete = errors.New("dry-run observations incomplete")

// Separate from the small offline canary probes: each full pass has its own
// deadline and neither process is allowed to mutate the desk.
var runLoopComparison = func(ctx context.Context, binary string, args ...string) (string, error) {
	out, err := proc.Command(ctx, binary, args...).CombinedOutput()
	return string(out), err
}

// loopPass is one binary's dry-run pass in a comparison: the transitions of a
// complete pass, else why it is not complete. A pass that ran out of its
// deadline or could not observe the desk is unobserved; any other error is
// failed.
type loopPass struct {
	counts map[string]int
	why    string
	failed bool
}

func (p loopPass) complete() bool { return p.counts != nil }

// compareLoopPasses runs the running release's and the candidate's read-only
// passes one after the other and refuses the candidate, retryably, when its
// pass fails, when it cannot observe the desk the running release just
// observed, or when it loses a kind of transition the running release plans.
// The running release's pass is only the baseline: one that cannot complete
// refuses nothing (a release whose own dry-run breaks must not hold back its
// fix), and the returned note says why the candidate goes uncompared.
func compareLoopPasses(ctx context.Context, current, candidate, config string) (string, error) {
	args := strings.Fields("supervisor loop --once --dry-run")
	if config != "" {
		args = append([]string{workerGitHubConfigOption, config}, args...)
	}
	run := func(binary string) (loopPass, error) {
		bounded, cancel := context.WithTimeout(ctx, loopComparisonTimeout)
		defer cancel()
		out, err := runLoopComparison(bounded, binary, args...)
		if ctx.Err() != nil {
			return loopPass{}, ctx.Err()
		}
		last := lastOf(out)
		switch {
		case bounded.Err() != nil || errors.Is(err, context.DeadlineExceeded):
			why := fmt.Sprintf("ran out of its %s", loopComparisonTimeout)
			if last != "" {
				why += " after: " + last
			}
			return loopPass{why: why}, nil
		case err != nil:
			why := err.Error()
			if last != "" {
				why += ": " + last
			}
			return loopPass{why: why, failed: !strings.Contains(out, errDryRunIncomplete.Error())}, nil
		}
		counts, err := loopTransitionCounts(out)
		if err != nil {
			return loopPass{why: err.Error(), failed: !errors.Is(err, errDryRunIncomplete)}, nil
		}
		return loopPass{counts: counts}, nil
	}
	before, err := run(current)
	if err != nil {
		return "", err
	}
	after, err := run(candidate)
	if err != nil {
		return "", err
	}
	switch {
	case after.failed:
		return "", fmt.Errorf("candidate dry-run unavailable (retryable): %s", after.why)
	case !before.complete():
		outcome := "the candidate's dry-run completed"
		if !after.complete() {
			outcome = "the candidate's dry-run did not complete either (" + after.why + ")"
		}
		return fmt.Sprintf("its dry-run gives no baseline (%s); %s", before.why, outcome), nil
	case !after.complete():
		return "", fmt.Errorf("candidate dry-run unavailable (retryable): %s; the running release's completed", after.why)
	}
	for _, kind := range []string{labStart, loopContinuationTransition, capacity.TaskKindReview, gitMerge} {
		if before.counts[kind] > 0 && after.counts[kind] == 0 {
			return "", fmt.Errorf("candidate dry-run lost %s transitions (running release: %d); retry or use --allow-loop-regression for an intentional change", kind, before.counts[kind])
		}
	}
	return "", nil
}

func loopTransitionCounts(output string) (map[string]int, error) {
	counts := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "SUPERVISOR GITHUB HELD") || strings.Contains(line, "FAILED") || strings.Contains(line, "UNREAD") || strings.Contains(line, "wait: admission or GitHub budget") {
			return nil, fmt.Errorf("%w: %s", errDryRunIncomplete, truncate(strings.TrimSpace(line), 200))
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "SUPERVISOR" || fields[1] != "WOULD" {
			continue
		}
		switch fields[2] {
		case "REVIEW":
			counts[capacity.TaskKindReview]++
		case "GO", "MERGE":
			counts[gitMerge]++
		case "CONTINUE", "RESUME":
			counts[loopContinuationTransition]++
		case "NEXT":
			if len(fields) >= 5 && fields[3] == "fill" {
				n, err := strconv.Atoi(fields[4])
				if err != nil {
					return nil, fmt.Errorf("invalid dry-run start count: %w", err)
				}
				counts[labStart] += n
			} else if len(fields) > 3 && fields[3] != "skip" && fields[3] != "wait:" {
				counts[labStart]++
			}
		}
	}
	return counts, nil
}

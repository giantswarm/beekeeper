package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

const (
	loopComparisonTimeout      = 90 * time.Second
	loopContinuationTransition = "continuation"
)

// Separate from the small offline canary probes: each full pass has its own
// deadline and neither process is allowed to mutate the desk.
var runLoopComparison = func(ctx context.Context, binary string, args ...string) (string, error) {
	out, err := proc.Command(ctx, binary, args...).CombinedOutput()
	return string(out), err
}

func compareLoopPasses(ctx context.Context, current, candidate, config string) error {
	args := strings.Fields("supervisor loop --once --dry-run")
	if config != "" {
		args = append([]string{workerGitHubConfigOption, config}, args...)
	}
	type result struct {
		counts map[string]int
		err    error
	}
	run := func(binary string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			bounded, cancel := context.WithTimeout(ctx, loopComparisonTimeout)
			defer cancel()
			out, err := runLoopComparison(bounded, binary, args...)
			if err == nil {
				err = bounded.Err()
			}
			if err != nil {
				ch <- result{err: err}
				return
			}
			counts, err := loopTransitionCounts(out)
			ch <- result{counts: counts, err: err}
		}()
		return ch
	}
	baseline, proposed := run(current), run(candidate)
	old, next := <-baseline, <-proposed
	if old.err != nil {
		return fmt.Errorf("running release dry-run unavailable (retryable): %w", old.err)
	}
	if next.err != nil {
		return fmt.Errorf("candidate dry-run unavailable (retryable): %w", next.err)
	}
	for _, kind := range []string{labStart, loopContinuationTransition, taskKindReview, gitMerge} {
		if old.counts[kind] > 0 && next.counts[kind] == 0 {
			return fmt.Errorf("candidate dry-run lost %s transitions (running release: %d); retry or use --allow-loop-regression for an intentional change", kind, old.counts[kind])
		}
	}
	return nil
}

func loopTransitionCounts(output string) (map[string]int, error) {
	counts := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "SUPERVISOR GITHUB HELD") || strings.Contains(line, "FAILED") || strings.Contains(line, "UNREAD") || strings.Contains(line, "wait: admission or GitHub budget") {
			return nil, fmt.Errorf("dry-run observations incomplete")
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "SUPERVISOR" || fields[1] != "WOULD" {
			continue
		}
		switch fields[2] {
		case "REVIEW":
			counts[taskKindReview]++
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

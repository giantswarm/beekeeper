package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// noChangeClosedEvent is the coordinator's close of an issue whose execution
// host's worker ended its task with no change.
const noChangeClosedEvent = "agents.no-change-closed"

// noChangeCloseTimeout bounds the reads and closes of closeNoChangeIssues.
const noChangeCloseTimeout = 15 * time.Second

// closeNoChangeIssues closes, for an execution host's worker that ends its
// task with --done --no-change, the open issues the task served: those of
// its serve records and of the deliveries it owns. The worker may not: its
// host refuses board move, and its gh may be a login that cannot close the
// issue. Refill offers an open issue again. The comment carries the worker's
// proof. A pull request, a closed issue and a reference GitHub does not
// answer are left as they are. A failed read or close is returned as a
// problem for the guide: the issue stays open, and the worker's outcome
// stands.
func (a *app) closeNoChangeIssues(ctx context.Context, st *state.State, ag state.Agent, me state.Party, proof string) ([]state.Event, []string) {
	var refs []github.PR
	add := func(ref string) {
		if r, ok := parseRef(ref); ok {
			r.Repo = strings.ToLower(r.Repo)
			if !slices.Contains(refs, r) {
				refs = append(refs, r)
			}
		}
	}
	for _, r := range st.Records {
		if r.Session.Is(me) {
			add(r.Issue)
		}
	}
	for _, i := range workerDeliveries(st, ag) {
		if d := st.Deliveries[i]; !slices.ContainsFunc(d.Pulls, func(p string) bool { return strings.EqualFold(p, d.Ref) }) {
			add(d.Ref)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	// The state is locked, and the host's call waits 30s for its receipt.
	ctx, cancel := context.WithTimeout(ctx, noChangeCloseTimeout)
	defer cancel()
	states, readErr := refStates(ctx, refs)
	comment := fmt.Sprintf("No change needed: %s\n\nClosed for the worker that ended this task with `agents idle --done --no-change`. Written by an agent (`beekeeper`).", strings.TrimSpace(proof))
	var events []state.Event
	var problems []string
	for _, r := range refs {
		url := issueRefURL(refName(r))
		s, read := states[r]
		switch {
		case !read && readErr != nil:
			problems = append(problems, fmt.Sprintf("%s stays open: %s ended its task with no change, and reading the issue to close it failed: %s", url, ag.Name, oneLine(readErr.Error())))
			continue
		case !read || s.Kind != github.RefIssue || s.State != github.Open:
			continue
		}
		if _, err := executionPublishGH(github.ActingOn(ctx, r.Repo), "issue", "close", url, "--repo", r.Repo, "--comment", comment); err != nil {
			problems = append(problems, fmt.Sprintf("%s stays open: %s ended its task with no change, and closing the issue failed: %s", url, ag.Name, oneLine(err.Error())))
			continue
		}
		events = append(events, event(me, noChangeClosedEvent, "%s: closed for %s, no change: %s", url, ag.Name, oneLine(proof)))
	}
	return events, problems
}

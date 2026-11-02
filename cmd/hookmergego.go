package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// A message copied into a worker prompt is not a merge grant. Read the loop's
// durable action, tied to both the exact head and its intended roster worker.
func recordedMergeGo(events []state.Event, worker state.Party, ref, head string) bool {
	allowed := false
	for _, e := range events {
		if e.By != loopParty {
			continue
		}
		key, rest, ok := keyed(e.Detail)
		if !ok {
			continue
		}

		switch e.Verb {
		case "supervisor.go":
			if strings.HasPrefix(key, "go "+ref+"@") {
				allowed = key == "go "+ref+"@"+head && strings.HasPrefix(rest, worker.Name+": ")
			}
		case "supervisor.merge":
			if strings.HasPrefix(key, "go "+ref+"@") {
				allowed = false
			}
		case "supervisor.review", "supervisor.rework", "supervisor.escalate":
			kind := strings.TrimPrefix(e.Verb, "supervisor.")
			if strings.HasPrefix(key, kind+" "+ref+"@") {
				allowed = false
			}
		case "supervisor.forward":
			if strings.HasPrefix(key, "forward "+ref+" ") {
				allowed = false
			}
		case "supervisor.end":
			if key == "end "+ref {
				allowed = false
			}
		case "supervisor.done":
			if key == "done "+ref+" "+workerRole {
				allowed = false
			}
			if target, ok := strings.CutPrefix(key, "done "+superloop.VerbDone+"@"); ok {
				if _, agent, ok := strings.Cut(target, " "); ok && agent == worker.Name {
					allowed = false
				}
			}
		}
	}
	return allowed
}

func (a *app) hookMergeGo(session, repo string, pr int, head string) string {
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return "merge authority unavailable: " + err.Error()
	}
	st, err := store.Read()
	if err != nil {
		return "merge authority unreadable: " + err.Error()
	}
	if st.Stop != nil {
		return "the desk is stopped"
	}
	caller := envParty(os.Getenv)
	caller.Session = session
	var worker state.Party
	var since time.Time
	for _, ag := range st.Agents {
		if sameReviewSession(ag.Party, caller) {
			worker = ag.Party
			since = ag.Registered
			if ag.AssignedAt.After(since) {
				since = ag.AssignedAt
			}
			break
		}
	}
	if worker.Name == "" {
		return "no registered worker identity for this merge; park for the supervisor"
	}
	if _, held := merge.BlockingFor(st, time.Now(), worker.Name, repo, pr, a.cfg.LaneOf(repo)); held {
		return "merge held by desk policy"
	}
	ref := fmt.Sprintf("%s#%d", repo, pr)
	events, err := store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") && !e.At.Before(since) })
	if err != nil {
		return "merge go log unreadable: " + err.Error()
	}
	if !recordedMergeGo(events, worker, ref, head) {
		return "no go from the supervisor recorded on disk for this worker, pull request and head; park for desk review"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pull, err := readLoopPull(ctx, ref)
	if err != nil {
		return "merge head and CI cannot be checked: " + err.Error()
	}
	if pull.State != superloop.Open || pull.Head != head || !pull.Green {
		return "merge head moved or current CI is not green; park for a new exact-head go"
	}
	if err := store.Log(about(event(worker, "merge.authorized", "https://github.com/%s/pull/%d at %s: supervisor go checked", repo, pr, head), ref)); err != nil {
		return "merge authorization could not be logged: " + err.Error()
	}
	return ""
}

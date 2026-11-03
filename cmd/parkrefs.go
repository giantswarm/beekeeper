package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// parkRefRetry bounds retries for issues and references not yet classified.
const parkRefRetry = 5 * time.Minute

// cacheParkRefs saves an attempt time and any resolved type to the park it read.
// A concurrent resume or new park must not inherit the old park's metadata.
func (a *app) cacheParkRefs(agents []state.Agent, refs map[github.PR]github.RefState) {
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, old := range agents {
			if old.Park == nil {
				continue
			}
			r, ok := parseRef(old.Park.On)
			if !ok {
				continue
			}
			read, ok := refs[r]
			if !ok || (read.Kind != "" && read.Kind != github.RefIssue && read.Kind != github.RefPullRequest) {
				continue
			}
			for _, current := range parkedAgents(st) {
				p := current.Park
				if !current.Is(old.Party) || p == nil || p.On != old.Park.On || !p.At.Equal(old.Park.At) {
					continue
				}
				if read.Kind != "" {
					p.RefKind = read.Kind
				}
				p.RefChecked = a.now.UTC()
			}
		}
		return nil, nil
	}); err != nil {
		_, _ = fmt.Fprintf(a.out, "cannot cache parked references: %v\n", err)
	}
}

// parkIsPull lazily migrates older parks before the supervisor reads any PR.
// GitHub reference types are immutable; an issue never needs another PR probe,
// including after a loop restart. Non-park review ledger entries already name PRs.
func (a *app) parkIsPull(ctx context.Context, st *state.State, ref string) bool {
	var parked []state.Agent
	for _, ag := range st.Agents {
		if ag.Park == nil || ag.Park.On != ref {
			continue
		}
		if ag.Park.RefKind != "" {
			return ag.Park.RefKind == github.RefPullRequest
		}
		parked = append(parked, ag)
	}
	if len(parked) == 0 {
		return true
	}
	r, ok := parseRef(ref)
	if !ok {
		return false
	}
	for _, ag := range parked {
		if !ag.Park.RefChecked.IsZero() && a.now.Sub(ag.Park.RefChecked) < parkRefRetry {
			return false
		}
	}
	// Record the attempt before GitHub: errors and missing references back
	// off across loop restarts, just as known issue parks do.
	a.cacheParkRefs(parked, map[github.PR]github.RefState{r: {}})
	reads, err := refStates(ctx, []github.PR{r})
	if err != nil {
		return false
	}
	a.cacheParkRefs(parked, reads)
	return reads[r].Kind == github.RefPullRequest
}

// readParkSuggestions returns this GitHub identity's open PRs linked to an issue.
var readParkSuggestions = func(ctx context.Context, r github.PR) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "pr", "list", "--repo", r.Repo, "--state", "open", "--author", "@me", "--search", "https://github.com/"+r.Repo+"/issues/"+strconv.Itoa(r.N), "--json", "url,closingIssuesReferences")
	if err != nil {
		return "", err
	}
	var pulls []struct {
		URL    string `json:"url"`
		Issues []struct {
			URL string `json:"url"`
		} `json:"closingIssuesReferences"`
	}
	if err := json.Unmarshal(out, &pulls); err != nil {
		return "", err
	}
	issue := "https://github.com/" + r.Repo + "/issues/" + strconv.Itoa(r.N)
	for _, p := range pulls {
		for _, i := range p.Issues {
			if i.URL == issue {
				return p.URL, nil
			}
		}
	}
	return "", nil
}

func (a *app) parkSuggestion(me state.Party, on string) string {
	st, err := a.store.Read()
	if err != nil {
		return ""
	}
	r, ok := parseRef(on)
	if !ok {
		return ""
	}
	serves := slices.ContainsFunc(st.Records, func(rec state.Record) bool {
		ref, ok := parseRef(rec.Issue)
		return rec.Session.Is(me) && ok && ref == r
	})
	if !serves {
		serves = slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
			return ag.Is(me) && slices.Contains(namedRefs(ag.Task, ""), strings.ToLower(refName(r)))
		})
	}
	if !serves {
		return ""
	}
	suggestion, _ := readParkSuggestions(context.Background(), r)
	return suggestion
}

// resolveParkRef refuses a target only when GitHub answers that it is missing.
// A failed read leaves the type empty for lazy classification after the park.
func resolveParkRef(ctx context.Context, on string) (string, error) {
	r, ok := parseRef(on)
	if !ok {
		return "", nil
	}
	reads, err := refStates(ctx, []github.PR{r})
	if err != nil {
		return "", nil
	}
	kind := reads[r].Kind
	if kind != github.RefIssue && kind != github.RefPullRequest {
		return "", refused("%s is not a readable issue or pull request", on)
	}
	return kind, nil
}

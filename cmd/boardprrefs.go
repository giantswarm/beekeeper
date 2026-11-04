package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A branch identifies the PR produced by a session, rather than every PR
// belonging to the GitHub account shared by multiple sessions.
func sessionPRReferences(ctx context.Context, gh github.GH, st *state.State, sessions []*claude.Session, me state.Party) (map[string]string, error) {
	out := map[string]string{}
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.Repo == "" || s.Branch == "" || endedOwner(st, s.Party()) {
			continue
		}
		key := strings.ToLower(s.Repo) + "\n" + s.Branch
		if seen[key] {
			continue
		}
		seen[key] = true
		raw, err := gh(github.ActingOn(ctx, s.Repo), "pr", "list", "--repo", s.Repo, "--head", s.Branch, "--state", "open", "--json", "number,state,title,body,closingIssuesReferences")
		if err != nil {
			return nil, fmt.Errorf("open PR references for running session: %w", err)
		}
		var prs []struct {
			Number             int
			State, Title, Body string
			Closing            []struct {
				Number     int
				Repository struct{ NameWithOwner string }
			} `json:"closingIssuesReferences"`
		}
		if err := json.Unmarshal(raw, &prs); err != nil {
			return nil, fmt.Errorf("open PR references: %w", err)
		}
		owner, _, _ := strings.Cut(s.Repo, "/")
		for _, pr := range prs {
			if pr.State != github.Open {
				continue
			}
			refs := namedRefs(pr.Title+"\n"+pr.Body, owner)
			// Bare issue numbers in PR prose belong to the PR's repository.
			for _, m := range prIssueNumber.FindAllStringSubmatch(pr.Title+"\n"+pr.Body, -1) {
				refs = append(refs, s.Repo+"#"+m[1])
			}
			for _, ref := range pr.Closing {
				refs = append(refs, fmt.Sprintf("%s#%d", ref.Repository.NameWithOwner, ref.Number))
			}
			for _, ref := range refs {
				ref = strings.ToLower(ref)
				if out[ref] == "" {
					out[ref] = fmt.Sprintf("referenced by open PR %s#%d of %s", s.Repo, pr.Number, partyLabel(s.Party(), me))
				}
			}
		}
	}
	return out, nil
}

var prIssueNumber = regexp.MustCompile(`(?:^|[\s(,:;])#([0-9]+)\b`)

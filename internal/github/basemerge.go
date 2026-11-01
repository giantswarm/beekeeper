package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// The compare API's caps on one answer: a comparison at a cap may hold more
// than it lists, so it proves nothing.
const (
	compareCommits = 250
	compareFiles   = 300
)

// compareAhead is a comparison whose head has every commit of its base.
const compareAhead = "ahead"

// comparison is the compare API's answer (repos/{repo}/compare/{a}...{b}).
type comparison struct {
	Status  string `json:"status"`
	AheadBy int    `json:"ahead_by"`
	Commits []struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	} `json:"commits"`
	Files []compareFile `json:"files"`
}

// compareFile is one file a comparison changes.
type compareFile struct {
	Name     string `json:"filename"`
	Previous string `json:"previous_filename"`
	Status   string `json:"status"`
	SHA      string `json:"sha"`
	Patch    string `json:"patch"`
}

// compare reads repo's comparison of from...to.
func compare(ctx context.Context, gh GH, repo, from, to string) (comparison, error) {
	var c comparison
	out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, from, to))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(out, &c); err != nil {
		return c, fmt.Errorf("the comparison %s...%s of %s: %w", from, to, repo, err)
	}
	return c, nil
}

// BaseMergeOnly reports whether head only merges base (the pull request's
// base branch) into approved, the head a review approved, and says why not:
//
//   - approved is an ancestor of head (no rebase, no force push),
//   - every commit head adds to approved is a merge, and
//   - the change head makes against its merge base with base is the one
//     approved makes against its own: the same files, each with the same
//     lines added and removed (context and positions may move with the
//     base; a file whose blob is unchanged needs no patch).
//
// A merge that resolved a conflict, carried a change of its own or merged
// another branch changes the change set and fails the last test; a
// comparison at the compare API's caps, or a changed file without a patch
// (binary, too large), proves nothing and fails too.
func BaseMergeOnly(ctx context.Context, gh GH, repo, base, approved, head string) (bool, string, error) {
	added, err := compare(ctx, gh, repo, approved, head)
	if err != nil {
		return false, "", err
	}
	switch {
	case added.Status != compareAhead:
		return false, fmt.Sprintf("%s is %s of the approved head %s, not ahead", short(head), added.Status, short(approved)), nil
	case added.AheadBy > len(added.Commits) || len(added.Commits) >= compareCommits:
		return false, fmt.Sprintf("%s adds %d commits to %s, more than the comparison lists", short(head), added.AheadBy, short(approved)), nil
	}
	for _, c := range added.Commits {
		if len(c.Parents) < 2 {
			return false, fmt.Sprintf("%s adds commit %s, which is no merge", short(head), short(c.SHA)), nil
		}
	}
	was, err := compare(ctx, gh, repo, base, approved)
	if err != nil {
		return false, "", err
	}
	now, err := compare(ctx, gh, repo, base, head)
	if err != nil {
		return false, "", err
	}
	if why := sameChange(was.Files, now.Files); why != "" {
		return false, fmt.Sprintf("%s changes %s other than the approved head %s: %s", short(head), base, short(approved), why), nil
	}
	return true, fmt.Sprintf("%s only merges %s into the approved head %s", short(head), base, short(approved)), nil
}

// sameChange says how two change sets against a base differ, "" when they
// are one.
func sameChange(was, now []compareFile) string {
	if len(was) >= compareFiles || len(now) >= compareFiles {
		return fmt.Sprintf("%d files, at the comparison's cap", max(len(was), len(now)))
	}
	if len(was) != len(now) {
		return fmt.Sprintf("%d changed files, not %d", len(now), len(was))
	}
	byName := map[string]compareFile{}
	for _, f := range was {
		byName[f.Name] = f
	}
	for _, f := range now {
		w, ok := byName[f.Name]
		switch {
		case !ok:
			return f.Name + " is a new change"
		case w.Status != f.Status || w.Previous != f.Previous:
			return f.Name + " is " + f.Status + ", not " + w.Status
		case w.SHA == f.SHA && w.SHA != "":
		case w.Patch == "" || f.Patch == "":
			return f.Name + " has no patch to compare"
		case !slices.Equal(changedLines(w.Patch), changedLines(f.Patch)):
			return f.Name + " adds or removes other lines"
		}
	}
	return ""
}

// changedLines are a patch's added and removed lines, in order: no hunk
// headers, no context.
func changedLines(patch string) []string {
	var out []string
	for l := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			out = append(out, l)
		}
	}
	return out
}

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

// The statuses of a comparison: its head has every commit of its base
// (ahead), is the base (identical), or each has commits of its own
// (diverged).
const (
	compareAhead     = "ahead"
	compareIdentical = "identical"
	compareDiverged  = "diverged"
)

// comparison is the compare API's answer (repos/{repo}/compare/{a}...{b}).
type comparison struct {
	Status    string `json:"status"`
	AheadBy   int    `json:"ahead_by"`
	MergeBase struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
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

// ApprovalCovers reports whether the approval of approved covers head, a
// later head of the pull request with base as its base branch, and says
// why or why not. It does when head
//
//   - only merges base into approved: approved is an ancestor of head (no
//     rebase, no force push), every commit head adds to approved is a
//     merge, and the change head makes against its merge base with base is
//     the one approved makes against its own: the same files, each with
//     the same hunks adding and removing the same lines (context and
//     positions may move with the base). A file without a patch (binary,
//     too large) passes only with the same blob in both and no change of
//     the base's to it between the two merge bases;
//   - resolves the conflicts of such a merge: as above, but every merge
//     has a parent the base branch holds, the pull request changes the
//     same files with the same statuses, and in each file whose change
//     differs from approved's every line it adds or removes beyond
//     approved's is a line approved's or the base's patch of the file
//     (between the two merge bases) adds or removes, the side the
//     resolution kept; or
//   - rebases approved onto base: head is no descendant of approved, and
//     its change against base is approved's, file by file as in the
//     first case, or differs only as a conflict's resolution does.
//
// A merge that carried a change of its own (a file, a line from neither
// side, any change to a file the base left alone or to one without a
// patch), merged another branch, or a commit that is no merge fails; a
// comparison at the compare API's caps proves nothing and fails too.
func ApprovalCovers(ctx context.Context, gh GH, repo, base, approved, head string) (bool, string, error) {
	added, err := compare(ctx, gh, repo, approved, head)
	if err != nil {
		return false, "", err
	}
	rebased := added.Status == compareDiverged
	switch {
	case added.Status != compareAhead && !rebased:
		return false, fmt.Sprintf("%s is %s of the approved head %s, not ahead", short(head), added.Status, short(approved)), nil
	case !rebased && (added.AheadBy > len(added.Commits) || len(added.Commits) >= compareCommits):
		return false, fmt.Sprintf("%s adds %d commits to %s, more than the comparison lists", short(head), added.AheadBy, short(approved)), nil
	}
	if !rebased {
		for _, c := range added.Commits {
			if len(c.Parents) < 2 {
				return false, fmt.Sprintf("%s adds commit %s, which is no merge", short(head), short(c.SHA)), nil
			}
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
	unpatched, differ, why := sameChange(was.Files, now.Files)
	if why != "" {
		return false, fmt.Sprintf("%s changes %s other than the approved head %s: %s", short(head), base, short(approved), why), nil
	}
	// moved is what the base changed between the two merge bases: a file
	// without a patch passes only untouched by it, and a conflict needs it.
	var moved []compareFile
	if (len(unpatched) > 0 || len(differ) > 0) && was.MergeBase.SHA != now.MergeBase.SHA {
		m, err := compare(ctx, gh, repo, was.MergeBase.SHA, now.MergeBase.SHA)
		if err != nil {
			return false, "", err
		}
		if len(m.Files) >= compareFiles {
			return false, fmt.Sprintf("%s changes %s other than the approved head %s: the base changed %d files, at the comparison's cap", short(head), base, short(approved), len(m.Files)), nil
		}
		moved = m.Files
	}
	if why := untouched(moved, unpatched); why != "" {
		return false, fmt.Sprintf("%s changes %s other than the approved head %s: %s", short(head), base, short(approved), why), nil
	}
	if len(differ) == 0 {
		if rebased {
			return true, fmt.Sprintf("%s rebases the approved head %s onto %s with the same change", short(head), short(approved), base), nil
		}
		return true, fmt.Sprintf("%s only merges %s into the approved head %s", short(head), base, short(approved)), nil
	}
	approvedBy := map[string]compareFile{}
	for _, f := range was.Files {
		approvedBy[f.Name] = f
	}
	headBy := map[string]compareFile{}
	for _, f := range now.Files {
		headBy[f.Name] = f
	}
	for _, name := range differ {
		if why := resolved(approvedBy[name], headBy[name], moved); why != "" {
			return false, fmt.Sprintf("%s changes %s other than the approved head %s: %s", short(head), base, short(approved), why), nil
		}
	}
	if rebased {
		return true, fmt.Sprintf("%s rebases the approved head %s onto %s, resolving its conflicts in %s", short(head), short(approved), base, strings.Join(differ, ", ")), nil
	}
	for _, c := range added.Commits {
		if ok, err := mergesBase(ctx, gh, repo, base, c.Parents); err != nil {
			return false, "", err
		} else if !ok {
			return false, fmt.Sprintf("%s adds merge %s, which merges no commit of %s", short(head), short(c.SHA), base), nil
		}
	}
	return true, fmt.Sprintf("%s merges %s into the approved head %s, resolving its conflicts in %s", short(head), base, short(approved), strings.Join(differ, ", ")), nil
}

// resolved says why head's change to a file is no resolution of a conflict
// between approved's change to it and the base's since (moved), "" when it
// is: every line head's patch adds or removes beyond approved's is a line
// approved's or the base's patch of the file adds or removes, whichever
// side the resolution kept. A file without a patch on either side, or one
// the base left alone, had no conflict to resolve.
func resolved(approved, head compareFile, moved []compareFile) string {
	i := slices.IndexFunc(moved, func(f compareFile) bool { return f.Name == head.Name || f.Previous == head.Name })
	switch {
	case i < 0:
		return head.Name + " adds or removes other lines, and the base did not change it since"
	case approved.Patch == "" || head.Patch == "" || moved[i].Patch == "":
		return head.Name + " has no patch to compare, and the base changed it since"
	}
	sides := map[string]bool{}
	left := map[string]int{}
	for _, l := range lineChanges(approved.Patch) {
		sides[l[1:]] = true
		left[l]++
	}
	for _, l := range lineChanges(moved[i].Patch) {
		sides[l[1:]] = true
	}
	for _, l := range lineChanges(head.Patch) {
		if left[l] > 0 {
			left[l]--
			continue
		}
		if !sides[l[1:]] {
			return fmt.Sprintf("%s adds or removes %q, a line neither the approved head nor the base changed", head.Name, strings.TrimSpace(l))
		}
	}
	return ""
}

// lineChanges are a patch's added and removed lines, each with its sign.
func lineChanges(patch string) []string {
	var out []string
	for _, l := range changedLines(patch) {
		if !strings.HasPrefix(l, "@@") {
			out = append(out, l)
		}
	}
	return out
}

// mergesBase reports whether one of a merge's parents is a commit the base
// branch holds.
func mergesBase(ctx context.Context, gh GH, repo, base string, parents []struct {
	SHA string `json:"sha"`
}) (bool, error) {
	for _, p := range parents {
		c, err := compare(ctx, gh, repo, p.SHA, base)
		if err != nil {
			return false, err
		}
		if c.Status == compareAhead || c.Status == compareIdentical {
			return true, nil
		}
	}
	return false, nil
}

// sameChange says how two change sets against a base differ in their
// files, "" when they change the same files with the same statuses;
// unpatched are the files without a patch whose blob is the same in both,
// one only while the base left them alone between the two merge bases
// (untouched), differ the files whose patches add or remove other lines.
func sameChange(was, now []compareFile) (unpatched, differ []string, why string) {
	if len(was) >= compareFiles || len(now) >= compareFiles {
		return nil, nil, fmt.Sprintf("%d files, at the comparison's cap", max(len(was), len(now)))
	}
	if len(was) != len(now) {
		return nil, nil, fmt.Sprintf("%d changed files, not %d", len(now), len(was))
	}
	byName := map[string]compareFile{}
	for _, f := range was {
		byName[f.Name] = f
	}
	for _, f := range now {
		w, ok := byName[f.Name]
		switch {
		case !ok:
			return nil, nil, f.Name + " is a new change"
		case w.Status != f.Status || w.Previous != f.Previous:
			return nil, nil, f.Name + " is " + f.Status + ", not " + w.Status
		case w.Patch == "" && f.Patch == "" && w.SHA == f.SHA && w.SHA != "":
			unpatched = append(unpatched, f.Name)
		case w.Patch == "" || f.Patch == "" || !slices.Equal(changedLines(w.Patch), changedLines(f.Patch)):
			differ = append(differ, f.Name)
		}
	}
	return unpatched, differ, ""
}

// untouched says which of names the base changed between the two merge
// bases (moved), "" when it changed none.
func untouched(moved []compareFile, names []string) string {
	for _, f := range moved {
		if slices.Contains(names, f.Name) || f.Previous != "" && slices.Contains(names, f.Previous) {
			return f.Name + " has no patch, and the base changed it since the approved head"
		}
	}
	return ""
}

// changedLines are a patch's hunks: each one's line counts, without its
// positions, then its added and removed lines in order, without context.
func changedLines(patch string) []string {
	var out []string
	for l := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			out = append(out, hunkCounts(l))
		case strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-"):
			out = append(out, l)
		}
	}
	return out
}

// hunkCounts is a hunk header ("@@ -12,4 +12,5 @@ func x") as its line
// counts alone ("@@ -4 +5"); a count left out is 1.
func hunkCounts(header string) string {
	f := strings.Fields(header)
	if len(f) < 3 {
		return header
	}
	count := func(r string) string {
		if _, n, ok := strings.Cut(r[1:], ","); ok {
			return n
		}
		return "1"
	}
	return "@@ -" + count(f[1]) + " +" + count(f[2])
}

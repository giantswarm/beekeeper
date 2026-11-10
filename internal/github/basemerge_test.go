package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	bmRepo             = "giantswarm/beekeeper"
	bmApproved         = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bmHead             = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bmMain             = "cccccccccccccccccccccccccccccccccccccccc"
	bmFork             = "dddddddddddddddddddddddddddddddddddddddd"
	modified           = "modified"
	statusKey          = "status"
	png                = "c.png"
	otherLines         = "a.go adds or removes other lines"
	aGo                = "a.go"
	bGo                = "b.go"
	dGo                = "d.go"
	statusAdded        = "added"
	resolvingConflictA = "resolving its conflicts in a.go"
)

// bmCommit is a commit of a comparison with its parents.
type bmCommit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

func commitOf(sha string, parents ...string) bmCommit {
	c := bmCommit{SHA: sha}
	for _, p := range parents {
		c.Parents = append(c.Parents, struct {
			SHA string `json:"sha"`
		}{p})
	}
	return c
}

// commitsOf is a comparison status ahead by commits it lists.
func commitsOf(status string, by int, commits ...bmCommit) map[string]any {
	return map[string]any{statusKey: status, "ahead_by": by, "commits": commits}
}

// filesOf is a comparison from mergeBase listing files.
func filesOf(mergeBase string, files []compareFile) map[string]any {
	return map[string]any{statusKey: compareAhead, "merge_base_commit": map[string]string{"sha": mergeBase}, "files": files}
}

// compareAnswers answers the compare API's calls by their from...to.
type compareAnswers map[string]any

func (c compareAnswers) gh(t *testing.T) GH {
	return func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		ref, ok := strings.CutPrefix(call, "api repos/"+bmRepo+"/compare/")
		a, known := c[ref]
		if !ok || !known {
			t.Fatalf("unexpected gh %s", call)
		}
		return json.Marshal(a)
	}
}

// approvedChange is the change the approved head makes against main: one
// line in a.go, b.go untouched since, and a binary c.png.
func approvedChange() []compareFile {
	return []compareFile{
		{Name: aGo, Status: modified, SHA: "a1", Patch: "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx"},
		{Name: bGo, Status: statusAdded, SHA: "b1", Patch: "@@ -0,0 +1 @@\n+b"},
		{Name: png, Status: modified, SHA: "c1"},
	}
}

func TestApprovalCovers(t *testing.T) {
	merge := commitsOf(compareAhead, 1, commitOf(bmHead, bmApproved, bmMain))
	// main moved a.go's context and line numbers: the change is the same.
	moved := approvedChange()
	moved[0].SHA, moved[0].Patch = "a2", "@@ -10,3 +10,3 @@\n other ctx\n-old\n+new\n ctx"
	// main changed a.go's first line to "newer" and line 50 from x to y;
	// resolved is a.go after the conflict on its first line, the approved
	// "new" kept against main's "newer".
	mainChangedA := []compareFile{{Name: aGo, Status: modified, SHA: "a9", Patch: "@@ -1,3 +1,3 @@\n ctx\n-old\n+newer\n ctx\n@@ -50,3 +50,3 @@\n ctx\n-x\n+y\n ctx"}}
	resolved := func() []compareFile {
		f := approvedChange()
		f[0].SHA, f[0].Patch = "a3", "@@ -1,3 +1,3 @@\n ctx\n-newer\n+new\n ctx"
		return f
	}
	// unreviewed is a.go with a line of neither side, added by the merge.
	unreviewed := func() []compareFile {
		f := approvedChange()
		f[0].SHA, f[0].Patch = "a5", "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx\n@@ -200,0 +200,1 @@\n+func unreviewed() { os.RemoveAll(\"/\") }"
		return f
	}
	for name, c := range map[string]struct {
		added map[string]any
		was   []compareFile
		now   []compareFile
		// base is what main changed between the approved head's merge
		// base (bmFork) and the head's (bmMain); nil: z.go alone.
		base []compareFile
		// holds is whether main holds the merged parent bmMain.
		holds bool
		// own are the commits of head past main beyond the approved
		// head's (nil: the ones added lists), ownBy how many there are
		// (0: as many as own lists).
		own   []bmCommit
		ownBy int
		want  bool
		why   string
	}{
		"a merge of main alone":                                 {added: merge, now: moved, want: true, why: "only merges main"},
		"a merge of main and an unchanged blob without a patch": {added: merge, now: approvedChange(), want: true},
		"a merge resolving a conflict with main's change": {
			added: merge, now: resolved(), base: mainChangedA, holds: true,
			want: true, why: resolvingConflictA,
		},
		// The worker kept the approved a.go (git checkout --ours): a
		// conflict resolved its way.
		"a merge that keeps its side of a conflict": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[0].Patch = "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx\n@@ -50,3 +50,3 @@\n ctx\n-y\n+x\n ctx"
				return f
			}(),
			base: mainChangedA, holds: true, want: true, why: resolvingConflictA,
		},
		"a resolved conflict of a merge of another branch": {
			added: merge, now: resolved(), base: mainChangedA,
			why: "which merges no commit of main",
		},
		"a binary main changed, kept at the approved blob": {
			added: merge, now: approvedChange(),
			base: []compareFile{{Name: png, Status: modified, SHA: "c9"}},
			why:  "c.png has no patch, and the base changed it",
		},
		"a binary main changed under its old name": {
			added: merge, now: approvedChange(),
			base: []compareFile{{Name: "d.png", Previous: png, Status: "renamed", SHA: "c9"}},
			why:  "d.png has no patch, and the base changed it",
		},
		"main changing more files than the comparison lists": {
			added: merge, now: approvedChange(), base: make([]compareFile, compareFiles),
			why: "at the comparison's cap",
		},
		// The same lines added, moved into another hunk of a file main
		// left alone: no conflict did that.
		"an added line moved into another hunk": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[0].SHA, f[0].Patch = "a4", "@@ -1,2 +1,1 @@\n ctx\n-old\n@@ -40,2 +39,3 @@\n ctx\n+new\n ctx"
				return f
			}(),
			why: otherLines + ", and the base did not change it",
		},
		"a commit of its own": {
			added: commitsOf(compareAhead, 2, commitOf(bmMain, bmApproved), commitOf(bmHead, bmMain, bmMain)),
			now:   moved, why: "which is no merge",
		},
		// A merge of main lists main's own commits since the approved
		// head's merge base too: they are main's, not the pull request's.
		"a merge of main bringing main's own commits": {
			added: commitsOf(compareAhead, 2, commitOf(bmMain, bmFork), commitOf(bmHead, bmApproved, bmMain)),
			own:   []bmCommit{commitOf(bmHead, bmApproved, bmMain)},
			now:   moved, want: true, why: "only merges main",
		},
		"a merge of main with a commit of its own after it": {
			added: commitsOf(compareAhead, 3, commitOf(bmMain, bmFork), commitOf(bmFork, bmApproved, bmMain), commitOf(bmHead, bmFork)),
			own:   []bmCommit{commitOf(bmFork, bmApproved, bmMain), commitOf(bmHead, bmFork)},
			now:   moved, why: "adds commit bbbbbbb, which is no merge",
		},
		"a rebase onto main": {added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)), now: moved, want: true, why: "rebases the approved head"},
		"a rebase resolving a conflict with main's change": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)), now: resolved(), base: mainChangedA,
			want: true, why: "rebases the approved head aaaaaaa onto main, resolving its conflicts in a.go",
		},
		"a rebase resolving a conflict by keeping the union of both sides' lines": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)),
			now: append([]compareFile{
				{Name: aGo, Status: modified, SHA: "a3", Patch: "@@ -1,3 +1,4 @@\n ctx\n newer\n+new\n ctx"},
			}, approvedChange()[1:]...),
			base: mainChangedA,
			want: true, why: resolvingConflictA,
		},
		"a rebase resolving a conflict by combining both sides into a new line": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)),
			was: append([]compareFile{
				{Name: aGo, Status: modified, SHA: "a1", Patch: "@@ -1,3 +1,3 @@\n ctx\n-\tif _, err := s.git(ctx, \"\", \"clone\", \"--no-local\", source, workspace); err != nil {\n+\tif _, err := s.git(ctx, \"\", \"clone\", \"--no-local\", cloneSource, workspace); err != nil {\n ctx"},
			}, approvedChange()[1:]...),
			now: append([]compareFile{
				{Name: aGo, Status: modified, SHA: "a3", Patch: "@@ -1,3 +1,3 @@\n ctx\n-\tif _, err := s.git(ctx, \"\", \"clone\", \"--bare\", \"--no-local\", source, repository); err != nil {\n+\tif _, err := s.git(ctx, \"\", \"clone\", \"--bare\", \"--no-local\", cloneSource, repository); err != nil {\n ctx"},
			}, approvedChange()[1:]...),
			base: []compareFile{
				{Name: aGo, Status: modified, SHA: "a9", Patch: "@@ -1,3 +1,3 @@\n ctx\n-\tif _, err := s.git(ctx, \"\", \"clone\", \"--no-local\", source, workspace); err != nil {\n+\tif _, err := s.git(ctx, \"\", \"clone\", \"--bare\", \"--no-local\", source, repository); err != nil {\n ctx"},
			},
			want: false, why: "a line neither the approved head nor the base changed",
		},
		"a rebase resolving a conflict by keeping main's version of a file": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)),
			now:   approvedChange()[1:],
			base:  mainChangedA,
			want:  true, why: resolvingConflictA,
		},
		"a rebase dropping an unconflicted hunk when keeping main's version of a file": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)),
			was: append([]compareFile{
				{Name: aGo, Status: modified, SHA: "a1", Patch: "@@ -1,3 +1,3 @@\n ctx\n-old\n+new\n ctx\n@@ -20,3 +20,3 @@\n ctx\n-guard\n+strictGuard\n ctx"},
			}, approvedChange()[1:]...),
			now:  approvedChange()[1:],
			base: mainChangedA,
			want: false, why: `a.go drops "-guard", a line the base did not change`,
		},
		"a rebase with another change": {
			added: commitsOf(compareDiverged, 1, commitOf(bmHead, bmMain)), now: unreviewed(), base: mainChangedA,
			why: "a line neither the approved head nor the base changed",
		},
		"a merge adding a line of neither side to a file main changed": {
			added: merge, now: unreviewed(), base: mainChangedA, holds: true,
			why: `a.go adds or removes "+func unreviewed() { os.RemoveAll(\"/\") }", a line neither`,
		},
		"a binary changed by a merge with main's change to it": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[2].SHA = "c2"
				return f
			}(),
			base: []compareFile{{Name: png, Status: modified, SHA: "c9"}}, holds: true,
			why: "c.png has no patch to compare, and the base changed it",
		},
		"a head behind the approved one":          {added: commitsOf("behind", 0), now: moved, why: "behind"},
		"more commits than listed":                {added: merge, ownBy: 3, now: moved, why: "more than the comparison lists"},
		"a conflict resolved in a file main kept": {added: merge, now: resolved(), why: otherLines + ", and the base did not change it"},
		"a change the merge carries":              {added: merge, now: append(approvedChange(), compareFile{Name: dGo, Status: statusAdded, SHA: "d1", Patch: "+d"}), why: "4 changed files, not 3"},
		"a binary the merge changed": {
			added: merge,
			now: func() []compareFile {
				f := approvedChange()
				f[2].SHA = "c2"
				return f
			}(),
			why: "c.png adds or removes other lines",
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := c.base
			if base == nil {
				base = []compareFile{{Name: "z.go", Status: modified, SHA: "z1", Patch: "+z"}}
			}
			holds := commitsOf(compareDiverged, 0)
			if c.holds {
				holds = commitsOf(compareIdentical, 0)
			}
			own := c.own
			if own == nil {
				own, _ = c.added["commits"].([]bmCommit)
			}
			wasFiles := c.was
			if wasFiles == nil {
				wasFiles = approvedChange()
			}
			approvedCommit := commitOf(bmApproved, bmFork)
			was := filesOf(bmFork, wasFiles)
			was["commits"], was["ahead_by"] = []bmCommit{approvedCommit}, 1
			now := filesOf(bmMain, c.now)
			now["commits"], now["ahead_by"] = append([]bmCommit{approvedCommit}, own...), 1+len(own)
			if c.ownBy > 0 {
				now["ahead_by"] = 1 + c.ownBy
			}
			gh := compareAnswers{
				bmApproved + "..." + bmHead: c.added,
				"main..." + bmApproved:      was,
				"main..." + bmHead:          now,
				bmFork + "..." + bmMain:     filesOf("", base),
				bmApproved + "...main":      commitsOf(compareDiverged, 0),
				bmMain + "...main":          holds,
			}.gh(t)
			got, why, err := ApprovalCovers(context.Background(), gh, bmRepo, "main", bmApproved, bmHead)
			if err != nil || got != c.want || !strings.Contains(why, c.why) {
				t.Errorf("ApprovalCovers = %v, %q, %v; want %v with %q", got, why, err, c.want, c.why)
			}
		})
	}
}

func TestHunkCounts(t *testing.T) {
	for header, want := range map[string]string{
		"@@ -12,4 +12,5 @@ func x()": "@@ -4 +5",
		"@@ -0,0 +1 @@":              "@@ -0 +1",
		"@@ -3 +3 @@":                "@@ -1 +1",
	} {
		if got := hunkCounts(header); got != want {
			t.Errorf("hunkCounts(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestSameChangeAtTheCapProvesNothing(t *testing.T) {
	files := make([]compareFile, compareFiles)
	for i := range files {
		files[i] = compareFile{Name: string(rune('a'+i%26)) + strings.Repeat("x", i), Status: modified, SHA: "s"}
	}
	if _, _, why := sameChange(files, files); !strings.Contains(why, "cap") {
		t.Errorf("sameChange at the cap = %q", why)
	}
}

func TestResolvedPreservesEdits(t *testing.T) {
	const (
		originalGuard   = "return enabled && checked"
		authorizedGuard = "return enabled && authorized"
		auditedGuard    = "return enabled && audited"
	)
	patch := func(before, after string) compareFile {
		return compareFile{Name: aGo, Patch: "@@ -1 +1 @@\n-" + before + "\n+" + after}
	}
	for name, tc := range map[string]struct {
		original, approved, base, head string
		want                           bool
	}{
		"dropped guards": {
			originalGuard, authorizedGuard,
			auditedGuard, "return enabled", false,
		},
		"invented guard combination": {
			originalGuard, authorizedGuard,
			auditedGuard, "return enabled && authorized && audited", false,
		},
		"intra-line edit splice rejected": {
			originalGuard, "return active && checked",
			auditedGuard, "return active && audited", false,
		},
		"disjoint edit partially dropped": {
			originalGuard, "return active && checked",
			auditedGuard, "return active && audit", false,
		},
		"literal whitespace changed": {
			`return "a b" + old`, `return "a b" + approved`,
			`return "a b" + audited`, `return "ab" + approved`, false,
		},
		"approved side retained": {
			originalGuard, authorizedGuard,
			auditedGuard, authorizedGuard, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			why := resolved(patch(tc.original, tc.approved), patch(tc.base, tc.head), []compareFile{patch(tc.original, tc.base)})
			if (why == "") != tc.want {
				t.Fatalf("resolved rejection = %q, want acceptance %v", why, tc.want)
			}
		})
	}
	// Union of both sides' lines is accepted; dropping an unconflicted hunk is rejected.
	unionHead := compareFile{Name: aGo, Patch: "@@ -1 +1,2 @@\n-" + auditedGuard + "\n+" + auditedGuard + "\n+" + authorizedGuard}
	if why := resolved(patch(originalGuard, authorizedGuard), unionHead, []compareFile{patch(originalGuard, auditedGuard)}); why != "" {
		t.Fatalf("union of both sides' lines rejected: %q", why)
	}
}

func TestResolvedDropsOnlyItsConflict(t *testing.T) {
	approved := compareFile{Name: aGo, Patch: "@@ -1 +1 @@\n-modeOld\n+modeApproved\n@@ -20,0 +21,3 @@\n+if !authorized {\n+ return denied\n+}"}
	base := []compareFile{{Name: aGo, Patch: "@@ -1 +1 @@\n-modeOld\n+modeBase"}}
	// Keeping the base's mode line drops the approved one with it, but the
	// guard the approved head inserts away from the conflict must stay.
	for name, tc := range map[string]struct {
		head compareFile
		why  string
	}{
		"an unconflicted insertion dropped with the file": {head: compareFile{}, why: `drops "+if !authorized {", a line outside its conflict`},
		"an unconflicted insertion dropped":               {head: compareFile{Name: aGo, Patch: "@@ -1,3 +1,3 @@\n ctx\n ctx\n ctx"}, why: "outside its conflict"},
		"the base's side kept with the guard":             {head: compareFile{Name: aGo, Patch: "@@ -20,0 +21,3 @@\n+if !authorized {\n+ return denied\n+}"}},
	} {
		t.Run(name, func(t *testing.T) {
			if why := resolved(approved, tc.head, base); tc.why == "" && why != "" || !strings.Contains(why, tc.why) {
				t.Errorf("resolved = %q, want %q", why, tc.why)
			}
		})
	}
	if got := changeBlocks(approved.Patch); len(got) != 2 || got[0].start != 1 || got[0].end != 2 || got[1].start != 21 || got[1].end != 21 {
		t.Errorf("changeBlocks = %+v", got)
	}
}

func TestBaseMergeCoversOnlyUnchangedBaseMerges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		parents []string
		change  bool
		want    bool
	}{
		{name: "base merge", status: compareAhead, parents: []string{bmApproved, bmMain}, want: true},
		{name: "other branch", status: compareAhead, parents: []string{bmApproved, bmFork}},
		{name: "ordinary commit", status: compareAhead, parents: []string{bmApproved}},
		{name: "rebase", status: compareDiverged, parents: []string{bmMain}},
		{name: "changed patch", status: compareAhead, parents: []string{bmApproved, bmMain}, change: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was := approvedChange()
			now := approvedChange()
			if tc.change {
				now[0].Patch = "@@ -1,3 +1,3 @@\n ctx\n-old\n+different\n ctx"
			}
			old := filesOf(bmFork, was)
			old["commits"] = []bmCommit{commitOf(bmApproved, bmFork)}
			current := filesOf(bmFork, now)
			current["commits"] = []bmCommit{commitOf(bmApproved, bmFork), commitOf(bmHead, tc.parents...)}
			answers := compareAnswers{
				bmApproved + "..." + bmHead: commitsOf(tc.status, 1, commitOf(bmHead, tc.parents...)),
				"main..." + bmApproved:      old,
				"main..." + bmHead:          current,
				bmApproved + "...main":      commitsOf(compareDiverged, 1),
				bmMain + "...main":          commitsOf(compareIdentical, 0),
				bmFork + "...main":          commitsOf(compareDiverged, 1),
			}
			ok, why, err := BaseMergeCovers(t.Context(), answers.gh(t), bmRepo, "main", bmApproved, bmHead)
			if err != nil || ok != tc.want {
				t.Fatalf("covered=%v why=%q err=%v", ok, why, err)
			}
		})
	}
}

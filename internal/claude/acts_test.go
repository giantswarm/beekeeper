package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const quotedRef = "owner/repo#7"

// said is a transcript line of a person's or the model's words.
func said(role, text string) string {
	content, _ := json.Marshal(text)
	return `{"type":"` + role + `","timestamp":"2026-09-25T11:50:00Z","message":{"id":"s","content":` + string(content) + "}}\n"
}

// bash is a Bash tool call of cmd.
func bash(id, cmd string) string {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return toolUse(id, "Bash", string(in))
}

// resultOf is a tool result carrying text.
func resultOf(id, text string) string {
	content, _ := json.Marshal(text)
	return `{"type":"user","timestamp":"2026-09-25T11:50:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":` + string(content) + `}]}}` + "\n"
}

func work(lines ...string) Work {
	w, _ := scanTranscript([]byte(strings.Join(lines, "")), true, testNow)
	return w
}

func TestWorkQuotedRefIsAMention(t *testing.T) {
	w := work(
		said("user", "From the supervisor: "+quotedRef+" is someone else's, see https://github.com/owner/repo/pull/8"),
		said("assistant", "Noted, "+quotedRef+" is not mine."),
		bash("a", "gh issue list --repo owner/other"),
		resultOf("a", "owner/other#9 open\n"),
	)
	if len(w.Refs) != 0 {
		t.Errorf("refs = %v, want none: nothing was acted on", w.Refs)
	}
	if !slices.Equal(w.Repos, []string{"owner/other"}) {
		t.Errorf("repos = %v, want the listed repository", w.Repos)
	}
	if !slices.Contains(w.Mentioned, quotedRef) || !slices.Contains(w.Mentioned, "owner/repo#8") || !slices.Contains(w.Mentioned, "owner/other#9") {
		t.Errorf("mentioned = %v, want the quoted refs and the one in the output", w.Mentioned)
	}
}

func TestWorkGhCommandIsAnAct(t *testing.T) {
	w := work(
		said("user", "Look at "+quotedRef+"."),
		bash("a", "gh pr view 12 --repo owner/repo"),
	)
	if !slices.Equal(w.Refs, []string{"owner/repo#12"}) {
		t.Errorf("refs = %v, want owner/repo#12", w.Refs)
	}
	if !slices.Equal(w.Mentioned, []string{quotedRef}) {
		t.Errorf("mentioned = %v, want the quoted ref only", w.Mentioned)
	}
}

func TestWorkActs(t *testing.T) {
	for name, tc := range map[string]struct {
		lines []string
		want  []string
	}{
		"devctl": {[]string{bash("a", "beekeeper gate -- devctl pr merge owner/repo 21 && devctl rollout wait inst owner/app --pr 22")},
			[]string{"owner/app#22", "owner/repo#21"}},
		"text flags are no refs": {[]string{bash("a", `gh pr comment 3 --repo owner/repo --body "fixes other/x#4" -t "x/y#5"`)},
			[]string{"owner/repo#3"}},
		"heredoc body": {[]string{bash("a", "gh issue comment 6 --repo=owner/repo --body-file - <<'EOF'\nsee other/x#4\nEOF")},
			[]string{"owner/repo#6"}},
		"url": {[]string{bash("a", "gh pr checks https://github.com/owner/repo/pull/30 --interval 10")},
			[]string{"owner/repo#30"}},
		"api path": {[]string{bash("a", "gh api repos/owner/repo/issues/31/comments --jq '.[].body'")},
			[]string{"owner/repo#31"}},
		"a run id is no ref": {[]string{bash("a", "gh run view 123456 --repo owner/repo; gh pr list --repo owner/repo --limit 5")},
			nil},
		"created pr": {[]string{bash("a", `gh pr create --repo owner/repo --title "fix owner/repo#1"`),
			resultOf("a", "https://github.com/owner/repo/pull/40\n")}, []string{"owner/repo#40"}},
		"another call's result": {[]string{bash("a", "gh pr view --repo owner/repo --json url"),
			resultOf("a", "https://github.com/owner/repo/pull/41\n")}, nil},
		"github tool": {[]string{toolUse("a", "mcp__github__get_issue", `{"owner":"owner","repo":"repo","issue_number":50,"body":"other/x#4"}`)},
			[]string{"owner/repo#50"}},
		"board tool url": {[]string{toolUse("a", "mcp__pro__get_item_by_issue", `{"issueUrl":"https://github.com/owner/repo/issues/51"}`)},
			[]string{"owner/repo#51"}},
		"created issue": {[]string{toolUse("a", "mcp__github__issue_write", `{"method":"create","owner":"owner","repo":"repo","title":"x"}`),
			resultOf("a", `{"url":"https://github.com/owner/repo/issues/52"}`)}, []string{"owner/repo#52"}},
		"a message is no act": {[]string{toolUse("a", "SendMessage", `{"to":"the supervisor","message":"merged owner/repo#60"}`)},
			nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := work(tc.lines...).Refs; !slices.Equal(got, tc.want) {
				t.Errorf("refs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkGhInACheckout(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{".git", "sub"} {
		if err := os.Mkdir(filepath.Join(repo, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	config := "[remote \"origin\"]\n\turl = git@github.com:owner/repo.git\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	line := strings.Replace(bash("a", "cd sub && gh pr view 70"), `"type":"assistant"`, `"type":"assistant","cwd":"`+repo+`"`, 1)
	if got := work(line).Refs; !slices.Equal(got, []string{"owner/repo#70"}) {
		t.Errorf("refs = %v, want the checkout's owner/repo#70", got)
	}
}

func TestWorkServe(t *testing.T) {
	w := Work{Refs: []string{"owner/a#1"}, Repos: []string{"owner/a"}, Mentioned: []string{"owner/b#2"}}.Serve("owner/b#2")
	if !slices.Equal(w.Refs, []string{"owner/b#2", "owner/a#1"}) || !slices.Equal(w.Repos, []string{"owner/b", "owner/a"}) || len(w.Mentioned) != 0 {
		t.Errorf("served %+v", w)
	}
	if got := (Work{}).Serve("not a ref"); len(got.Refs) != 0 {
		t.Errorf("a record without a ref: %+v", got)
	}
}

// The overlaps come from acted-on refs: two sessions quoting one issue are
// no overlap, two acting on it are.
func TestOverlapsFromActedRefs(t *testing.T) {
	now := time.Now()
	a := &Session{PID: 1, Name: "a", LastActive: now}
	b := &Session{PID: 2, Name: "b", LastActive: now}
	c := &Session{PID: 3, Name: "c", LastActive: now}
	quoting := work(said("user", "Is "+quotedRef+" yours?"), bash("x", "gh pr view 12 --repo owner/other"))
	acting := work(bash("x", "gh issue view 7 --repo owner/repo"))
	opts := OverlapOptions{ActiveSince: now.Add(-time.Hour), Ignore: []string{"owner/other"}}

	if got := Overlaps([]*Session{a, b}, map[int]Work{1: quoting, 2: acting}, opts); len(got) != 0 {
		t.Errorf("one session quoting what another acts on: overlaps = %+v, want none", got)
	}
	got := Overlaps([]*Session{a, b, c}, map[int]Work{1: quoting, 2: acting, 3: acting}, opts)
	if len(got) != 2 || got[0].Kind != "ref" || got[0].Key != quotedRef || !slices.Equal(got[0].Sessions, []string{"b", "c"}) {
		t.Errorf("two sessions acting on %s: overlaps = %+v", quotedRef, got)
	}
}

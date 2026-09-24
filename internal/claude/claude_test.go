package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	devctlRepo = "giantswarm/devctl"
	labRepo    = "example/lab-notes"
	mmRepo     = "giantswarm/model-manager"
	musterRepo = "giantswarm/muster"
	musterRef  = musterRepo + "#1"
	supRun     = "Supervisor run 10"
)

func TestScanWork(t *testing.T) {
	text := `{"text":"see https://github.com/giantswarm/devctl/pull/2420 and giantswarm/backstage#2594"}
{"input":{"command":"gh pr view 12 --repo giantswarm/muster"}}
{"text":"a path/to/file#12 is no ref, nor is a#b, nor owner/repo#12abc"}
{"text":"git@github.com:giantswarm/dex.git and later giantswarm/model-manager#178."}`
	w := scanWork(text)
	wantRefs := []string{mmRepo + "#178", "giantswarm/backstage#2594", devctlRepo + "#2420"}
	if !slices.Equal(w.Refs, wantRefs) {
		t.Errorf("refs = %v, want %v", w.Refs, wantRefs)
	}
	wantRepos := []string{mmRepo, "giantswarm/dex", musterRepo, "giantswarm/backstage", devctlRepo}
	if !slices.Equal(w.Repos, wantRepos) {
		t.Errorf("repos = %v, want %v", w.Repos, wantRepos)
	}
}

func TestWorkPrimaryAndCurrentRefs(t *testing.T) {
	w := Work{
		Repos: []string{"giantswarm/giantswarm", devctlRepo, labRepo},
		Refs:  []string{"giantswarm/giantswarm#37742", "giantswarm/devctl#1", "giantswarm/devctl#2", "giantswarm/dex#3", "giantswarm/dex#4"},
	}
	ignore := []string{"giantswarm/giantswarm", labRepo}
	if got := w.Primary(ignore); got != devctlRepo {
		t.Errorf("primary = %q", got)
	}
	if got := w.CurrentRefs(ignore); !slices.Equal(got, []string{"giantswarm/devctl#1", "giantswarm/devctl#2", "giantswarm/dex#3"}) {
		t.Errorf("current refs = %v", got)
	}
}

func TestOverlaps(t *testing.T) {
	now := time.Now()
	a := &Session{PID: 1, Name: "a", LastActive: now}
	b := &Session{PID: 2, Name: "b", LastActive: now}
	sup := &Session{PID: 3, Name: "supervisor", LastActive: now}
	idle := &Session{PID: 4, Name: "idle", LastActive: now.Add(-3 * time.Hour)}
	work := map[int]Work{
		1: {Repos: []string{musterRepo}, Refs: []string{musterRef}},
		2: {Repos: []string{musterRepo}, Refs: []string{musterRef}},
		3: {Repos: []string{musterRepo}, Refs: []string{musterRef}},
		4: {Repos: []string{musterRepo}, Refs: []string{musterRef}},
	}
	got := Overlaps([]*Session{a, b, sup, idle}, work, OverlapOptions{
		Skip:        func(s *Session) bool { return s == sup },
		ActiveSince: now.Add(-time.Hour),
	})
	if len(got) != 2 || got[0].Kind != "ref" || got[1].Kind != "repo" {
		t.Fatalf("overlaps = %+v", got)
	}
	for _, o := range got {
		if !slices.Equal(o.Sessions, []string{"a", "b"}) {
			t.Errorf("%s: sessions = %v", o.Key, o.Sessions)
		}
	}
}

func TestRepoFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:giantswarm/beekeeper.git":     "giantswarm/beekeeper",
		"https://github.com/example/lab-notes":        "example/lab-notes",
		"ssh://git@github.com/giantswarm/devctl.git/": "giantswarm/devctl",
		"https://gitlab.com/a/b.git":                  "gitlab.com/a/b",
	} {
		if got := RepoFromURL(in); got != want {
			t.Errorf("RepoFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitInfoWorktree(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, "repo", ".git")
	wtGit := filepath.Join(common, "worktrees", "wt")
	wt := filepath.Join(root, "wt")
	for _, d := range []string{wtGit, filepath.Join(wt, "sub")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(common, "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:giantswarm/beekeeper.git\n")
	write(filepath.Join(wtGit, "commondir"), "../..\n")
	write(filepath.Join(wtGit, "HEAD"), "ref: refs/heads/someone/feature\n")
	write(filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")
	repo, branch := GitInfo(filepath.Join(wt, "sub"))
	if repo != "giantswarm/beekeeper" || branch != "someone/feature" {
		t.Errorf("GitInfo = %q, %q", repo, branch)
	}
}

func TestSleepDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"1500": 1500 * time.Second, "5m": 5 * time.Minute, "1.5h": 90 * time.Minute} {
		if got, ok := sleepDuration([]string{in}); !ok || got != want {
			t.Errorf("sleepDuration(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := sleepDuration([]string{"infinity"}); ok {
		t.Error("infinity parsed")
	}
}

func TestResolve(t *testing.T) {
	ss := []*Session{{PID: 10, ID: "abc", Name: "Agent four"}, {PID: 11, ID: "def", Name: "Agent five"}, {PID: 12, ID: "ghi", Name: supRun}}
	for q, want := range map[string]string{"agent four": "Agent four", "def": "Agent five", "12": supRun, "supervisor": supRun} {
		s, err := Resolve(ss, q)
		if err != nil || s.Name != want {
			t.Errorf("Resolve(%q) = %v, %v; want %q", q, s, err, want)
		}
	}
	if _, err := Resolve(ss, "agent"); err == nil {
		t.Error("ambiguous query resolved")
	}
	if _, err := Resolve(ss, "nobody"); err == nil {
		t.Error("unknown query resolved")
	}
}

func TestParseTurn(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		role string
		text string
	}{
		{`{"type":"assistant","timestamp":"2026-09-24T19:00:00Z","message":{"content":[{"type":"text","text":"merged 12"},{"type":"tool_use","name":"Bash"}]}}`, true, "assistant", "merged 12"},
		{`{"type":"user","message":{"content":"merge now 12"}}`, true, "user", "merge now 12"},
		{`{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`, false, "", ""},
		{`{"type":"user","isMeta":true,"message":{"content":"meta"}}`, false, "", ""},
		{`{"type":"summary"}`, false, "", ""},
	}
	for _, c := range cases {
		turn, ok := parseTurn([]byte(c.line))
		if ok != c.ok || turn.Role != c.role || turn.Text != c.text {
			t.Errorf("parseTurn(%s) = %+v, %v", c.line, turn, ok)
		}
	}
}

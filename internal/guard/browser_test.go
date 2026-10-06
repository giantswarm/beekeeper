package guard

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

func TestBrowserRefusal(t *testing.T) {
	held := func(session string) func() []lease.Holder {
		return func() []lease.Holder {
			return []lease.Holder{{Env: "spidertron", Session: "s1"}, {Env: browserLease, Session: session}}
		}
	}
	for name, tc := range map[string]struct {
		cmd, session string
		leases       func() []lease.Holder
		refused      bool
	}{
		"muster login without the lease":  {"timeout 120 muster auth login --context spidertron --silent", "s1", held("other"), true},
		"muster login under the lease":    {"muster auth login --context spidertron", "s1", held("s1"), false},
		"no browser lease held at all":    {"muster auth login", "s1", func() []lease.Holder { return nil }, true},
		"gh web login":                    {"gh auth login --hostname github.com --web", "s1", held("other"), true},
		"gh token login opens nothing":    {"gh auth login --with-token < f", "s1", held("other"), false},
		"xdg-open after a list":           {"cd x && xdg-open https://example.com", "s1", held("other"), true},
		"muster status passes":            {"muster auth status --context spidertron", "s1", held("other"), false},
		"a mention is no command":         {"grep -n 'muster auth login' README.md", "s1", held("other"), false},
		"lease held under the desktop id": {"muster auth login", "s1", func() []lease.Holder { return []lease.Holder{{Env: browserLease, HostSession: "local_s1"}} }, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := Hook{Leases: tc.leases}.browserRefusal(tc.cmd, tc.session)
			if (r != "") != tc.refused {
				t.Errorf("browserRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
			if tc.refused && !strings.Contains(r, "beekeeper lease claim browser") {
				t.Errorf("the refusal does not say how to get the lease: %q", r)
			}
		})
	}
}

// A desktop turn (acceptEdits) of a session beekeeper started in bypass
// never calls the desktop's Claude in Chrome tools, whose navigate to a new
// site waits on a person's site approval; its headless turns (bypass), a
// person's own sessions and every other tool pass.
// The browser call and the started session the refusal tests use.
const (
	chromeNavigate = "mcp__claude-in-chrome__navigate"
	worker         = "worker"
)

func TestDesktopBrowserRefusal(t *testing.T) {
	started := func(session string) bool { return session == worker }
	for name, tc := range map[string]struct {
		tool, mode, session string
		refused             bool
	}{
		"navigate in a worker's desktop turn":     {chromeNavigate, ModeAcceptEdits, worker, true},
		"tabs context in a worker's desktop turn": {"mcp__claude-in-chrome__tabs_context_mcp", ModeAcceptEdits, worker, true},
		"the desktop's spelling of the server":    {"mcp__Claude_in_Chrome__navigate", ModeAcceptEdits, worker, true},
		"a worker's headless turn":                {chromeNavigate, "bypassPermissions", worker, false},
		"the person's own session":                {chromeNavigate, ModeAcceptEdits, "person", false},
		"another tool of a worker":                {"mcp__github__get_issue", ModeAcceptEdits, worker, false},
		"no session id":                           {chromeNavigate, ModeAcceptEdits, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			in := `{"tool_name":"` + tc.tool + `","permission_mode":"` + tc.mode + `","session_id":"` + tc.session + `","tool_input":{"url":"https://example.org"}}`
			out := Hook{Started: started}.Decide([]byte(in))
			refused := out != nil && strings.Contains(string(out), `"permissionDecision":"deny"`)
			if refused != tc.refused {
				t.Fatalf("Decide = %s, want refused %v", out, tc.refused)
			}
			if tc.refused && !strings.Contains(string(out), "beekeeper browse") {
				t.Errorf("the refusal does not name beekeeper browse: %s", out)
			}
		})
	}
	// Without a start check the hook refuses no browser call.
	if out := (Hook{}).Decide([]byte(`{"tool_name":chromeNavigate,"permission_mode":"acceptEdits","session_id":worker}`)); out != nil {
		t.Errorf("Decide without Started = %s, want nil", out)
	}
}

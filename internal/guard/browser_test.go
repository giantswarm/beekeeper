package guard

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// musterLoginCmd is the muster sign-in without arguments.
const musterLoginCmd = "muster auth login"

func TestBrowserRefusal(t *testing.T) {
	held := func(session string) func() []lease.Holder {
		return func() []lease.Holder {
			return []lease.Holder{{Env: "fixture-app", Session: "s1"}, {Env: browserLease, Session: session}}
		}
	}
	heldOnDesktop := func() []lease.Holder { return []lease.Holder{{Env: browserLease, HostSession: "local_s1"}} }
	for name, tc := range map[string]struct {
		cmd, session string
		leases       func() []lease.Holder
		refused      bool
	}{
		"muster login without the lease":       {"timeout 120 muster auth login --context fixture-app --silent", "s1", held("other"), true},
		"muster login under the lease":         {"muster auth login --context fixture-app", "s1", held("s1"), true},
		"muster login, no lease at all":        {musterLoginCmd, "s1", func() []lease.Holder { return nil }, true},
		"muster login without a lease reader":  {musterLoginCmd, "s1", nil, true},
		"no-browser under the lease":           {"muster auth login --context fixture-app --no-browser", "s1", held("s1"), false},
		"no-browser without the lease":         {"timeout 600 muster auth login --no-browser --context fixture-app", "s1", held("other"), false},
		"no-browser, no lease at all":          {"muster auth login --no-browser", "s1", func() []lease.Holder { return nil }, false},
		"no-browser=true":                      {"muster auth login --no-browser=true", "s1", held("other"), false},
		"no-browser=false opens":               {"muster auth login --no-browser=false", "s1", held("s1"), true},
		"the variable in front":                {"MUSTER_NO_BROWSER=1 muster auth login --context lab", "s1", held("other"), false},
		"the variable through env":             {"env MUSTER_NO_BROWSER=true muster auth login", "s1", held("other"), false},
		"the variable off":                     {"MUSTER_NO_BROWSER=0 muster auth login", "s1", held("s1"), true},
		"the last assignment in front decides": {"MUSTER_NO_BROWSER=1 MUSTER_NO_BROWSER=0 muster auth login", "s1", held("s1"), true},
		"an earlier export is not trusted":     {"export MUSTER_NO_BROWSER=1; muster auth login", "s1", held("s1"), true},
		"an export overridden in front":        {"export MUSTER_NO_BROWSER=1; MUSTER_NO_BROWSER=0 muster auth login", "s1", held("s1"), true},
		"an export unset again":                {"export MUSTER_NO_BROWSER=1; unset MUSTER_NO_BROWSER; muster auth login", "s1", held("s1"), true},
		"an export in a subshell":              {"(export MUSTER_NO_BROWSER=1); muster auth login", "s1", held("s1"), true},
		"the last flag decides, false last":    {"muster auth login --no-browser --no-browser=false", "s1", held("s1"), true},
		"the last flag decides, true last":     {"muster auth login --no-browser=false --no-browser", "s1", held("other"), false},
		"a flag in a comment":                  {"muster auth login # --no-browser", "s1", held("s1"), true},
		"a flag in a comment without a space":  {"muster auth login #--no-browser", "s1", held("s1"), true},
		"a hash inside a word is no comment":   {"muster auth login --context a#b --no-browser", "s1", held("other"), false},
		"a quoted hash is no comment":          {"muster auth login --context 'a #b' --no-browser", "s1", held("other"), false},
		"the flag as another option's value":   {"muster auth login --context --no-browser", "s1", held("s1"), true},
		"the flag quoted as a value":           {`muster auth login --server "--no-browser"`, "s1", held("s1"), true},
		"the flag quoted on its own":           {`muster auth login "--no-browser"`, "s1", held("other"), false},
		"the flag after a boolean flag":        {"muster auth login --silent --no-browser", "s1", held("other"), false},
		"the flag after the end of flags":      {"muster auth login -- --no-browser", "s1", held("s1"), true},
		"help as another option's value":       {"muster auth login --context -h", "s1", held("s1"), true},
		"the flag on another command":          {"muster auth login --no-browser; muster auth login --context lab", "s1", held("s1"), true},
		"the flag after the command ends":      {"muster auth login && echo --no-browser", "s1", held("s1"), true},
		"muster login's help":                  {"muster auth login --help", "s1", held("other"), false},
		"gh web login":                         {"gh auth login --hostname github.com --web", "s1", held("other"), true},
		"gh web login under the lease":         {"gh auth login --web", "s1", held("s1"), false},
		"gh token login opens nothing":         {"gh auth login --with-token < f", "s1", held("other"), false},
		"xdg-open after a list":                {"cd x && xdg-open https://example.com", "s1", held("other"), true},
		"muster status passes":                 {"muster auth status --context fixture-app", "s1", held("other"), false},
		"a mention is no command":              {"grep -n 'muster auth login' README.md", "s1", held("other"), false},
		"xdg-open, lease under the desktop id": {"xdg-open https://example.com", "s1", heldOnDesktop, false},
		"muster login, lease under desktop id": {musterLoginCmd, "s1", heldOnDesktop, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := Hook{Leases: tc.leases}.browserRefusal(tc.cmd, tc.session)
			if (r != "") != tc.refused {
				t.Errorf("browserRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
			if tc.refused && !strings.Contains(r, "beekeeper lease claim browser") {
				t.Errorf("the refusal does not say how to get the lease: %q", r)
			}
			if tc.refused && strings.Contains(tc.cmd, "muster") {
				for _, want := range []string{"`muster auth login --no-browser`", "`beekeeper browse "} {
					if !strings.Contains(r, want) {
						t.Errorf("the refusal does not name %s: %q", want, r)
					}
				}
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

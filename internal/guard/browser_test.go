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

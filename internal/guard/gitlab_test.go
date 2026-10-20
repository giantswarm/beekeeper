package guard

import (
	"strings"
	"testing"
)

// A GitLab application on record is answered on its GitLab's authorize page
// alone: by its client id, or by its title where the id is redacted, for a
// recorded callback host; any other application or callback there is
// refused; a GitHub consent page does not take its client id; the same page
// on another host is a consent page elsewhere.
func TestGitLabConsent(t *testing.T) {
	apps := []App{{Name: "workspace-manager", ClientID: "uid-1", Callbacks: []string{"muster.example.org"}, GitLab: "gitlab.example.org", Declared: "decl"}}
	page := "https://gitlab.example.org/oauth/authorize?client_id=uid-1&redirect_uri=https%3A%2F%2Fmuster.example.org%2Fcb&response_type=code"
	title := "Authorize workspace-manager to use your account?"
	for name, tc := range map[string]struct {
		url, title, want string
	}{
		"its page":            {page, title, Allow},
		"redacted, its title": {strings.ReplaceAll(page, "uid-1", chromeRedacted), title, Allow},
		"redacted, another":   {strings.ReplaceAll(page, "uid-1", chromeRedacted), "Authorize other", Deny},
		"another client id":   {strings.ReplaceAll(page, "uid-1", "uid-2"), title, Deny},
		"another callback":    {strings.ReplaceAll(page, "muster.example.org", "evil.example.net"), title, Deny},
		"another GitLab":      {strings.ReplaceAll(page, "gitlab.example.org", "gitlab.example.net"), title, Deny},
		"GitHub's page":       {strings.ReplaceAll(page, "gitlab.example.org/oauth/authorize", "github.com/login/oauth/authorize"), title, Deny},
	} {
		if d := Click(apps, Page{URL: tc.url, Title: tc.title}, "Clicks Authorize"); d.Decision != tc.want {
			t.Errorf("%s: %+v, want %s", name, d, tc.want)
		}
	}
}

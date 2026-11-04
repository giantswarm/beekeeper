package guard

import "testing"

func TestGitHubSudoBarrier(t *testing.T) {
	for _, tc := range []struct{ path, title, want string }{
		{"/organizations/example/settings/apps/demo", sudoTitle, "token_expiry,device_flow,callback_urls"},
		{"/organizations/example/settings/apps/demo/permissions", "Confirm access · GitHub", "permissions"},
		{"/organizations/example/settings/installations/123", sudoTitle, "repository_selection"},
		{"/sessions/sudo", sudoTitle, "access_confirmation"},
		{"/organizations/example/settings/apps/demo", "App settings", ""},
		{"/organizations/example/settings/installations", sudoTitle, ""},
	} {
		t.Run(tc.path+tc.title, func(t *testing.T) {
			want := tc.want
			if want != "" {
				want = "barrier: github_sudo setting=" + want
			}
			if got := GitHubSudoBarrier(Page{URL: "https://github.com" + tc.path, Title: tc.title}); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if got := GitHubSudoBarrier(Page{URL: "https://example.org" + tc.path, Title: tc.title}); got != "" {
				t.Fatalf("other site: %q", got)
			}
		})
	}
}

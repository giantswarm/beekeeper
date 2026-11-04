package guard

import "testing"

func TestGitHubSudoBarrier(t *testing.T) {
	for _, tc := range []struct{ path, title, want string }{
		{"/organizations/example/settings/apps/demo", "Confirm access", "token_expiry,device_flow,callback_urls"},
		{"/organizations/example/settings/apps/demo/permissions", "Confirm access · GitHub", "permissions"},
		{"/organizations/example/settings/installations/123", "Confirm access", "repository_selection"},
		{"/sessions/sudo", "Confirm access", "access_confirmation"},
		{"/organizations/example/settings/apps/demo", "App settings", ""},
		{"/organizations/example/settings/installations", "Confirm access", ""},
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

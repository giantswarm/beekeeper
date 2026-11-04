package guard

import (
	"net/url"
	"slices"
	"strings"
)

// GitHubSudoBarrier identifies metadata hidden by GitHub's access confirmation.
const sudoPermissions = "permissions"

// It deliberately includes no URL, account, App name, or challenge value.
func GitHubSudoBarrier(page Page) string {
	u, err := url.Parse(page.URL)
	if err != nil || u.Scheme != executionHTTPS || !strings.EqualFold(u.Host, githubHost) ||
		!strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(page.Title, " · GitHub")), "Confirm access") {
		return ""
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	setting := ""
	if len(p) >= 5 && p[0] == "organizations" && p[2] == "settings" {
		switch {
		case p[3] == "installations" && len(p) == 5:
			setting = "repository_selection"
		case p[3] == "apps" && len(p) == 5:
			setting = "token_expiry,device_flow,callback_urls"
		case p[3] == "apps" && len(p) == 6 && p[5] == sudoPermissions:
			setting = sudoPermissions
		}
	}
	// GitHub can redirect to its sudo endpoint instead of keeping the target URL.
	if u.Path == "/sessions/sudo" {
		setting = "access_confirmation"
	}
	if setting == "" {
		return ""
	}
	return "barrier: github_sudo setting=" + setting
}

// GitHubSudoBarriers reports confirmed sudo pages from the browser's tab metadata.
func GitHubSudoBarriers(transcript string) []string {
	var lines []string
	observePages(transcript, func(page Page) {
		if line := GitHubSudoBarrier(page); line != "" && !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	})
	slices.Sort(lines)
	return lines
}

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

var alertRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9_.-]+$`)

// DependabotAlert is the state of one alert, without its advisory payload.
type DependabotAlert struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	URL    string `json:"html_url"`
}

// ReadDependabotAlert uses the caller's gh identity, including an agent's App
// identity. It never falls back to a person's login or changes token scopes.
func ReadDependabotAlert(ctx context.Context, run GH, repo string, number int) (DependabotAlert, error) {
	if !alertRepository.MatchString(repo) || number <= 0 {
		return DependabotAlert{}, fmt.Errorf("expected owner/repo and a positive alert number")
	}
	path := fmt.Sprintf("repos/%s/dependabot/alerts/%d", repo, number)
	out, err := run(ActingOn(ctx, repo), "api", "--method", "GET", path)
	if err != nil {
		return DependabotAlert{}, fmt.Errorf("read Dependabot alert: %w; the GitHub App needs repository permission Dependabot alerts: read (vulnerability_alerts), accepted on its installation; do not add scopes to a person's login", err)
	}
	var alert DependabotAlert
	if err := json.Unmarshal(out, &alert); err != nil {
		return DependabotAlert{}, fmt.Errorf("decode Dependabot alert: %w", err)
	}
	if alert.Number != number || alert.URL == "" {
		return DependabotAlert{}, fmt.Errorf("incomplete Dependabot alert response")
	}
	switch alert.State {
	case "open", "dismissed", "fixed", "auto_dismissed":
		return alert, nil
	default:
		return DependabotAlert{}, fmt.Errorf("unknown Dependabot alert state %q", alert.State)
	}
}

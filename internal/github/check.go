package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// The outcomes of a pull request's check on its head.
const (
	CheckMissing = "missing"
	CheckPending = "pending"
	CheckPass    = "pass"
	CheckFail    = "fail"
)

// Check is one named check of a pull request's head and the pull request's
// state.
type Check struct {
	// State is the pull request's: Open, Merged or Closed.
	State string
	// Outcome is CheckMissing when the head has no check of that name.
	Outcome string
	// URL is the check run's page.
	URL string
	// Annotations are the messages a failed check run annotated with its
	// own name as the title: what it found.
	Annotations []string
}

// checkJob is a check run's id in its details URL, which is the job's page.
var checkJob = regexp.MustCompile(`/job/(\d+)`)

// PullCheck reads repo#n's state and the newest check run named name on its
// head (one GraphQL request); a failed one's annotations titled name take a
// second, which leaves out the runner's own ("Process completed with exit
// code 1").
func PullCheck(ctx context.Context, run GH, repo string, n int, name string) (Check, error) {
	out, err := run(ctx, "pr", "view", strconv.Itoa(n), "--repo", repo, "--json", "state,statusCheckRollup")
	if err != nil {
		return Check{}, err
	}
	var p struct {
		State  string `json:"state"`
		Checks []struct {
			Type       string    `json:"__typename"`
			Name       string    `json:"name"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			DetailsURL string    `json:"detailsUrl"`
			StartedAt  time.Time `json:"startedAt"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return Check{}, fmt.Errorf("gh pr view %d --repo %s: %w", n, repo, err)
	}
	c := Check{State: p.State, Outcome: CheckMissing}
	var newest time.Time
	for _, r := range p.Checks {
		if r.Type != "CheckRun" || r.Name != name || (c.Outcome != CheckMissing && !r.StartedAt.After(newest)) {
			continue
		}
		newest, c.URL = r.StartedAt, r.DetailsURL
		switch {
		case r.Status != "COMPLETED":
			c.Outcome = CheckPending
		case r.Conclusion == "SUCCESS":
			c.Outcome = CheckPass
		default:
			c.Outcome = CheckFail
		}
	}
	if c.Outcome != CheckFail {
		return c, nil
	}
	m := checkJob.FindStringSubmatch(c.URL)
	if m == nil {
		return c, nil
	}
	out, err = run(ctx, "api", fmt.Sprintf("repos/%s/check-runs/%s/annotations", repo, m[1]), "--jq", fmt.Sprintf("[.[] | select(.title == %s) | .message]", strconv.Quote(name)))
	if err != nil {
		return Check{}, err
	}
	if err := json.Unmarshal(out, &c.Annotations); err != nil {
		return Check{}, fmt.Errorf("check run %s annotations: %w", m[1], err)
	}
	return c, nil
}

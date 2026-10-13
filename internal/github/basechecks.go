package github

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// BaseChecks is what a red check of a pull request is weighed against: the
// checks its base branch requires and each check's outcome on the base
// branch's head.
type BaseChecks struct {
	Base string
	// Head is the base branch's head the outcomes are read on.
	Head string
	// Required are the contexts branch protection or a ruleset of the base
	// requires.
	Required []string
	// Outcomes are the check runs' and statuses' outcomes on Head by name:
	// CheckPass, CheckFail or CheckPending; a name without one is
	// CheckMissing.
	Outcomes map[string]string
}

// Outcome is the named check's outcome on the base head, CheckMissing when
// it has none.
func (b BaseChecks) Outcome(name string) string {
	if o, ok := b.Outcomes[name]; ok {
		return o
	}
	return CheckMissing
}

// Requires says whether the base requires the named check.
func (b BaseChecks) Requires(name string) bool { return slices.Contains(b.Required, name) }

// ReadBaseChecks reads repo's base branch: the contexts its branch
// protection and its rulesets require, its head, and the outcome of every
// check run (the newest of a name) and status (the latest of a context) on
// that head; four requests or more (pages). An error is GitHub not answering one of them:
// nothing is known then.
func ReadBaseChecks(ctx context.Context, run GH, repo, base string) (BaseChecks, error) {
	b := BaseChecks{Base: base, Outcomes: map[string]string{}}
	branch := url.PathEscape(base)
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/branches/%s", repo, branch))
	if err != nil {
		return b, err
	}
	var br struct {
		Commit struct {
			Sha string `json:"sha"`
		} `json:"commit"`
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := json.Unmarshal(out, &br); err != nil || br.Commit.Sha == "" {
		return b, fmt.Errorf("the branch %s of %s: no head in GitHub's answer", base, repo)
	}
	b.Head = br.Commit.Sha
	required := br.Protection.RequiredStatusChecks.Contexts
	for _, c := range br.Protection.RequiredStatusChecks.Checks {
		required = append(required, c.Context)
	}
	rules, err := listAll[struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}](ctx, run, fmt.Sprintf("repos/%s/rules/branches/%s", repo, branch))
	if err != nil {
		return b, err
	}
	for _, r := range rules {
		if r.Type != "required_status_checks" {
			continue
		}
		for _, c := range r.Parameters.RequiredStatusChecks {
			required = append(required, c.Context)
		}
	}
	slices.Sort(required)
	b.Required = slices.Compact(required)

	if err := b.readCheckRuns(ctx, run, repo); err != nil {
		return b, err
	}
	return b, b.readStatuses(ctx, run, repo)
}

// readCheckRuns sets the outcome of the newest check run of each name on
// the head.
func (b *BaseChecks) readCheckRuns(ctx context.Context, run GH, repo string) error {
	out, err := run(ctx, "api", "--paginate", fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, b.Head),
		"--jq", ".check_runs[] | {name, status, conclusion, started_at}")
	if err != nil {
		return err
	}
	newest := map[string]time.Time{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var r struct {
			Name       string    `json:"name"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			StartedAt  time.Time `json:"started_at"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			return fmt.Errorf("the check runs of %s at %s: %q is no check run", repo, short(b.Head), sc.Text())
		}
		if t, seen := newest[r.Name]; seen && !r.StartedAt.After(t) {
			continue
		}
		newest[r.Name] = r.StartedAt
		switch {
		case r.Status != "completed":
			b.Outcomes[r.Name] = CheckPending
		case slices.Contains([]string{"success", "neutral", "skipped"}, r.Conclusion):
			b.Outcomes[r.Name] = CheckPass
		default:
			b.Outcomes[r.Name] = CheckFail
		}
	}
	return sc.Err()
}

// readStatuses sets the outcome of each status context on the head, its
// latest state (the combined status); a check run of the same name wins.
func (b *BaseChecks) readStatuses(ctx context.Context, run GH, repo string) error {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", repo, b.Head))
	if err != nil {
		return err
	}
	var s struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return fmt.Errorf("the statuses of %s at %s: %w", repo, short(b.Head), err)
	}
	for _, st := range s.Statuses {
		if _, ok := b.Outcomes[st.Context]; ok {
			continue
		}
		switch strings.ToLower(st.State) {
		case "success":
			b.Outcomes[st.Context] = CheckPass
		case "pending":
			b.Outcomes[st.Context] = CheckPending
		default:
			b.Outcomes[st.Context] = CheckFail
		}
	}
	return nil
}

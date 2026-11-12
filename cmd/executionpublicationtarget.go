package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// restPull is a pull request as GitHub's REST API answers it, as far as a
// continuation's publication target needs it.
type restPull struct {
	State string `json:"state"`
	URL   string `json:"html_url"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// readRESTPull reads pull request n of repository by its number.
func readRESTPull(ctx context.Context, repository string, n int) (restPull, error) {
	var pull restPull
	data, err := executionPublishGH(github.ActingOn(ctx, repository), workerGitHubAPIGroup, fmt.Sprintf("repos/%s/pulls/%d", repository, n))
	if err != nil {
		return pull, err
	}
	if err := json.Unmarshal(data, &pull); err != nil {
		return pull, fmt.Errorf("decode pull request %s#%d: %w", repository, n, err)
	}
	return pull, nil
}

// continuationPublication accepts only the repair targets established by the
// start gate's recorded park or loop claim. A source ref or a PR in a brief is
// not a grant. Pin the source and destination together before reserving a start.
func continuationPublication(ctx context.Context, repository string, repairs []string) (*state.ExecutionPublication, error) {
	var target *state.ExecutionPublication
	for _, raw := range repairs {
		ref, ok := parseRef(raw)
		if !ok || !strings.EqualFold(ref.Repo, repository) {
			continue
		}
		if target != nil {
			return nil, refused("continuation names multiple pull requests in its repository")
		}
		pull, err := readRESTPull(ctx, repository, ref.N)
		if err != nil {
			return nil, fmt.Errorf("read continuation pull request: %w", err)
		}
		if pull.State != noteStatusOpen ||
			pull.URL != fmt.Sprintf("https://github.com/%s/pull/%d", repository, ref.N) ||
			!strings.EqualFold(pull.Head.Repo.FullName, repository) ||
			!publicationCommit(pull.Head.SHA) || pull.Head.Ref == "" || pull.Head.Ref == pull.Base.Ref ||
			pull.Head.Ref == pull.Head.Repo.DefaultBranch {
			return nil, refused("continuation requires an open pull request on a distinct branch in its repository")
		}
		target = &state.ExecutionPublication{Branch: pull.Head.Ref, Head: pull.Head.SHA, URL: pull.URL}
	}
	return target, nil
}

// assignedContinuationPull is the pull request a continuation's publication
// goes to, read by the number it was assigned: its branch may have carried
// other pull requests before, so a listing by branch is no answer. One that
// moved to another branch or repository is refused; a closed one is returned
// closed, for the publication to refuse. Neither ever leads to a new PR.
func assignedContinuationPull(ctx context.Context, repository, assigned, branch string) ([]publicationPull, error) {
	ref, ok := parseRef(assigned)
	if !ok || !strings.EqualFold(ref.Repo, repository) {
		return nil, refused("assigned continuation pull request %s is not in %s; no new PR will be created", assigned, repository)
	}
	pull, err := readRESTPull(ctx, repository, ref.N)
	if err != nil {
		return nil, fmt.Errorf("read assigned continuation pull request %s: %w", assigned, err)
	}
	if pull.URL != assigned || pull.Head.Ref != branch || !strings.EqualFold(pull.Head.Repo.FullName, repository) {
		return nil, refused("assigned continuation pull request %s is no longer on branch %s of %s; no new PR will be created", assigned, branch, repository)
	}
	pullState := github.Closed
	if pull.State == noteStatusOpen {
		pullState = github.Open
	}
	return []publicationPull{{URL: pull.URL, State: pullState, Title: pull.Title, Body: pull.Body}}, nil
}

// closeDuplicateExecutionPulls closes, once merged has merged, the pull
// requests other executions published for the same task and left open: in
// merged's repository, at the URL and on the branch the coordinator recorded
// for them, and closing an issue merged closes. The task is the issue both
// close; a shared title, brief line or mere mention is no task identity, and
// a pull request no execution published is never touched. It returns the
// pull requests it closed.
func closeDuplicateExecutionPulls(ctx context.Context, st *state.State, merged superloop.Pull) ([]string, error) {
	ref, ok := parseRef(merged.Ref)
	if !ok {
		return nil, nil
	}
	repository := ref.Repo
	mergedURL := cmp.Or(merged.URL, loop.PullRefURL(merged.Ref))
	closes := pullClosingIssues(merged.Ref, merged.Title, merged.Body, merged.Closes)
	if len(closes) == 0 {
		return nil, nil
	}
	// The other publications of executions in the repository: URL to branch.
	branches := map[string]string{}
	for _, start := range st.Starts {
		r := start.Execution
		if r == nil || r.Published == nil || r.Published.URL == "" || r.Published.Branch == "" ||
			!strings.EqualFold(r.Request.Repository, repository) || strings.EqualFold(r.Published.URL, mergedURL) {
			continue
		}
		branches[strings.ToLower(r.Published.URL)] = r.Published.Branch
	}
	if len(branches) == 0 {
		return nil, nil
	}
	pulls, err := readRepoOpenPulls(ctx, repository)
	if err != nil {
		return nil, err
	}
	var closed []string
	for _, p := range pulls {
		url := p.URL
		if url == "" && p.Number > 0 {
			url = fmt.Sprintf("https://github.com/%s/pull/%d", repository, p.Number)
		}
		branch, recorded := branches[strings.ToLower(url)]
		if !recorded || p.HeadRefName != branch || (p.State != "" && !strings.EqualFold(p.State, github.Open)) {
			continue
		}
		var linked []string
		for _, c := range p.Closing {
			if c.Number > 0 {
				linked = append(linked, fmt.Sprintf("%s#%d", cmp.Or(c.Repository.NameWithOwner, repository), c.Number))
			}
		}
		issue := ""
		for _, c := range pullClosingIssues(repository+"#0", p.Title, p.Body, linked) {
			if slices.ContainsFunc(closes, func(m string) bool { return strings.EqualFold(m, c) }) {
				issue = c
				break
			}
		}
		if issue == "" {
			continue
		}
		comment := fmt.Sprintf("Superseded by %s, which merged and closes %s.", mergedURL, issueRefURL(issue))
		if _, err := executionPublishGH(github.ActingOn(ctx, repository), "pr", "close", url, loop.LoopRepoFlag, repository, "--comment", comment); err != nil {
			return closed, fmt.Errorf("close superseded %s: %w", url, err)
		}
		closed = append(closed, url)
	}
	return closed, nil
}

// pullClosingIssues are the issues (owner/repo#n) a pull request closes:
// those GitHub links and those its title and body name with a closing
// keyword, each once.
func pullClosingIssues(self, title, body string, linked []string) []string {
	var out []string
	for _, raw := range append(slices.Clone(linked), superloop.ParseClosingIssues(self, title+"\n"+body)...) {
		r, ok := parseRef(raw)
		if !ok || strings.EqualFold(refName(r), self) {
			continue
		}
		name := refName(r)
		if !slices.ContainsFunc(out, func(e string) bool { return strings.EqualFold(e, name) }) {
			out = append(out, name)
		}
	}
	return out
}

// issueRefURL is the issue page of an owner/repo#n reference.
func issueRefURL(ref string) string {
	if r, ok := parseRef(ref); ok {
		return fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N)
	}
	return ref
}

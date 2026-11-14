package fakegithub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

const apiBase = "https://api.github.com"

func (s *Server) wireRepo(r *repo) wire.Repository {
	return wire.Repository{
		ID:            r.id,
		NodeID:        r.nodeID,
		Name:          r.name(),
		FullName:      r.fullName,
		Owner:         wire.User{Login: r.owner()},
		Private:       r.private,
		Visibility:    r.visibility,
		DefaultBranch: r.defaultBranch,
		Archived:      r.archived,
	}
}

func users(logins []string) []wire.User {
	out := make([]wire.User, 0, len(logins))
	for _, l := range logins {
		out = append(out, wire.User{Login: l})
	}
	return out
}

func (s *Server) wireIssue(is *issue) wire.Issue {
	labels := make([]wire.Label, 0, len(is.labels))
	for _, l := range is.labels {
		labels = append(labels, wire.Label{Name: l})
	}
	return wire.Issue{
		ID:            is.id,
		NodeID:        is.nodeID,
		Number:        is.number,
		Title:         is.title,
		Body:          is.body,
		State:         is.state,
		StateReason:   is.stateReason,
		User:          wire.User{Login: is.author},
		Assignees:     users(is.assignees),
		Labels:        labels,
		CreatedAt:     is.createdAt,
		UpdatedAt:     is.updatedAt,
		ClosedAt:      is.closedAt,
		RepositoryURL: apiBase + "/repos/" + is.repo,
	}
}

func (s *Server) wirePull(p *pull) wire.PullRequest {
	r := s.st.repos[p.repo]
	wr := s.wireRepo(r)
	w := wire.PullRequest{
		ID:             p.id,
		NodeID:         p.nodeID,
		Number:         p.number,
		Title:          p.title,
		State:          p.state,
		Draft:          p.draft,
		Merged:         p.merged,
		MergedAt:       p.mergedAt,
		MergeCommitSHA: p.mergeSHA,
		User:           wire.User{Login: p.author},
		Head:           wire.BranchRef{Ref: "pr-" + strconv.Itoa(p.number), SHA: p.head, Repo: &wr},
		Base:           wire.BranchRef{Ref: p.base, SHA: r.branchHead, Repo: &wr},
		CreatedAt:      p.createdAt,
		UpdatedAt:      p.updatedAt,
		ClosedAt:       p.closedAt,
	}
	if p.mergedBy != "" {
		w.MergedBy = &wire.User{Login: p.mergedBy}
	}
	return w
}

func wireCheck(cr *checkRun) wire.CheckRun {
	return wire.CheckRun{
		ID: cr.id, Name: cr.name, HeadSHA: cr.sha, Status: cr.status, Conclusion: cr.conclusion,
		StartedAt: cr.startedAt, CompletedAt: cr.completedAt,
	}
}

func wireStatus(cs *commitStatus) wire.Status {
	return wire.Status{ID: cs.id, Context: cs.context, State: cs.state, CreatedAt: cs.createdAt, UpdatedAt: cs.createdAt}
}

func notFound() reply { return errReply(http.StatusNotFound, "Not Found") }

// readOnlyTokenRequest checks that a token mint asks for read permissions and nothing more (contract
// §1.6). A request without permissions would get every permission the App has, writes included.
// GitHub answers a request it can't grant with 422, as the fake does.
func readOnlyTokenRequest(r *http.Request) error {
	var req wire.InstallationTokenRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("problems parsing JSON: %v", err)
		}
	}
	if len(req.Permissions) == 0 {
		return errors.New("beekeeper asks for read permissions only: the request names none, which grants every permission the App has")
	}
	var beyond []string
	for p, level := range req.Permissions {
		if level != wire.PermissionRead {
			beyond = append(beyond, p+": "+level)
		}
	}
	if len(beyond) > 0 {
		sort.Strings(beyond)
		return fmt.Errorf("beekeeper asks for read permissions only, not %s", strings.Join(beyond, ", "))
	}
	return nil
}

// rest answers a routed REST request. s.mu is held.
func (s *Server) rest(c *Call, route wire.Route, params map[string]string, r *http.Request, now time.Time) reply {
	st := s.st
	repoName := params["owner"] + "/" + params["repo"]
	switch route.Operation {
	case forge.OpCreateInstallToken:
		if st.installationID != 0 && params["id"] != strconv.FormatInt(st.installationID, 10) {
			return notFound()
		}
		if err := readOnlyTokenRequest(r); err != nil {
			return errReply(http.StatusUnprocessableEntity, err.Error())
		}
		s.tokenSeq++
		tok := fmt.Sprintf("ghs_fake%06d", s.tokenSeq)
		exp := now.Add(time.Hour)
		s.tokens[tok] = exp
		return jsonReply(http.StatusCreated, wire.InstallationToken{Token: tok, ExpiresAt: exp})

	case forge.OpReposGet:
		if !st.visible(repoName) {
			return notFound()
		}
		return jsonReply(http.StatusOK, s.wireRepo(st.repos[repoName]))

	case forge.OpIssuesGet:
		ref := repoName + "#" + params["number"]
		is, ok := st.issues[ref]
		if !ok || !st.visible(repoName) {
			return notFound()
		}
		return jsonReply(http.StatusOK, s.wireIssue(is))

	case forge.OpPullsGet, forge.OpPullsListReviews:
		ref := repoName + "#" + params["number"]
		p, ok := st.pulls[ref]
		if !ok || !st.visible(repoName) {
			return notFound()
		}
		if route.Operation == forge.OpPullsGet {
			return jsonReply(http.StatusOK, s.wirePull(p))
		}
		out := make([]wire.Review, 0, len(p.reviews))
		for _, rv := range p.reviews {
			out = append(out, wire.Review{ID: rv.id, User: wire.User{Login: rv.by}, State: rv.state, CommitID: rv.commit, SubmittedAt: rv.submittedAt})
		}
		return jsonReply(http.StatusOK, out)

	case forge.OpChecksListForRef:
		if !st.visible(repoName) {
			return notFound()
		}
		runs := st.checks[repoName+"@"+params["ref"]]
		out := wire.CheckRunList{TotalCount: len(runs), CheckRuns: make([]wire.CheckRun, 0, len(runs))}
		for i := len(runs) - 1; i >= 0; i-- { // newest first
			out.CheckRuns = append(out.CheckRuns, wireCheck(runs[i]))
		}
		return jsonReply(http.StatusOK, out)

	case forge.OpStatusesListForRef, wire.OpCombinedStatus:
		if !st.visible(repoName) {
			return notFound()
		}
		all := st.stats[repoName+"@"+params["ref"]]
		newest := make([]wire.Status, 0, len(all))
		for i := len(all) - 1; i >= 0; i-- {
			newest = append(newest, wireStatus(all[i]))
		}
		if route.Operation == forge.OpStatusesListForRef {
			return jsonReply(http.StatusOK, newest)
		}
		latest := map[string]bool{}
		comb := wire.CombinedStatus{SHA: params["ref"], State: statusSuccess, Statuses: []wire.Status{}}
		for _, ws := range newest {
			if latest[ws.Context] {
				continue
			}
			latest[ws.Context] = true
			comb.Statuses = append(comb.Statuses, ws)
			switch {
			case ws.State == statusFailure || ws.State == statusError:
				comb.State = statusFailure
			case ws.State == statusPending && comb.State != statusFailure:
				comb.State = statusPending
			}
		}
		if len(comb.Statuses) == 0 {
			comb.State = statusPending
		}
		comb.TotalCount = len(comb.Statuses)
		return jsonReply(http.StatusOK, comb)

	case forge.OpReposGetBranchRules:
		if !st.visible(repoName) {
			return notFound()
		}
		rp := st.repos[repoName]
		rules := []wire.BranchRule{}
		if params["branch"] == rp.defaultBranch {
			if len(rp.requiredChecks) > 0 {
				var p wire.RequiredStatusChecksParams
				for _, ctx := range rp.requiredChecks {
					p.RequiredStatusChecks = append(p.RequiredStatusChecks, struct {
						Context string `json:"context"`
					}{Context: ctx})
				}
				raw, _ := json.Marshal(p)
				rules = append(rules, wire.BranchRule{Type: wire.RuleRequiredStatusChecks, Parameters: raw})
			}
			if rp.requiredApprovals > 0 {
				raw, _ := json.Marshal(wire.PullRequestRuleParams{RequiredApprovingReviewCount: rp.requiredApprovals})
				rules = append(rules, wire.BranchRule{Type: wire.RulePullRequest, Parameters: raw})
			}
		}
		return jsonReply(http.StatusOK, rules)

	case forge.OpListInstallationRepos:
		var names []string
		for name, rp := range st.repos {
			if rp.installed {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		out := wire.InstallationRepositories{TotalCount: len(names), Repositories: []wire.Repository{}}
		for _, n := range names {
			out.Repositories = append(out.Repositories, s.wireRepo(st.repos[n]))
		}
		return jsonReply(http.StatusOK, out)

	case forge.OpListWebhookDeliveries:
		return s.listDeliveries(r)

	case forge.OpGetWebhookDelivery:
		id, err := strconv.ParseInt(params["id"], 10, 64)
		if err != nil {
			return notFound()
		}
		for _, e := range s.log {
			if e.ID == id {
				return jsonReply(http.StatusOK, e.HookDeliveryDetail)
			}
		}
		return notFound()

	case wire.OpRedeliverWebhook:
		// Recorded as a mutating call (every scenario's no_github_writes check fails on it); the fake
		// does not redeliver.
		return jsonReply(http.StatusAccepted, map[string]any{})
	}
	return notFound()
}

// listDeliveries answers GET /app/hook/deliveries: newest first, per_page (≤100, default 30), an
// opaque cursor for older pages in the Link header, and a status filter (success, failure).
func (s *Server) listDeliveries(r *http.Request) reply {
	q := r.URL.Query()
	per := 30
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		per = min(v, 100)
	}
	var before int64 = -1
	if cur := q.Get("cursor"); cur != "" {
		v, err := strconv.ParseInt(strings.TrimPrefix(cur, "v1_"), 10, 64)
		if err != nil {
			return errReply(http.StatusUnprocessableEntity, "Invalid cursor")
		}
		before = v
	}
	status := q.Get("status")
	var page []wire.HookDelivery
	more := false
	for i := len(s.log) - 1; i >= 0; i-- {
		e := s.log[i]
		if before >= 0 && e.ID >= before {
			continue
		}
		ok := e.StatusCode >= 200 && e.StatusCode < 300
		if (status == "success" && !ok) || (status == "failure" && ok) {
			continue
		}
		if len(page) == per {
			more = true
			break
		}
		page = append(page, e.HookDelivery)
	}
	if page == nil {
		page = []wire.HookDelivery{}
	}
	rp := jsonReply(http.StatusOK, page)
	if more {
		next := fmt.Sprintf("%s/app/hook/deliveries?per_page=%d&cursor=v1_%d", s.srv.URL, per, page[len(page)-1].ID)
		if status != "" {
			next += "&status=" + status
		}
		rp.header.Set("Link", "<"+next+`>; rel="next"`)
	}
	return rp
}

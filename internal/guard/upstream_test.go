package guard

import (
	"errors"
	"strings"
	"testing"
)

// The worktrees of the upstream tests: a fork clone with an upstream remote,
// a clone of ours, a clone of theirs, a fork clone whose gh default is the
// fork (gh repo set-default), a clone of a repository off GitHub.
const (
	forkDir    = "/w/fork"
	oursDir    = "/w/ours"
	theirsDir  = "/w/theirs"
	pinnedDir  = "/w/pinned"
	offHubDir  = "/w/offhub"
	theirRepo  = "kagent-dev/kagent"
	originURL  = "remote.origin.url"
	branchKey  = "branch"
	prFill     = "gh pr create --fill"
	repoKey    = "repo"
	theirName  = "kagent"
	reviewList = "giantswarm/upstream-review#1"
)

// worktrees are the remotes and git config of each test worktree.
var worktrees = map[string]map[string]string{
	forkDir: {originURL: "git@github.com:giantswarm/kagent.git", "remote.upstream.url": "https://github.com/kagent-dev/kagent.git",
		branchKey: "upstream/fix-loop", "branch.upstream/fix-loop.remote": "origin"},
	oursDir:   {originURL: "https://github.com/giantswarm/beekeeper", branchKey: "main"},
	theirsDir: {originURL: "ssh://git@github.com/kagent-dev/kagent.git", branchKey: "main"},
	pinnedDir: {originURL: "https://github.com/giantswarm/kagent.git", "remote.origin.gh-resolved": "base",
		"remote.upstream.url": "https://github.com/kagent-dev/kagent.git"},
	offHubDir: {originURL: "https://gitlab.com/kagent-dev/kagent.git"},
}

// fakeGit answers the git calls the upstream guard makes from worktrees.
func fakeGit(dir string, args ...string) ([]byte, error) {
	cfg := worktrees[dir]
	none := errors.New("exit status 1")
	switch strings.Join(args[:min(2, len(args))], " ") {
	case "config --get-regexp":
		var b strings.Builder
		for k, v := range cfg {
			if strings.HasPrefix(k, "remote.") {
				b.WriteString(k + " " + v + "\n")
			}
		}
		return []byte(b.String()), nil
	case "config --get":
		if v, ok := cfg[args[2]]; ok {
			return []byte(v + "\n"), nil
		}
	case "remote get-url":
		if v, ok := cfg["remote."+args[len(args)-1]+".url"]; ok {
			return []byte(v + "\n"), nil
		}
	case "symbolic-ref --short":
		if v, ok := cfg[branchKey]; ok {
			return []byte(v + "\n"), nil
		}
	}
	return nil, none
}

func upstreamHook(record func(session, command, repo string)) Hook {
	h := hook()
	h.Upstream = Upstream{Orgs: []string{"giantswarm", "teemow"}, Review: reviewList, Git: fakeGit, Getenv: func(string) string { return "" }}
	h.Refused = record
	return h
}

func TestUpstreamRefusesWritesOutsideTheOrgs(t *testing.T) {
	const mutation = `gh api graphql -f query='mutation { addComment(input: {subjectId: "I_x", body: "y"}) { clientMutationId } }'`
	for _, c := range []struct{ cwd, cmd, repo string }{
		// every verb, the repository from --repo, -R, a URL or owner/repo#n
		{oursDir, "gh pr create --repo kagent-dev/kagent --title t --body b", theirRepo},
		{oursDir, "gh pr create --repo=kagent-dev/kagent --fill", theirRepo},
		{oursDir, "gh pr create -R github.com/kagent-dev/kagent -t t -b b", theirRepo},
		{oursDir, "gh pr comment https://github.com/kagent-dev/kagent/pull/12 --body 'thanks'", theirRepo},
		{oursDir, "gh pr review 12 --approve -R kagent-dev/kagent", theirRepo},
		{oursDir, "gh pr close 12 -R kagent-dev/kagent --comment 'superseded'", theirRepo},
		{oursDir, "gh pr edit 12 -R kagent-dev/kagent --add-label bug", theirRepo},
		{oursDir, "gh pr ready https://github.com/kagent-dev/kagent/pull/12", theirRepo},
		{oursDir, "gh pr merge 12 --squash -R kagent-dev/kagent", theirRepo},
		{oursDir, "gh issue create -R agent-substrate/substrate --title t --body b", "agent-substrate/substrate"},
		{oursDir, "gh issue comment kagent-dev/kagent#5 --body x", theirRepo},
		{oursDir, "gh issue close 5 --repo kagent-dev/kagent", theirRepo},
		{oursDir, "gh issue edit https://github.com/kagent-dev/kagent/issues/5 --title x", theirRepo},
		{oursDir, "gh release create v1.0.0 -R kagent-dev/kagent --notes x", theirRepo},
		// --repo between the subcommand and the verb, in one word
		{oursDir, "gh pr -R kagent-dev/kagent create --fill", theirRepo},
		{oursDir, "gh pr create -Rkagent-dev/kagent --fill", theirRepo},
		{oursDir, "gh issue comment 1 -R=kagent-dev/kagent -b x", theirRepo},
		// the repository from the worktree: gh's base, upstream before origin
		{theirsDir, "gh pr create --title t --body b", theirRepo},
		{forkDir, prFill, theirRepo},
		{forkDir, "gh issue comment 5 --body x", theirRepo},
		{oursDir, "GH_REPO=kagent-dev/kagent gh issue create -t t -b b", theirRepo},
		{oursDir, "cd /w/theirs && gh pr comment 3 -b x", theirRepo},
		// gh api: the path, the method given or implied
		{oursDir, "gh api repos/kagent-dev/kagent/issues/1/comments -f body=x", theirRepo},
		{oursDir, "gh api -X PATCH /repos/kagent-dev/kagent/pulls/3 -f state=closed", theirRepo},
		{oursDir, "gh api --method=DELETE repos/kagent-dev/kagent/issues/comments/9", theirRepo},
		{oursDir, "gh api --method PUT repos/kagent-dev/kagent/pulls/3/merge", theirRepo},
		{oursDir, "gh api -XPOST https://api.github.com/repos/kagent-dev/kagent/issues -F title=x", theirRepo},
		{oursDir, "gh api --input body.json repos/kagent-dev/kagent/pulls", theirRepo},
		{oursDir, "gh api -X POST orgs/kagent-dev/repos -f name=x", "kagent-dev/*"},
		{theirsDir, "gh api 'repos/{owner}/{repo}/issues' -f title=x", theirRepo},
		{oursDir, mutation, "node id"},
		{oursDir, `gh api graphql -f owner=kagent-dev -f query='mutation($owner:String!){ createIssue(input:{}) { issue { url } } }'`, "kagent-dev/*"},
		{oursDir, "curl -sf -X POST -H 'Authorization: token x' -d '{}' https://api.github.com/repos/kagent-dev/kagent/issues", theirRepo},
		{oursDir, "curl -d '{}' api.github.com/repos/kagent-dev/kagent/issues", theirRepo},
		{oursDir, "curl -X POST https://API.GitHub.com:443/repos/kagent-dev/kagent/issues", theirRepo},
		{oursDir, `curl -d '{"query":"mutation { addComment(input:{}) { clientMutationId } }"}' https://api.github.com/graphql`, "node id"},
		// git push: the remote's URL in the worktree, https, ssh and scp forms
		{forkDir, "git push upstream upstream/fix-loop", theirRepo},
		{oursDir, "git push https://github.com/kagent-dev/kagent.git HEAD:main", theirRepo},
		{oursDir, "git push git@github.com:kagent-dev/kagent.git fix", theirRepo},
		{oursDir, "git push ssh://git@github.com/kagent-dev/kagent fix", theirRepo},
		{theirsDir, "git push", theirRepo},
		{oursDir, "git -C /w/theirs push origin fix", theirRepo},
		{oursDir, "git push --repo=https://github.com/kagent-dev/kagent", theirRepo},
		// a shell's command string, a here-document it reads
		{oursDir, "bash -c 'gh pr create -R kagent-dev/kagent --fill'", theirRepo},
		{oursDir, "sh <<'EOF'\ngh issue comment 1 -R kagent-dev/kagent -b x\nEOF", theirRepo},
	} {
		var session, command, repo string
		d := decide(t, upstreamHook(func(s, c, r string) { session, command, repo = s, c, r }), c.cwd, c.cmd, nil)
		refused(t, d, c.cmd, "a human decides what goes upstream: patches land on our fork and as a row on "+reviewList)
		if d != nil && !strings.Contains(d.Reason, c.repo) {
			t.Errorf("%s: the refusal does not name %s:\n%s", c.cmd, c.repo, d.Reason)
		}
		if d != nil && strings.Count(d.Reason, "\n") > 0 {
			t.Errorf("%s: the refusal is more than one line:\n%s", c.cmd, d.Reason)
		}
		if !strings.Contains(repo, c.repo) || command == "" || session != "" {
			t.Errorf("%s: recorded %q to %q, want the command to %s", c.cmd, command, repo, c.repo)
		}
	}
}

func TestUpstreamPassesReadsAndOurWrites(t *testing.T) {
	for _, c := range []struct{ cwd, cmd string }{
		// reads
		{oursDir, "gh pr view 12 -R kagent-dev/kagent --json state"},
		{oursDir, "gh issue view https://github.com/kagent-dev/kagent/issues/5"},
		{oursDir, "gh pr list -R kagent-dev/kagent --author x"},
		{oursDir, "gh api repos/kagent-dev/kagent/pulls/3"},
		{oursDir, "gh api -X GET repos/kagent-dev/kagent/issues -f per_page=100"},
		{oursDir, "gh api graphql -f query='{ repository(owner:\"kagent-dev\", name:\"kagent\") { url } }'"},
		{oursDir, "gh search prs --repo kagent-dev/kagent loop"},
		{forkDir, "git fetch upstream"},
		{oursDir, "curl -s https://api.github.com/repos/kagent-dev/kagent/pulls"},
		{oursDir, `echo "gh pr create -R kagent-dev/kagent"`},
		{oursDir, `curl -d '{"query":"{ viewer { login } }"}' https://api.github.com/graphql`},
		// writes to the orgs, any case, our fork's upstream/<slug> branch
		{oursDir, prFill},
		{oursDir, "gh pr create -R giantswarm/kagent --body 'from https://github.com/kagent-dev/kagent/issues/1'"},
		{oursDir, "gh issue create -R teemow/notes -t t -b b"},
		{oursDir, "gh issue comment 3 -R GiantSwarm/beekeeper -b x"},
		{oursDir, "gh api -X POST repos/giantswarm/beekeeper/issues -f title=x"},
		{oursDir, `gh api graphql -f query='mutation { addComment(input:{}) { clientMutationId } }' -f owner=giantswarm`},
		{forkDir, "git push origin upstream/fix-loop"},
		{forkDir, "git push"},
		{oursDir, "git push -u origin feat"},
		{pinnedDir, prFill},
		{offHubDir, "git push origin feat"},
	} {
		passed(t, decide(t, upstreamHook(nil), c.cwd, c.cmd, nil), c.cmd)
	}
}

func TestUpstreamOffWithoutOrgs(t *testing.T) {
	h := upstreamHook(nil)
	h.Upstream.Orgs = nil
	passed(t, decide(t, h, oursDir, "gh pr create -R kagent-dev/kagent --fill", nil), "no orgs")
}

func TestUpstreamNamesTheReviewListByDefault(t *testing.T) {
	h := upstreamHook(nil)
	h.Upstream.Review = ""
	refused(t, decide(t, h, oursDir, "gh pr create -R kagent-dev/kagent --fill", nil), "default review",
		"as a row on the team's upstream review list")
}

func TestUpstreamRecordWithholdsTokens(t *testing.T) {
	var command string
	h := upstreamHook(func(_, c, _ string) { command = c })
	refused(t, decide(t, h, oursDir, "gh issue comment 1 -R kagent-dev/kagent -b '"+fakePAT+"'", nil), "token", "a human decides")
	if strings.Contains(command, fakePAT) || !strings.Contains(command, "withheld") {
		t.Errorf("the record carries the token or does not say it withheld it: %q", command)
	}
}

func TestUpstreamRefusesConnectorPosts(t *testing.T) {
	h := upstreamHook(nil)
	refused(t, decideEvent(t, h, toolEvent("mcp__github__create_pull_request",
		map[string]any{ownerKey: "kagent-dev", repoKey: theirName, "title": "t"})), "connector", theirRepo)
	passed(t, decideEvent(t, h, toolEvent("mcp__github__create_pull_request",
		map[string]any{ownerKey: "giantswarm", repoKey: theirName, "title": "t"})), "connector to ours")
	passed(t, decideEvent(t, h, toolEvent("mcp__github__get_pull_request",
		map[string]any{ownerKey: "kagent-dev", repoKey: theirName})), "connector read")
}

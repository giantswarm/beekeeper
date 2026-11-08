package guard

import (
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// The upstream guard refuses an agent's GitHub write to a repository outside
// the organizations it works in: a pull request, issue, comment, review,
// close, edit, merge or release there, a write through gh api or curl, a
// push to such a remote. What is meant for an upstream project lands as a
// commit on our fork and as a row on the upstream review list; a human
// decides what goes upstream and opens it. The refusal comes before the
// command runs, whatever an instruction said.

// Upstream is the upstream guard's configuration.
type Upstream struct {
	// Orgs are the GitHub organizations and accounts an agent writes to
	// (github.allowedOrgs); empty, the guard is off.
	Orgs       []string
	Visibility github.VisibilityPolicy
	// Review is where an upstream candidate is queued (github.upstreamReview),
	// named in the refusal; "" names the team's upstream review list.
	Review string
	// Git runs git in dir and returns its output; nil runs the git binary.
	Git func(dir string, args ...string) ([]byte, error)
	// Getenv reads the hook's environment (GH_REPO); nil reads os.Getenv.
	Getenv func(string) string
}

// upstreamWrite is a write the upstream guard refuses: what writes, where
// to, and the command line segment it is.
type upstreamWrite struct{ what, repo, command, refusal string }

const (
	ghRepoVerb   = "repo"
	unsetCmd     = "unset"
	originRemote = "origin"
)

// ghWrites are the gh subcommands and verbs that write to a repository.
var ghWrites = map[string]map[string]bool{
	"pr": {verbCreate: true, verbComment: true, "review": true, verbClose: true, verbEdit: true, "ready": true, "merge": true,
		"reopen": true, "lock": true, verbUnlock: true, "update-branch": true},
	"issue": {verbCreate: true, verbComment: true, verbClose: true, verbEdit: true, "reopen": true, "lock": true, verbUnlock: true,
		verbDelete: true, "transfer": true, "pin": true, "unpin": true, "develop": true},
	"release":  {verbCreate: true, verbEdit: true, verbDelete: true, "upload": true, "delete-asset": true},
	"label":    {verbCreate: true, verbEdit: true, verbDelete: true, litClone: true},
	ghRepoVerb: {verbEdit: true, verbDelete: true, "archive": true, "unarchive": true, "rename": true},
}

// ghValued are the flags of gh's pr, issue, release, label and repo verbs
// whose value is the next word, so that it is not read as a positional
// argument (a title, a body, a branch).
var ghValued = map[string]bool{"-R": true, flagRepo: true, "-t": true, "--title": true, "-b": true, "--body": true,
	"-F": true, flagBodyFile: true, "-B": true, "--base": true, "-H": true, "--head": true, "-l": true, "--label": true,
	"-a": true, "--assignee": true, "-r": true, "--reviewer": true, "-m": true, "--milestone": true, "-p": true,
	"--project": true, "--add-label": true, "--remove-label": true, "--add-assignee": true, "--remove-assignee": true,
	"--add-reviewer": true, "--remove-reviewer": true, "--add-project": true, "--remove-project": true, "-c": true,
	"--comment": true, "-n": true, "--notes": true, "--notes-file": true, "--target": true, "--template": true,
	"-T": true, "-s": true, "--subject": true, "--match-head-commit": true, "--author-email": true, "-A": true,
	"-d": true, "--description": true, "--color": true, "--name": true, worktreeReason: true, "-j": true, flagJSON: true,
	"-q": true, flagJQ: true}

var (
	// githubURL: a github.com repository URL in https, ssh or scp form, and
	// what follows its name. Groups: owner, repository.
	githubURL = regexp.MustCompile(`^(?:(?:https?|ssh|git)://(?:[^@/]+@)?(?:www\.)?github\.com(?::\d+)?/|(?:[^@/\s]+@)?github\.com:)` +
		`([\w.-]+)/([\w.-]+?)(?:\.git)?(?:[/#?].*)?$`)
	// githubAPI: a REST path under a repository or an organization, in
	// gh api's form (repos/o/r, /orgs/o) or as a URL of api.github.com.
	// Groups: the owner of repos/, its repository, the owner of orgs/.
	githubAPI = regexp.MustCompile(`^(?:(?i:(?:https?://)?api\.github\.com)(?::\d+)?)?/?(?:repos/([^/?#]+)/([^/?#]+)|orgs/([^/?#]+))(?:[/?#].*)?$`)
	// graphqlURL: GitHub's GraphQL endpoint.
	graphqlURL = regexp.MustCompile(`^(?i:(?:https?://)?api\.github\.com)(?::\d+)?/graphql(?:[?#].*)?$`)
	// issueRef: owner/repo#123.
	issueRef = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#\d+$`)
	// slug: owner/repo, or host/owner/repo as gh's --repo takes it.
	slug = regexp.MustCompile(`^(?:(?:www\.)?github\.com/)?([\w.-]+)/([\w.-]+?)(?:\.git)?$`)
	// gqlMutation: a GraphQL mutation that posts to or changes an issue,
	// pull request, review or discussion, whose repository the command
	// line rarely names (a node id).
	gqlMutation = regexp.MustCompile(`\b(?:create(?:Issue|PullRequest|Discussion)|add(?:Comment|DiscussionComment|PullRequestReview(?:Comment|Thread|ThreadReply)?)|` +
		`submitPullRequestReview|(?:close|reopen)(?:Issue|PullRequest)|update(?:Issue|IssueComment|PullRequest)|mergePullRequest|` +
		`markPullRequestReadyForReview|enablePullRequestAutoMerge|transferIssue)\s*\(`)
	// gqlOwner: an owner a GraphQL call names, in the query
	// (repository(owner: "o" …)) or as a variable (-f owner=o).
	gqlOwner = regexp.MustCompile(`\bowner\s*(?::\s*\\?"([\w.-]+)\\?"|=\s*([\w.-]+))`)
)

// writeMethods are the HTTP methods that write.
var writeMethods = map[string]bool{methodPost: true, "PATCH": true, "PUT": true, "DELETE": true}

// methodPost is the method a body implies.
const methodPost = "POST"

func (u Upstream) on() bool { return len(u.Orgs) > 0 || len(u.Visibility.PublicOnlyOwners) > 0 }

// allows reports whether owner is one of the organizations.
func (u Upstream) allows(owner string) bool {
	return len(u.Orgs) == 0 || slices.ContainsFunc(u.Orgs, func(o string) bool { return strings.EqualFold(o, owner) })
}

const unresolvedAPIRepo = "an unresolved gh api repository"

// reason is the one-line refusal of w.
func (u Upstream) reason(w upstreamWrite) string {
	if w.refusal != "" {
		return "Refused: " + w.refusal
	}
	if w.repo == unresolvedAPIRepo {
		return "Refused: cannot resolve the gh api repository; write the literal repos/<owner>/<repo>/... path."
	}
	review := u.Review
	if review == "" {
		review = "the team's upstream review list"
	}
	return "Refused: a human decides what goes upstream: patches land on our fork and as a row on " + review +
		". `" + w.what + "` writes to " + w.repo + ", outside github.allowedOrgs (" + strings.Join(u.Orgs, ", ") + ")."
}

// bashWrite is the first GitHub write cmd, run in cwd, aims at a
// repository outside the organizations; nil for none.
func (u Upstream) bashWrite(cmd, cwd string) *upstreamWrite {
	if !u.on() {
		return nil
	}
	return u.scan(cmd, cwd, false, 0)
}

func (u Upstream) scan(cmd, cwd string, forceUnknown bool, depth int) *upstreamWrite {
	if depth > 4 {
		return nil
	}
	rawSc := scanShell(cmd)
	outerPlain, inners, hadSubst := opaquePlain(rawSc.plain)
	sc := scanShell(outerPlain)
	sc.heredocs = rawSc.heredocs
	segs := sc.segments()
	vars := scanVars(sc, segs)
	states := walkSegments(sc, cwd, u.Git != nil)
	anyCD := hasAnyCD(rawSc, 0) || hasAnyCD(sc, 0)
	for _, in := range inners {
		if hasAnyCD(scanShell(in), 0) {
			anyCD = true
		}
	}
	lineMoved := (!isPlain(sc) || hadSubst) && anyCD

	for _, in := range inners {
		if w := u.scan(in, cwd, lineMoved || forceUnknown, depth+1); w != nil {
			return w
		}
	}

	for _, st := range states {
		sg := st.seg
		rawWords := shellWords(sc.plain[sg.start:sg.end])
		known := st.known && !lineMoved && !forceUnknown
		for _, words := range expandAPIRepository(rawWords, vars) {
			if w := u.write(words, st.dir, known, st.vars); w != nil {
				w.command = strings.TrimSpace(cmd[sg.start:sg.end])
				return w
			}
			if envCmd := envSplitCommand(words); envCmd != "" {
				if w := u.scan(envCmd, st.dir, !known, depth+1); w != nil {
					return w
				}
			}
			if flockCmd := flockCommand(words); flockCmd != "" {
				if w := u.scan(flockCmd, st.dir, !known, depth+1); w != nil {
					return w
				}
			}
			// A command string or here-document a shell runs is a command line
			// of its own.
			if k := commandAt(words); k == len(words) || !shells[path.Base(words[k])] {
				continue
			}
			for _, arg := range words[1:] {
				if strings.ContainsAny(arg, " \t\n") {
					if w := u.scan(arg, st.dir, !known, depth+1); w != nil {
						return w
					}
				}
			}
			for _, h := range sc.heredocs {
				if h.op >= sg.start && h.op < sg.end {
					if w := u.scan(cmd[h.start:h.end], st.dir, !known, depth+1); w != nil {
						return w
					}
				}
			}
		}
	}
	return nil
}

// expandAPIRepository resolves endpoint variables before checking the target.
// Keep every literal loop/assignment candidate so an allowed value cannot hide
// a write outside the configured organizations. Never execute shell expansion.
func expandAPIRepository(words []string, vars map[string][]string) [][]string {
	k := commandAt(words)
	if k == len(words) || path.Base(words[k]) != "gh" {
		return expandWordsList(words, vars)
	}
	ep := ghAPIEndpoint(words)
	if ep == "" {
		return expandWordsList(words, vars)
	}
	endpoints := []string{ep}
	for _, ref := range pathVar.FindAllString(ep, -1) {
		name := strings.Trim(ref[1:], "{}")
		vals, ok := vars[name]
		if !ok {
			if value, exists := os.LookupEnv(name); exists {
				vals = []string{value}
			}
		}
		if len(vals) == 0 {
			continue
		}
		var next []string
		for _, endpoint := range endpoints {
			for _, value := range vals {
				next = append(next, pathVar.ReplaceAllStringFunc(endpoint, func(candidate string) string {
					if candidate == ref {
						return value
					}
					return candidate
				}))
			}
		}
		endpoints = next
	}
	var out [][]string
	for _, endpoint := range endpoints {
		cp := slices.Clone(words)
		for i := k + 1; i < len(cp); i++ {
			if cp[i] == ep {
				cp[i] = endpoint
				break
			}
		}
		out = append(out, expandWordsList(cp, vars)...)
	}
	return out
}

// write is the refused write of one simple command, nil for none.
func (u Upstream) write(words []string, cwd string, known bool, vars map[string]string) *upstreamWrite {
	k := commandAt(words)
	if k == len(words) {
		return nil
	}
	env := ""
	for _, w := range words[:k] {
		if v, ok := strings.CutPrefix(w, "GH_REPO="); ok {
			env = v
		}
	}
	args := words[k+1:]
	var what, repo string
	switch path.Base(words[k]) {
	case "gh":
		what, repo = u.ghWrite(args, cwd, env, known)
	case gitCmd:
		what, repo = u.pushWrite(words[k:], cwd, known, vars)
	case "curl":
		what, repo = u.curlWrite(args)
	}
	if repo == "" {
		return nil
	}
	return u.checkWrite(what, repo)
}

// checkWrite applies owner and visibility policy to the resolved target.
func (u Upstream) checkWrite(what, repo string) *upstreamWrite {
	owner, _, _ := strings.Cut(repo, "/")
	if !u.allows(owner) {
		return &upstreamWrite{what: what, repo: repo}
	}
	if err := u.Visibility.Check(repo); err != nil {
		return &upstreamWrite{what: what, repo: repo, refusal: err.Error()}
	}
	return nil
}

// ghWrite is what a gh call writes and the repository it writes to; "" for
// a read. A GraphQL mutation that names no owner, or one outside the
// organizations, reads as one to an unknown repository.
func (u Upstream) ghWrite(args []string, cwd, env string, known bool) (what, repo string) {
	if len(args) == 0 {
		return "", ""
	}
	if args[0] == ghAPISub {
		return u.ghAPIWrite(args, cwd, env, known)
	}
	args = expandGHShortFlags(args)
	// The subcommand and verb are the first two positional words: gh's
	// --repo is a persistent flag, so it may stand between them.
	var pos []string
	named := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, ok := repoFlag(a); ok {
			named = repoSlug(v)
			continue
		}
		if ghValued[a] {
			if (a == "-R" || a == flagRepo) && i+1 < len(args) {
				named = repoSlug(args[i+1])
			}
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 || !ghWrites[pos[0]][pos[1]] {
		return "", ""
	}
	what = "gh " + pos[0] + " " + pos[1]
	// A positional argument: a URL or owner/repo#n names the repository,
	// as gh repo's owner/repo does.
	for _, a := range pos[2:] {
		if r := urlRepo(a); r != "" {
			return what, r
		}
		if m := issueRef.FindStringSubmatch(a); m != nil {
			return what, m[1] + "/" + m[2]
		}
		if r := repoSlug(a); r != "" && pos[0] == "repo" {
			return what, r
		}
	}
	if named != "" {
		return what, named
	}
	return what, u.baseRepo(cwd, env, known)
}

// expandGHShortFlags follows gh's short flag rules: boolean flags may be
// clustered, and a valued flag consumes the rest of its word or the next
// argument. In particular, a title containing "R" is never a repo flag.
func expandGHShortFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			out = append(out, a)
			if ghValued[a] && i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
			continue
		}
		for j := 1; j < len(a); j++ {
			flag := "-" + a[j:j+1]
			out = append(out, flag)
			if !ghValued[flag] {
				continue
			}
			if j+1 < len(a) {
				out = append(out, strings.TrimPrefix(a[j+1:], "="))
			} else if i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
			break
		}
	}
	return out
}

// repoFlag is the value of a --repo or -R flag written in one word
// (--repo=o/r, -Ro/r, -R=o/r).
func repoFlag(a string) (string, bool) {
	if v, ok := strings.CutPrefix(a, flagRepo+"="); ok {
		return v, true
	}
	if v, ok := strings.CutPrefix(a, "-R"); ok && v != "" {
		return strings.TrimPrefix(v, "="), true
	}
	return "", false
}

// ghAPIMethod is the HTTP method of a gh api call's args (from "api" on):
// the one -X or --method names, else POST when a field or an input gives
// a body, else "".
func ghAPIMethod(args []string) string {
	method, fields := "", false
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-X" || a == "--method") && i+1 < len(args):
			method = strings.ToUpper(args[i+1])
			i++
		case strings.HasPrefix(a, "--method="):
			method = strings.ToUpper(strings.TrimPrefix(a, "--method="))
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			method = strings.ToUpper(a[2:])
		case a == "-f" || a == "-F" || a == flagField || a == flagRawField || a == flagInput,
			strings.HasPrefix(a, flagField+"="), strings.HasPrefix(a, flagRawField+"="), strings.HasPrefix(a, flagInput+"="),
			strings.HasPrefix(a, "-f") && len(a) > 2, strings.HasPrefix(a, "-F") && len(a) > 2:
			fields = true
		case ghAPIValued[a]:
			i++
		}
	}
	if method == "" && fields {
		method = methodPost
	}
	return method
}

// ghAPIWrite is a gh api call's write: a write method, given or implied by
// a field or an input, on a repository's or an organization's path, or a
// GraphQL mutation that posts.
func (u Upstream) ghAPIWrite(args []string, cwd, env string, known bool) (what, repo string) {
	words := append([]string{"gh"}, args...)
	ep := ghAPIEndpoint(words)
	if ep == "" {
		return "", ""
	}
	if ep == executionGraphQL {
		return u.gqlWrite("gh api graphql", strings.Join(args, " ")+"\n"+ghAPIBodies(words, cwd))
	}
	method := ghAPIMethod(args)
	if !writeMethods[method] {
		return "", ""
	}
	what = "gh api -X " + method + " " + ep
	if strings.HasPrefix(strings.TrimPrefix(ep, "/"), "repos//") {
		return what, unresolvedAPIRepo
	}
	m := githubAPI.FindStringSubmatch(ep)
	switch {
	case m == nil:
		if strings.HasPrefix(strings.TrimPrefix(ep, "/"), "repos/") || strings.ContainsAny(ep, "$`") {
			return what, unresolvedAPIRepo
		}
		return "", ""
	case strings.ContainsAny(m[1]+m[2]+m[3], "$`"):
		return what, unresolvedAPIRepo
	case m[3] != "":
		return what, m[3] + "/*"
	case m[1] == "{owner}" || m[2] == "{repo}":
		return what, u.baseRepo(cwd, env, known)
	}
	return what, m[1] + "/" + m[2]
}

// gqlWrite is the write of a GraphQL call whose query and variables are in
// text: a mutation that posts, to an owner outside the organizations or to
// a target it names by node id only.
func (u Upstream) gqlWrite(via, text string) (what, repo string) {
	if !gqlMutation.MatchString(text) {
		return "", ""
	}
	if len(u.Visibility.PublicOnlyOwners) > 0 {
		return via, "an unresolved GraphQL mutation repository"
	}
	what = via + " (" + strings.TrimSuffix(strings.TrimSpace(gqlMutation.FindString(text)), "(") + ")"
	owners := gqlOwner.FindAllStringSubmatch(text, -1)
	if len(owners) == 0 {
		return what, "a repository the mutation names by node id only (write it as gh pr or gh issue with --repo)"
	}
	for _, m := range owners {
		if o := m[1] + m[2]; !u.allows(o) || u.Visibility.Restricted(o) {
			return what, o + "/*"
		}
	}
	return "", ""
}

// curlWrite is a curl call's write to GitHub's REST API: a write method,
// given or implied by a body, on a repository's or an organization's path.
func (u Upstream) curlWrite(args []string) (what, repo string) {
	method, body, target := "", false, ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-X" || a == "--request") && i+1 < len(args):
			method = strings.ToUpper(args[i+1])
			i++
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			method = strings.ToUpper(a[2:])
		case strings.HasPrefix(a, "--request="):
			method = strings.ToUpper(strings.TrimPrefix(a, "--request="))
		case curlData[strings.SplitN(a, "=", 2)[0]]:
			body = true
		case strings.Contains(strings.ToLower(a), "api.github.com"):
			target = strings.TrimPrefix(a, "--url=")
		}
	}
	if method == "" && body {
		method = methodPost
	}
	if target == "" || !writeMethods[method] {
		return "", ""
	}
	if graphqlURL.MatchString(target) {
		return u.gqlWrite("curl graphql", strings.Join(args, " "))
	}
	m := githubAPI.FindStringSubmatch(target)
	switch {
	case m == nil:
		return "", ""
	case m[3] != "":
		return "curl -X " + method, m[3] + "/*"
	}
	return "curl -X " + method, m[1] + "/" + m[2]
}

// pushWrite is a git push's write: the GitHub repository of the remote it
// pushes to, a name resolved in the worktree it runs in (git -C), a URL as
// given, the branch's push remote when none is named.
func (u Upstream) pushWrite(gitWords []string, cwd string, known bool, vars map[string]string) (what, repo string) {
	args, dir, dirKnown, _ := parseGitGlobal(gitWords, cwd, known, vars, u.Git != nil)
	if len(args) == 0 || args[0] != "push" {
		return "", ""
	}
	remote := ""
	for k := 1; k < len(args); k++ {
		a := args[k]
		if v, ok := strings.CutPrefix(a, flagRepo+"="); ok {
			remote = v
			break
		}
		if gitPushValued[a] && k+1 < len(args) {
			if a == flagRepo {
				remote = args[k+1]
				break
			}
			k++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			remote = a
			break
		}
	}
	if r := urlRepo(remote); r != "" {
		return "git push " + remote, r
	}
	if !dirKnown {
		if remote == "" {
			remote = originRemote
		}
		return "git push " + remote, "a repository the guard cannot resolve after an unfollowed cd (pass git -C <dir> or a repository URL)"
	}
	if remote == "" {
		remote = u.pushRemote(dir)
	}
	url := remote
	if urlRepo(remote) == "" {
		url = u.git(dir, "remote", "get-url", "--push", remote)
	}
	if r := urlRepo(url); r != "" {
		return "git push " + remote, r
	}
	return "", ""
}

// pushRemote is the remote a plain git push in dir pushes to: the branch's
// pushRemote, remote.pushDefault, the branch's remote, origin.
func (u Upstream) pushRemote(dir string) string {
	branch := u.git(dir, "symbolic-ref", "--short", "-q", "HEAD")
	keys := []string{"remote.pushDefault"}
	if branch != "" {
		keys = []string{"branch." + branch + ".pushRemote", "remote.pushDefault", "branch." + branch + ".remote"}
	}
	for _, k := range keys {
		if r := u.git(dir, "config", "--get", k); r != "" && r != "." {
			return r
		}
	}
	return "origin"
}

// baseRepo is the repository a gh call without --repo acts on, as gh
// resolves it: GH_REPO, the remote gh repo set-default chose, else the
// first GitHub remote of upstream, github and origin, else any.
func (u Upstream) baseRepo(cwd, env string, known bool) string {
	getenv := u.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if env == "" {
		env = getenv("GH_REPO")
	}
	if r := repoSlug(env); r != "" {
		return r
	}
	if !known {
		return "a repository the guard cannot resolve after an unfollowed cd (pass -R <owner/repo> or a URL)"
	}
	type remote struct{ name, repo, resolved string }
	var remotes []remote
	byName := map[string]int{}
	for line := range strings.SplitSeq(u.git(cwd, "config", "--get-regexp", `^remote\..*\.(url|gh-resolved)$`), "\n") {
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		key = strings.TrimPrefix(key, "remote.")
		dot := strings.LastIndexByte(key, '.')
		if dot < 0 {
			continue
		}
		name, field := key[:dot], key[dot+1:]
		j, seen := byName[name]
		if !seen {
			j = len(remotes)
			byName[name] = j
			remotes = append(remotes, remote{name: name})
		}
		if field == "url" {
			remotes[j].repo = urlRepo(val)
		} else {
			remotes[j].resolved = val
		}
	}
	for _, r := range remotes {
		if r.resolved == "base" && r.repo != "" {
			return r.repo
		}
		if s := repoSlug(r.resolved); s != "" {
			return s
		}
	}
	for _, name := range []string{"upstream", "github", "origin"} {
		if j, ok := byName[name]; ok && remotes[j].repo != "" {
			return remotes[j].repo
		}
	}
	for _, r := range remotes {
		if r.repo != "" {
			return r.repo
		}
	}
	return ""
}

// git runs git in dir and returns its trimmed output, "" on an error.
func (u Upstream) git(dir string, args ...string) string {
	git := u.Git
	if git == nil {
		git = runGit
	}
	out, err := git(dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// urlRepo is owner/repo of a github.com URL, "" for anything else.
func urlRepo(s string) string {
	if m := githubURL.FindStringSubmatch(s); m != nil {
		return m[1] + "/" + m[2]
	}
	return ""
}

// repoSlug is owner/repo of gh's --repo value (OWNER/REPO,
// HOST/OWNER/REPO or a URL), "" for anything else.
func repoSlug(s string) string {
	if r := urlRepo(s); r != "" {
		return r
	}
	if m := slug.FindStringSubmatch(s); m != nil {
		return m[1] + "/" + m[2]
	}
	return ""
}

// Include repository mutations which post no text as well as comment tools.
func upstreamConnectorWrites(tool string) bool {
	if postsToGitHub(tool) {
		return true
	}
	for _, verb := range []string{"create_issue", "update_issue", "create_or_update_file", "delete_file", "push_files", "create_branch", "merge_pull_request", "create_repository", "fork_repository"} {
		if strings.HasSuffix(tool, "__"+verb) {
			return true
		}
	}
	return false
}

// ownerKey is the input of a GitHub connector tool that names the owner.
const ownerKey = "owner"

// connectorWrite is the refused write of a GitHub connector (MCP) tool that
// posts: its owner input outside the organizations; nil for none.
func (u Upstream) connectorWrite(tool string, input map[string]any) *upstreamWrite {
	if !u.on() || !strings.HasPrefix(tool, mcpPrefix) || !upstreamConnectorWrites(tool) {
		return nil
	}
	owner, _ := input[ownerKey].(string)
	if owner == "" && len(u.Visibility.PublicOnlyOwners) == 0 {
		return nil
	}
	repo, _ := input["repo"].(string)
	w := u.checkWrite(tool, owner+"/"+repo)
	if owner == "" && len(u.Visibility.PublicOnlyOwners) > 0 {
		w = &upstreamWrite{what: tool, repo: "unknown", refusal: "cannot resolve repository visibility for connector write"}
	}
	if w != nil {
		w.command = tool
	}
	return w
}

// commandLimit bounds the command a hook.refused event records.
const commandLimit = 300

// refuseUpstream refuses w and records it: the command as written, unless a
// token pattern matches in it, cut at commandLimit.
func (h Hook) refuseUpstream(session string, w upstreamWrite) []byte {
	if h.Refused != nil {
		command := w.command
		if hits := h.Outbound.hits(command); len(hits) > 0 {
			command = w.what + " [command withheld: " + strings.Join(hits, ", ") + "]"
		}
		if len(command) > commandLimit {
			command = command[:commandLimit] + "…"
		}
		h.Refused(session, command, w.repo)
	}
	return answer(hookOutput{PermissionDecision: decisionDeny, Reason: h.Upstream.reason(w)})
}

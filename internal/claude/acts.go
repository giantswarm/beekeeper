package claude

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// Acts collects, in transcript order, the issues, pull requests and
// repositories a session acts on: what its gh and devctl commands name, the
// owner, repository and number of its GitHub tools' calls, and what the
// issues and pull requests it creates come back as. Text it reads or writes
// is no act: a ref it only quotes is a mention.
type Acts struct {
	n        int
	refs     map[string]int
	repos    map[string]int
	creates  map[string]bool
	checkout map[string]string
}

// NewActs starts an empty collection.
func NewActs() *Acts {
	return &Acts{refs: map[string]int{}, repos: map[string]int{}, creates: map[string]bool{}, checkout: map[string]string{}}
}

func (a *Acts) repo(repo string) {
	a.n++
	a.repos[repo] = a.n
}

func (a *Acts) ref(repo, num string) {
	a.repo(repo)
	a.refs[repo+"#"+num] = a.n
}

// refsIn notes the refs a URL or an "owner/repo#N" in s names.
func (a *Acts) refsIn(s string) {
	for _, r := range scanWork(s).Refs {
		repo, num, _ := strings.Cut(r, "#")
		a.ref(repo, num)
	}
}

// Call notes what a tool call acts on: the gh and devctl commands of a
// shell command run in cwd, or a tool's owner, repo and number fields.
func (a *Acts) Call(id, name string, input json.RawMessage, cwd string) {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return
	}
	if cmd, ok := in["command"].(string); ok {
		a.command(id, cmd, cwd)
		return
	}
	if strings.HasPrefix(name, "mcp__") {
		a.tool(id, name, in)
	}
}

// Result notes the refs a call that creates an issue or a pull request
// returned: the new item's URL.
func (a *Acts) Result(id, text string) {
	if a.creates[id] {
		a.refsIn(text)
	}
}

// Work is what the session acted on, with the refs window mentions besides
// as Mentioned.
func (a *Acts) Work(window string) Work {
	w := Work{Repos: byRecency(a.repos), Refs: byRecency(a.refs)}
	for _, r := range scanWork(window).Refs {
		if _, ok := a.refs[r]; !ok {
			w.Mentioned = append(w.Mentioned, r)
		}
	}
	return w
}

// numberKeys are the fields a GitHub tool takes an issue or pull request
// number in.
var numberKeys = []string{"issue_number", "issueNumber", "pull_number", "pullNumber", "pr_number", "number"}

func (a *Acts) tool(id, name string, in map[string]any) {
	owner, _ := in["owner"].(string)
	repo, _ := in["repo"].(string)
	if owner != "" && repo != "" {
		repo = owner + "/" + repo
	}
	if r, n := ownerRepo(repo); n != len(repo) || r == "" {
		repo = ""
	}
	if repo != "" {
		a.repo(repo)
		for _, k := range numberKeys {
			if num := numberField(in[k]); num != "" {
				a.ref(repo, num)
				break
			}
		}
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		if strings.HasSuffix(strings.ToLower(k), "url") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if s, ok := in[k].(string); ok {
			a.refsIn(s)
		}
	}
	method, _ := in["method"].(string)
	if strings.HasSuffix(name, "create_pull_request") || strings.HasSuffix(name, "create_issue") ||
		strings.HasSuffix(name, "issue_write") && method == "create" {
		a.creates[id] = true
	}
}

func numberField(v any) string {
	switch v := v.(type) {
	case float64:
		if v > 0 && v == float64(int(v)) {
			return strconv.Itoa(int(v))
		}
	case string:
		if v != "" && leadingDigits(v) == v {
			return v
		}
	}
	return ""
}

// command notes the gh and devctl commands of a shell command line run in
// dir; a cd before them moves dir.
func (a *Acts) command(id, cmd, dir string) {
	for _, w := range guard.Commands(cmd) {
		if w[0] == "cd" && len(w) > 1 {
			dir = resolveDir(dir, w[1])
			continue
		}
		if i := slices.IndexFunc(w, isGitHubCLI); i >= 0 {
			a.cli(id, w[i:], dir)
		}
	}
}

func resolveDir(dir, to string) string {
	if rest, ok := strings.CutPrefix(to, "~"); ok && (rest == "" || rest[0] == '/') {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, rest)
	}
	if filepath.IsAbs(to) {
		return filepath.Clean(to)
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, to)
}

// valueFlags are gh's and devctl's flags whose value is no ref: text,
// queries, limits. A value-taking flag missing here at worst lets a number
// value pass for the item's number.
var valueFlags = map[string]bool{
	"--body": true, "-b": true, "--title": true, "-t": true, "--body-file": true, "-F": true, "-f": true,
	"--field": true, "--raw-field": true, "--json": true, "--jq": true, "-q": true, "--template": true, "-T": true,
	"--limit": true, "-L": true, "--search": true, "-S": true, "--label": true, "-l": true, "--assignee": true,
	"-a": true, "--author": true, "-A": true, "--base": true, "-B": true, "--head": true, "-H": true,
	"--state": true, "-s": true, "--reviewer": true, "-r": true, "--milestone": true, "-m": true,
	"--subject": true, "--message": true, "--interval": true, "-i": true, "--timeout": true, "--add-label": true,
	"--remove-label": true, "--add-assignee": true, "--remove-assignee": true, "--hostname": true, "--method": true,
	"-X": true, "--header": true, "--input": true, "--match-head-commit": true, "--reason": true,
}

// cli notes what one gh or devctl command acts on: its --repo and the
// number of a gh pr or issue command, a devctl pr command's repository and
// number, the URLs and "repos/owner/repo/…" paths among its arguments.
func (a *Acts) cli(id string, w []string, dir string) {
	tool := path.Base(w[0])
	var repo, pr string
	var args []string
	for j := 1; j < len(w); j++ {
		s := w[j]
		if !strings.HasPrefix(s, "-") || s == "-" {
			args = append(args, s)
			continue
		}
		flag, val, inline := strings.Cut(s, "=")
		if !inline && (flag == "--repo" || flag == "-R" || flag == "--pr" || valueFlags[flag]) && j+1 < len(w) {
			j++
			val = w[j]
		}
		switch flag {
		case "--repo", "-R":
			repo = repoArg(val)
		case "--pr":
			pr = numberField(val)
		}
	}
	var nums []string
	for i, s := range args {
		switch {
		case s != "" && leadingDigits(s) == s:
			nums = append(nums, s)
		case strings.HasPrefix(strings.TrimPrefix(s, "/"), "repos/"):
			a.apiPath(strings.TrimPrefix(strings.TrimPrefix(s, "/"), "repos/"))
		case repoArg(s) == s && i > 0 && repo == "":
			repo = s
		default:
			a.refsIn(s)
		}
	}
	if pr != "" {
		nums = append(nums, pr)
	}
	sub, verb := "", ""
	if len(args) > 1 {
		sub, verb = args[0], args[1]
	}
	switch {
	case tool == "gh" && (sub == "pr" || sub == "issue") && verb == "create":
		a.creates[id] = true
	case tool == "gh" && (sub == "pr" || sub == "issue") && verb != "list" && verb != "status":
		nums = nums[:min(1, len(nums))]
	case tool == "devctl" && sub == "pr", pr != "":
	default:
		nums = nil
	}
	if repo == "" && tool == "gh" && len(nums) > 0 && dir != "" {
		repo = a.checkoutRepo(dir)
	}
	if repo == "" {
		return
	}
	a.repo(repo)
	for _, n := range nums {
		a.ref(repo, n)
	}
}

// repoArg is "owner/repo" from a --repo value, "" when it is none.
func repoArg(s string) string {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "github.com/"), "/")
	if r, n := ownerRepo(s); r != "" && n == len(s) {
		return r
	}
	return ""
}

// apiPath notes a gh api path's "owner/repo" and its issue or pull
// request.
func (a *Acts) apiPath(p string) {
	repo, n := ownerRepo(p)
	if repo == "" {
		return
	}
	a.repo(repo)
	rest := p[n:]
	for _, kind := range []string{"/pulls/", "/issues/"} {
		if num := leadingDigits(strings.TrimPrefix(rest, kind)); strings.HasPrefix(rest, kind) && num != "" {
			a.ref(repo, num)
		}
	}
}

// checkoutRepo is the GitHub repository of the checkout dir lies in.
func (a *Acts) checkoutRepo(dir string) string {
	r, ok := a.checkout[dir]
	if !ok {
		r, _ = GitInfo(dir)
		a.checkout[dir] = r
	}
	return r
}

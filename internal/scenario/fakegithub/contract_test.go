//go:build scenario

package fakegithub

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// ContractEnv turns the contract test on: it needs the real gh on PATH.
const ContractEnv = "BEEKEEPER_GH_CONTRACT"

// realHost is the host gh reaches over plain HTTP: its local instance,
// whose API is http://api.github.localhost. The test's server is its
// proxy.
const realHost = "github.localhost"

// How a case's outputs are compared.
const (
	sameBytes = iota // stdout byte for byte
	sameJSON         // stdout as JSON values
	sameHead         // an --include answer: status, the headers that matter, body
)

type contractCase struct {
	name string
	args []string
	how  int
	// etagOf makes the call conditional: each side first reads the ETag
	// of that path and sends it back.
	etagOf string
}

var contractCases = []contractCase{
	{name: "api body", args: []string{"api", "repos/example/app"}},
	{name: "api --jq string", args: []string{"api", "repos/example/app", "--jq", ".full_name"}},
	{name: "api --jq list", args: []string{"api", "repos/example/app/issues/1", "--jq", ".labels[].name"}},
	{name: "api --jq object", args: []string{"api", "repos/example/app", "--jq", "{name, private}"}},
	{name: "api --jq null and numbers", args: []string{"api", "repos/example/app", "--jq", ".description, .id, .private"}},
	{name: "api --include", args: []string{"api", "-i", "repos/example/app"}, how: sameHead},
	{name: "api conditional 304", args: []string{"api", "-i", "repos/example/app"}, how: sameHead, etagOf: "repos/example/app"},
	{name: "api 404", args: []string{"api", "repos/example/nope"}},
	{name: "api --paginate joins arrays", args: []string{"api", "--paginate", "repos/example/app/issues?state=all&per_page=1"}},
	{name: "api --paginate --slurp", args: []string{"api", "--paginate", "--slurp", "repos/example/app/issues?state=all&per_page=3"}},
	{name: "api --paginate --jq", args: []string{"api", "--paginate", "repos/example/app/issues?state=all&per_page=1", "--jq", ".[].number"}},
	{name: "api --paginate of an object", args: []string{"api", "--paginate", "repos/example/app/commits/fix-1/check-runs?per_page=100"}},
	{name: "api GET with fields", args: []string{"api", "-X", "GET", "search/issues", "-f", "q=repo:example/app is:issue is:open", "-F", "per_page=1"}},
	{name: "api graphql", args: []string{"api", "graphql", "-f", "query={viewer{login}}"}},
	{name: "api graphql typed variable", args: []string{"api", "graphql", "-f", `query=query($n:Int!){repository(owner:"example",name:"app"){issue(number:$n){title}}}`, "-F", "n=2"}},
	{name: "api graphql error", args: []string{"api", "graphql", "-f", `query={repository(owner:"example",name:"app"){issue(number:99){title}}}`}},
	{name: "api graphql --paginate", args: []string{"api", "graphql", "--paginate", "-f",
		`query=query($endCursor:String){repository(owner:"example",name:"app"){issues(first:1,after:$endCursor){nodes{number} pageInfo{hasNextPage endCursor}}}}`}},
	{name: "the board's items query", args: []string{"api", "graphql", "-f", "query=" + coreItemsQuery, "-F", "n=1", "-f", "o=example", "-f", "q=is:open"}},
	{name: "pr view --json", how: sameJSON, args: []string{"pr", "view", "4", "--repo", "example/app", "--json",
		"number,state,title,body,url,headRefName,baseRefName,isDraft,author,labels,mergedAt,mergeCommit,reviewDecision,mergeable,closingIssuesReferences,reviews"}},
	{name: "pr view of no pull request", args: []string{"pr", "view", "99", "--repo", "example/app", "--json", "state"}},
	{name: "pr list --json", how: sameJSON, args: []string{"pr", "list", "--repo", "example/app", "--state", "all", "--json", "number,title,state,headRefName"}},
	{name: "search prs --json", how: sameJSON, args: []string{"search", "prs", "--state", "open", "--json", "repository,number", "--limit", "1000", "--author", "desk", "--owner", "example"}},
	{name: "search prs of nobody", how: sameJSON, args: []string{"search", "prs", "--state", "open", "--json", "number,title,state,url,isDraft", "--author", "nobody", "--repo", "example/app"}},
}

var (
	etagLine  = regexp.MustCompile(`(?mi)^etag: (\S+)\r?$`)
	keptHeads = regexp.MustCompile(`(?i)^(content-type|link|retry-after):`)
)

// normalizeHead reduces an --include answer to what both sides must
// agree on: the status, the headers that carry meaning and the body. The
// protocol, the date, the length, the request's id and the rate limit
// counters differ by the transport and by the order of the calls; the
// ETag's value by the address in the body's links.
func normalizeHead(out string) string {
	head, body, _ := strings.Cut(out, "\r\n\r\n")
	lines := strings.Split(head, "\n")
	status := strings.Fields(lines[0])
	kept := []string{strings.Join(status[1:], " ")}
	for _, l := range lines[1:] {
		l = strings.TrimRight(l, "\r")
		switch {
		case keptHeads.MatchString(l):
			kept = append(kept, l)
		case strings.HasPrefix(strings.ToLower(l), "etag:"):
			kept = append(kept, "Etag: present")
		}
	}
	return strings.Join(kept, "\n") + "\n\n" + body
}

// The fake gh renders what the real gh renders against the same server:
// the same output and the same exit code for every case. On demand
// (make test-gh-contract): it needs the real gh, and it fails when the
// translator drifts from it.
func TestContractWithRealGH(t *testing.T) {
	if os.Getenv(ContractEnv) == "" {
		t.Skipf("on demand: set %s=1 with the real gh on PATH (make test-gh-contract)", ContractEnv)
	}
	real, err := exec.LookPath("gh")
	if err != nil {
		t.Fatalf("the contract test needs the real gh: %v", err)
	}
	version, _ := exec.Command(real, "--version").Output() //nolint:gosec // the real gh
	t.Logf("real gh: %s", bytes.SplitN(version, []byte("\n"), 2)[0])
	s, _ := build(t, fixture)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	home := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GH_CONFIG_DIR=" + home + "/gh", "XDG_CONFIG_HOME=" + home + "/.config",
		"XDG_STATE_HOME=" + home + "/.local/state", "XDG_CACHE_HOME=" + home + "/.cache", "XDG_DATA_HOME=" + home + "/.local/share",
		"GH_HOST=" + realHost, "GH_TOKEN=" + Token, "HTTP_PROXY=" + s.URL(), "http_proxy=" + s.URL(), "NO_PROXY=", "no_proxy=",
		"GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1", "GH_TELEMETRY=disabled", "GH_SPINNER_DISABLED=1",
		"GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "PAGER=cat", "NO_COLOR=1", "CLICOLOR=0", "GIT_TERMINAL_PROMPT=0",
	}
	runReal := func(args ...string) (stdout, stderr string, exit int) {
		c := exec.Command(real, args...) //nolint:gosec // the real gh against the test's own server
		c.Env, c.Dir = env, home
		var out, errOut bytes.Buffer
		c.Stdout, c.Stderr = &out, &errOut
		err := c.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			exit = ee.ExitCode()
		default:
			t.Fatalf("gh %s: %v", strings.Join(args, " "), err)
		}
		// The real gh reaches the server as its local instance's API.
		public := strings.NewReplacer("http://api."+realHost, publicAPI, "api."+realHost, "api.github.com")
		return public.Replace(out.String()), public.Replace(errOut.String()), exit
	}
	runFake := func(args ...string) (stdout, stderr string, exit int) {
		res := gh(s, "fake", args...)
		if !res.Handled {
			t.Fatalf("the fake gh does not translate gh %s: %s", strings.Join(args, " "), res.Reason)
		}
		return res.Stdout, res.Stderr, res.Exit
	}
	for _, c := range contractCases {
		t.Run(c.name, func(t *testing.T) {
			sides := map[string]func(...string) (string, string, int){"fake": runFake, "real": runReal}
			got := map[string][3]string{}
			for name, run := range sides {
				args := c.args
				if c.etagOf != "" {
					head, _, _ := run("api", "-i", c.etagOf)
					m := etagLine.FindStringSubmatch(head)
					if m == nil {
						t.Fatalf("%s gh api -i %s shows no ETag:\n%s", name, c.etagOf, head)
					}
					args = append(append([]string(nil), args...), "-H", "If-None-Match: "+m[1])
				}
				stdout, stderr, exit := run(args...)
				if c.how == sameHead {
					stdout = normalizeHead(stdout)
				}
				got[name] = [3]string{stdout, stderr, string(rune('0' + exit))}
			}
			fake, real := got["fake"], got["real"]
			if fake[2] != real[2] {
				t.Errorf("exit code: fake %s, real %s\nfake stderr: %s\nreal stderr: %s", fake[2], real[2], fake[1], real[1])
			}
			if fake[1] != real[1] {
				t.Errorf("stderr differs:\nfake: %q\nreal: %q", fake[1], real[1])
			}
			if c.how == sameJSON {
				var a, b any
				if err := json.Unmarshal([]byte(fake[0]), &a); err != nil {
					t.Fatalf("the fake's stdout is no JSON: %v: %s", err, fake[0])
				}
				if err := json.Unmarshal([]byte(real[0]), &b); err != nil {
					t.Fatalf("the real gh's stdout is no JSON: %v: %s", err, real[0])
				}
				if !reflect.DeepEqual(a, b) {
					t.Errorf("stdout differs as JSON:\nfake: %s\nreal: %s", fake[0], real[0])
				}
				return
			}
			if fake[0] != real[0] {
				t.Errorf("stdout differs:\nfake: %q\nreal: %q", fake[0], real[0])
			}
		})
	}
	if un := s.Unsupported(); len(un) != 0 {
		t.Errorf("the real gh asked for what the model lacks: %+v", un)
	}
}

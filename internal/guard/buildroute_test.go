package guard

import (
	"strings"
	"testing"
)

// slot is a command as the hook routes it through a build slot.
func slot(cmd string) string {
	return ShellQuote(self) + " run -- " + ShellQuote(testShell) + " -c " + ShellQuote(cmd)
}

func TestRouteBuildsOnlyTheBuild(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"clone then build", "git clone https://example.com/a.git && cd a && govulncheck ./...",
			"git clone https://example.com/a.git && cd a && " + slot("govulncheck ./...")},
		{"a file written, then a build", "cat > x.go <<'EOF'\npackage x\nEOF\ngo build ./...",
			"cat > x.go <<'EOF'\npackage x\nEOF\n" + slot("go build ./...")},
		{"work on both sides", "git pull; go test ./...; git status",
			"git pull; " + slot("go test ./...") + "; git status"},
		{"a pipeline stays whole", "git fetch && go test ./... 2>&1 | tail -5",
			"git fetch && " + slot("go test ./... 2>&1 | tail -5")},
		{"redirections go with the build", "git fetch && go test ./... > out.txt 2>&1",
			"git fetch && " + slot("go test ./... > out.txt 2>&1")},
		{"builds in a row share a slot", "git fetch && go vet ./... && go test ./...",
			"git fetch && " + slot("go vet ./... && go test ./...")},
		{"statements of builds share a slot", "git fetch\ngo vet ./...\ngo test ./...",
			"git fetch\n" + slot("go vet ./...\ngo test ./...")},
		{"a build each side of other work", "go build ./... && git add -A && go test ./...",
			slot("go build ./...") + " && git add -A && " + slot("go test ./...")},
		{"builds beside a || stay apart", "git fetch || go vet ./... && go test ./...",
			"git fetch || " + slot("go vet ./...") + " && " + slot("go test ./...")},
		{"a statement's && does not join across a ;", "git fetch && go vet ./...; go test ./...",
			"git fetch && " + slot("go vet ./...") + "; " + slot("go test ./...")},
		{"set options go with the build", "set -o pipefail; git fetch && go test ./... | tail -1",
			"set -o pipefail; git fetch && " + slot("set -o pipefail; go test ./... | tail -1")},
		{"an export reaches the build", "export GOFLAGS=-p=2; git fetch && go test ./...",
			"export GOFLAGS=-p=2; git fetch && " + slot("go test ./...")},
		{"an assignment before the command stays with it", "git fetch && CGO_ENABLED=0 go build ./...",
			"git fetch && " + slot("CGO_ENABLED=0 go build ./...")},
		{"an environment variable is inherited", "git fetch && go test $PKG",
			"git fetch && " + slot("go test $PKG")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(t, hook(), "/", tc.in, nil)
			if d == nil || d.UpdatedInput == nil {
				t.Fatalf("not rewritten: %+v", d)
			}
			if got := d.UpdatedInput["command"]; got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRouteBuildsKeepsALineWhole(t *testing.T) {
	for name, in := range map[string]string{
		"a single build":             "go test ./...",
		"only cd beside the build":   "cd sub && go test ./...",
		"only setup beside it":       "set -e; export A=1; cd sub && go test ./...",
		"every part a build":         "go vet ./... && go test ./...",
		"a plain assignment":         "PKG=./x; git fetch && go test $PKG",
		"an assignment from output":  "PKG=$(go list ./...); git fetch; go test $PKG",
		"a function":                 "f() { true; }; git fetch; go test ./...",
		"a subshell":                 "(cd sub && git pull) && go test ./...",
		"a group":                    "{ cd sub; git pull; } && go test ./...",
		"a loop":                     "for p in a b; do go test ./$p; done; git status",
		"a condition":                "if git pull; then go test ./...; fi",
		"source":                     ". ./env.sh; git pull && go test ./...",
		"eval":                       "eval \"$X\"; git pull && go test ./...",
		"a background job":           "git fetch & go test ./...",
		"negation":                   "! git fetch && go test ./...",
		"the exit status":            "git fetch; go test $?",
		"a positional parameter":     "git fetch; go test $1",
		"a here-document to a build": "git fetch && go run . <<EOF\nx\nEOF\n",
		"a shell reading a program":  "git fetch && bash <<EOF\ngo test ./...\nEOF\n",
		"what the line built runs":   "go build -o bin/x . && ./bin/x",
		"a set after a chain":        "git fetch && set -e && go test ./...",
		"an unparsable line":         "git fetch && go test ./... )",
	} {
		t.Run(name, func(t *testing.T) {
			d := decide(t, hook(), "/", in, nil)
			if d == nil || d.UpdatedInput == nil {
				t.Fatalf("not rewritten: %+v", d)
			}
			if got := d.UpdatedInput["command"]; got != slot(in) {
				t.Errorf("got  %s\nwant %s", got, slot(in))
			}
		})
	}
}

// A background run gets the longer wait, routed or not.
func TestRouteBuildsBackground(t *testing.T) {
	d := decide(t, hook(), "/", "git fetch && go test ./...", map[string]any{backgroundKey: true})
	if d == nil || d.UpdatedInput == nil {
		t.Fatalf("not rewritten: %+v", d)
	}
	want := backgroundExport + "git fetch && " + slot("go test ./...")
	if got := d.UpdatedInput["command"]; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGraphQLRoute(t *testing.T) {
	const query = `gh api graphql -f query='{ repository(owner: "example", name: "notes") { id } }'`
	for _, tc := range []struct{ name, in, want string }{
		{"a literal repository", query, "GH_REPO=example/notes " + query},
		{"name first", `gh api graphql -f query='{ repository(name: "notes", owner: "example") { id } }'`,
			`GH_REPO=example/notes gh api graphql -f query='{ repository(name: "notes", owner: "example") { id } }'`},
		{"variables", `gh api graphql -F owner=example -f name=notes -f query='query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }'`,
			`GH_REPO=example/notes gh api graphql -F owner=example -f name=notes -f query='query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }'`},
		{"beside other commands", "cd /tmp && " + query + " | jq .", "cd /tmp && GH_REPO=example/notes " + query + " | jq ."},
		{"one owner, two repositories", `gh api graphql -f query='{ a: repository(owner: "example", name: "x") { id } b: repository(owner: "example", name: "y") { id } }'`,
			`GH_REPO=example/x gh api graphql -f query='{ a: repository(owner: "example", name: "x") { id } b: repository(owner: "example", name: "y") { id } }'`},
		{"named already", "GH_REPO=example/other " + query, "GH_REPO=example/other " + query},
		{"a mutation", `gh api graphql -f query='mutation { x(repository(owner: "example", name: "notes")) { id } }'`,
			`gh api graphql -f query='mutation { x(repository(owner: "example", name: "notes")) { id } }'`},
		{"two owners", `gh api graphql -f query='{ a: repository(owner: "one", name: "x") { id } b: repository(owner: "two", name: "y") { id } }'`,
			`gh api graphql -f query='{ a: repository(owner: "one", name: "x") { id } b: repository(owner: "two", name: "y") { id } }'`},
		{"no repository", `gh api graphql -f query='{ viewer { login } }'`, `gh api graphql -f query='{ viewer { login } }'`},
		{"a variable no flag sets", `gh api graphql -f query='query($o: String!) { repository(owner: $o, name: "x") { id } }'`,
			`gh api graphql -f query='query($o: String!) { repository(owner: $o, name: "x") { id } }'`},
		{"text about a query", `echo 'gh api graphql repository(owner: "example", name: "notes")'`,
			`echo 'gh api graphql repository(owner: "example", name: "notes")'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := graphqlRoute(tc.in, "/"); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// The hook carries the route into the call it allows.
func TestGraphQLRouteRewrites(t *testing.T) {
	const query = `gh api graphql -f query='{ repository(owner: "example", name: "notes") { id } }'`
	d := decide(t, hook(), "/", query, nil)
	if d == nil || d.UpdatedInput == nil {
		t.Fatalf("not rewritten: %+v", d)
	}
	if got, _ := d.UpdatedInput["command"].(string); !strings.HasPrefix(got, "GH_REPO=example/notes gh api") {
		t.Errorf("got %s", got)
	}
}

// The cases of the text-argument issue: what a call carries is data, what it
// runs stays judged.
func TestTextIsNoCommand(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = "fixtureprod"
	for _, cmd := range []string{
		`beekeeper agents idle --done --report "ran muster auth login for the fixtureprod context"`,
		`beekeeper timer add 5m "kubectl --context fixtureprod delete pod x is the prose here"`,
		"python3 - <<'EOF'\nprint('muster auth login')\nprint('kubectl --context fixtureprod delete pod x')\nEOF\n",
		"python3 - <<'EOF'\nprint('the worker waits with devctl release wait o/r')\nEOF\n",
		"gh pr list --author @me",
		"gh issue list --assignee @me --repo example/x",
		`gh issue create --repo example/x --title t --body "a vault Employee is prose, as is op read here"`,
	} {
		wantPassed(t, h, cmd)
	}
	wantRefused(t, h, "kubectl --context fixtureprod delete pod x", prodMsg)
	wantRefused(t, h, "kubectl --context fixtureprod delete pod x", `Matched token "kubectl" at byte 0`)
	wantRefused(t, h, `beekeeper timer add 5m "check" --probe "kubectl --context fixtureprod delete pod x"`, prodMsg)
}

package guard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		{"a later set stays out of the build", "set -o pipefail; git fetch; go test ./... | cat; echo $?; set +o pipefail",
			"set -o pipefail; git fetch; " + slot("set -o pipefail; go test ./... | cat") + "; echo $?; set +o pipefail"},
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
		"a single build":              "go test ./...",
		"only cd beside the build":    "cd sub && go test ./...",
		"only setup beside it":        "set -e; export A=1; cd sub && go test ./...",
		"every part a build":          "go vet ./... && go test ./...",
		"a plain assignment":          "PKG=./x; git fetch && go test $PKG",
		"an assignment from output":   "PKG=$(go list ./...); git fetch; go test $PKG",
		"a function":                  "f() { true; }; git fetch; go test ./...",
		"a subshell":                  "(cd sub && git pull) && go test ./...",
		"a group":                     "{ cd sub; git pull; } && go test ./...",
		"a loop":                      "for p in a b; do go test ./$p; done; git status",
		"a condition":                 "if git pull; then go test ./...; fi",
		"source":                      ". ./env.sh; git pull && go test ./...",
		"eval":                        "eval \"$X\"; git pull && go test ./...",
		"a background job":            "git fetch & go test ./...",
		"negation":                    "! git fetch && go test ./...",
		"the exit status":             "git fetch; go test $?",
		"a positional parameter":      "git fetch; go test $1",
		"a here-document to a build":  "git fetch && go run . <<EOF\nx\nEOF\n",
		"a shell reading a program":   "git fetch && bash <<EOF\ngo test ./...\nEOF\n",
		"what the line built runs":    "go build -o bin/x . && ./bin/x",
		"a set after a chain":         "git fetch && set -e && go test ./...",
		"an assigning expansion":      "git fetch; go test \"${PKG:=./x}\"; echo \"$PKG\"",
		"an assignment in other work": "echo \"${PKG:=./x}\"; go test \"$PKG\"",
		"arithmetic":                  "git fetch; go test $((i++))",
		"a required parameter":        "git fetch; go test \"${X:?unset}\"",
		"a parameter of this shell":   "git fetch; go test $RANDOM",
		"a builtin behind command":    "command eval x; git fetch; go test ./...",
		"an unparsable line":          "git fetch && go test ./... )",
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
		{"no repository", literalViewerGraphQLCommand, literalViewerGraphQLCommand},
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

// A routed line does what the line as written does: the same output and exit,
// run by a real shell, with a go that fails and a run that only runs.
func TestRouteBuildsKeepsWhatTheLineDoes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	stub := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // a test's stub
			t.Fatal(err)
		}
		return p
	}
	stub("go", `echo "go $*"; exit 7`)
	h := Hook{Self: stub("beekeeper", `shift 2; exec "$@"`), Shell: bash}
	run := func(line string) string {
		c := exec.Command(bash, "-c", line) //nolint:gosec // the test's own lines
		c.Dir = dir
		c.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		return fmt.Sprintf("%s(%v)", out, err)
	}
	for _, line := range []string{
		`set -o pipefail; printf setup; go test ./... | cat; printf 'status=%s\n' $?; set +o pipefail`,
		`printf setup; go test ./... | cat; printf 'status=%s\n' $?; set -o pipefail`,
		`set -e; printf setup; go test ./...; printf 'not here\n'`,
		`set -e; printf setup; go vet ./...; go test ./...; printf 'not here\n'`,
		`printf setup; go test "${PKG:=./fixture}"; printf 'pkg=%s\n' "$PKG"`,
		`printf '%s\n' "${PKG:=./fixture}"; go test "$PKG"`,
		`i=1; printf setup; go test $((i++)); printf 'i=%s\n' "$i"`,
		`printf setup; go test $(( 1 + 1 )); printf 'done\n'`,
		`printf setup; go test "${MISSING:?unset}"; printf 'not here\n'`,
		`printf setup; go test ./...; printf 'status=%s\n' $?`,
		`printf setup && go test ./... || printf 'failed\n'`,
		`false || go test ./... && printf 'not here\n'`,
		`printf setup; go test ./... | cat; printf '%s\n' "${PIPESTATUS[0]}"`,
		`printf setup; go test ./...; printf '%s\n' "$_"`,
		`command printf setup; go test ./...`,
	} {
		if routed := h.routeBuilds(line); routed != "" {
			if got, want := run(routed), run(line); got != want {
				t.Errorf("%s\nrouted %s\ngot  %s\nwant %s", line, routed, got, want)
			}
		}
	}
}

func TestGraphQLRouteReadsAJSONRequest(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("literal.json", `{"query":"{ repository(owner: \"example\", name: \"notes\") { id } }"}`)
	write("variables.json", `{"query":"query($o: String!, $n: String!) { repository(owner: $o, name: $n) { id } }","variables":{"o":"example","n":"notes"}}`)
	write("mutation.json", `{"query":"mutation { x(repository(owner: \"example\", name: \"notes\")) { id } }"}`)
	write("owners.json", `{"query":"{ a: repository(owner: \"one\", name: \"x\") { id } b: repository(owner: \"two\", name: \"y\") { id } }"}`)
	write("plain.graphql", `{ repository(owner: "example", name: "notes") { id } }`)
	const stdin = "gh api graphql --input - <<'EOF'\n" + `{"query":"{ repository(owner: \"example\", name: \"notes\") { id } }"}` + "\nEOF\n"
	for in, routed := range map[string]bool{
		"gh api graphql --input literal.json":    true,
		"gh api graphql --input=variables.json":  true,
		"gh api graphql -F query=@plain.graphql": true,
		stdin:                                    true,
		"gh api graphql --input mutation.json":   false,
		"gh api graphql --input owners.json":     false,
		"gh api graphql --input missing.json":    false,
	} {
		want := in
		if routed {
			want = "GH_REPO=example/notes " + in
		}
		if got := graphqlRoute(in, dir); got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

// A literal is data in a program that can run nothing, and stays a command in
// one that may run it.
func TestInterpreterLiteralIsData(t *testing.T) {
	h := hook()
	for _, cmd := range []string{
		"python3 - <<'EOF'\ntext = 'devctl release wait example/repo'\nprint(text)\nEOF\n",
		"python3 - <<'EOF'\nimport pathlib, re\np = pathlib.Path('docs.md')\np.write_text('devctl pr merge o/r 7')\nEOF\n",
		"python3 - <<'EOF'\nfrom pathlib import Path\nprint('the system', 'devctl release wait o/r')\nEOF\n",
	} {
		wantPassed(t, h, cmd)
	}
	const gate = "outside the gate"
	for _, cmd := range []string{
		"python3 - <<'EOF'\nimport helper\nhelper.go('devctl pr merge o/r 7')\nEOF\n",
		"python3 - <<'EOF'\nimport os as o\nc = 'devctl pr merge o/r 7'\no.system(c)\nEOF\n",
		"python3 - <<'EOF'\nc = 'devctl pr merge o/r 7'\nexec(c)\nEOF\n",
		"python3 - <<'EOF'\nc = 'devctl pr merge o/r 7'\n__import__('os').system(c)\nEOF\n",
		"python3 - <<'EOF'\nx = 1; import shutil\nc = 'devctl pr merge o/r 7'\nEOF\n",
		"perl - <<'EOF'\nopen(F, \"devctl pr wait o/r 7 |\");\nEOF\n",
		"ruby - <<'EOF'\nopen(\"|devctl pr wait o/r 7\")\nEOF\n",
		"node - <<'EOF'\nimport('child_process').then(c => c.execSync('devctl pr wait o/r 7'))\nEOF\n",
		"perl - <<'EOF'\nprint `devctl pr wait o/r 7`;\nEOF\n",
	} {
		wantRefused(t, h, cmd, gate)
	}
}

package remotebuild

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// CheckerTool is a program a repository's lint or tests call that the
// merge-result checker's builder provides: the image's own when it has one,
// else Module, installed with go install into the builder's workspace and
// put on the command's PATH under Name.
type CheckerTool struct {
	Name   string
	Module string
}

// GolangCILintModule is the golangci-lint a Go image without one gets for
// merge validation: the version the repository's CI pins.
const GolangCILintModule = "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0"

// GoJQModule is the jq a Go image without one gets for merge validation:
// gojq, a jq written in Go, installed under the name jq.
const GoJQModule = "github.com/itchyny/gojq/cmd/gojq@v0.12.19"

// CheckerTools is the one list of the programs the merge-result checker's
// environment has before any check runs: what the CI images the repositories
// are tested in carry and their lint and tests call.
var CheckerTools = []CheckerTool{
	{Name: "golangci-lint", Module: GolangCILintModule},
	{Name: "jq", Module: GoJQModule},
}

// EnvironmentLacks starts the checker environment's line naming the Debian
// packages the repository's CI installs and the builder image lacks. A check
// that fails after it is the checker's environment fault, not the branch's.
const EnvironmentLacks = "checker environment lacks"

// checkerEnv starts every merge-result command in the builder. $1 holds the
// Debian packages the repository's CI installs; name and module pairs of
// CheckerTools follow up to --, then the command. Each tool the image lacks
// is installed into the pod's workspace and put on PATH; a failed install
// exits 125, a checker environment failure, so it is never read as a finding
// of the check. A package the image lacks cannot be installed by the builder's
// non-root user: the line naming it marks a failing check as the
// environment's. The builder's non-root user may not write an image's GOPATH
// (where Go keeps its checksum database cache) or the user's cache directory:
// installs and the linter keep both in the workspace.
const checkerEnv = `packages=$1
shift
while [ $# -gt 0 ] && [ "$1" != -- ]; do
 name=$1 module=$2
 shift 2
 command -v "$name" >/dev/null 2>&1 && continue
 dir=/work/tools/${module##*/}
 if [ ! -x "$dir/$name" ]; then
  printf 'beekeeper run: %s not in builder image; installing %s\n' "$name" "$module" >&2
  GOPATH=/work/tools/gopath GOBIN=$dir GOFLAGS= go install "$module" || { printf 'beekeeper run: installing %s failed\n' "$module" >&2; exit 125; }
  built=${module%@*}
  built=${built##*/}
  if [ "$built" != "$name" ]; then ln -sf "$built" "$dir/$name" || exit 125; fi
 fi
 PATH=$dir:$PATH
done
shift
lacking=
for p in $packages; do
 [ "$(dpkg-query -W -f='${Status}' "$p" 2>/dev/null)" = "install ok installed" ] || lacking="$lacking $p"
done
if [ -n "$lacking" ]; then
 printf 'beekeeper run: ` + EnvironmentLacks + `%s, which the repository CI installs\n' "$lacking" >&2
fi
if [ "$1" = golangci-lint ]; then
 export GOPATH=/work/tools/gopath GOLANGCI_LINT_CACHE=${GOLANGCI_LINT_CACHE:-/work/tools/lint-cache}
fi
export PATH
exec "$@"`

// checkerCommand runs argv in the checker environment of a repository whose
// CI installs packages.
func checkerCommand(argv, packages []string) []string {
	out := []string{shellBash, "-c", checkerEnv, "checker", strings.Join(packages, " ")}
	for _, t := range CheckerTools {
		out = append(out, t.Name, t.Module)
	}
	return append(append(out, "--"), argv...)
}

// ciFiles are the CI configurations whose installs a merge-result check
// needs: the GitHub workflows and the CircleCI config.
var ciFiles = []string{".github/workflows/*.yml", ".github/workflows/*.yaml", ".circleci/config.yml"}

// ciFileLimit bounds what is read of one CI file.
const ciFileLimit = 1 << 20

// CIPackages are the Debian packages the CI of the repository at root
// installs: a job's or step's apt-packages input and the packages of the
// apt-get install and apt install commands its scripts run, sorted. A
// package for another architecture than amd64, the builder's, is left out.
func CIPackages(root string) []string {
	seen := map[string]bool{}
	for _, pattern := range ciFiles {
		files, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, file := range files {
			// Only a regular file of the worktree: never a link out of it.
			if fi, err := os.Lstat(file); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			f, err := os.Open(file) //nolint:gosec // a CI file of the checked worktree, read only
			if err != nil {
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(f, ciFileLimit))
			_ = f.Close()
			var doc any
			if err != nil || yaml.Unmarshal(raw, &doc) != nil {
				continue
			}
			ciInstalls(doc, func(p string) {
				if name, ok := debianPackage(p); ok {
					seen[name] = true
				}
			})
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// ciInstalls hands add every package a CI document installs.
func ciInstalls(node any, add func(string)) {
	entry := func(key string, value any) {
		if s, ok := value.(string); ok && key == "apt-packages" {
			s = githubExpression.ReplaceAllLiteralString(s, " ")
			for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
				add(p)
			}
			return
		}
		ciInstalls(value, add)
	}
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			entry(k, v)
		}
	case map[any]any:
		for k, v := range n {
			key, _ := k.(string)
			entry(key, v)
		}
	case []any:
		for _, v := range n {
			ciInstalls(v, add)
		}
	case string:
		for _, p := range aptInstalls(n) {
			add(p)
		}
	}
}

// githubExpression is an Actions expression, which no shell parses: a
// script reads it as a parameter expansion, which no package name is.
var githubExpression = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// aptInstalls are the literal packages of the apt-get install and apt
// install commands of a shell script; text no shell parses has none.
func aptInstalls(script string) []string {
	if !strings.Contains(script, "apt") {
		return nil
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(githubExpression.ReplaceAllLiteralString(script, "${EXPRESSION}")), "")
	if err != nil {
		return nil
	}
	var out []string
	syntax.Walk(f, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		var argv []string
		for _, w := range call.Args {
			// A word the shell expands is no package name.
			value, _ := literal(w)
			argv = append(argv, value)
		}
		out = append(out, aptInstallArgs(argv)...)
		return true
	})
	return out
}

// aptInstallArgs are the packages of one command, when it is apt-get or apt
// install, possibly behind sudo or env.
func aptInstallArgs(argv []string) []string {
	i := slices.IndexFunc(argv, func(a string) bool { b := path.Base(a); return b == "apt-get" || b == "apt" })
	if i < 0 {
		return nil
	}
	var out []string
	install := false
	for j := i + 1; j < len(argv); j++ {
		switch a := argv[j]; {
		case a == "-o" || a == "-t" || a == "-c" || a == "--option" || a == "--target-release" || a == "--config-file":
			j++
		case strings.HasPrefix(a, "-"):
		case !install && a != "install":
			return nil
		case !install:
			install = true
		default:
			out = append(out, a)
		}
	}
	return out
}

// debianName is a package as apt-get install takes it: a name, optionally an
// architecture and a version.
var debianName = regexp.MustCompile(`^([a-z0-9][a-z0-9+.-]+)(?::([a-z0-9-]+))?(?:=\S+)?$`)

// debianPackage is the package name p installs on the builder's amd64.
func debianPackage(p string) (string, bool) {
	m := debianName.FindStringSubmatch(p)
	if m == nil || m[2] != "" && m[2] != "amd64" && m[2] != "all" {
		return "", false
	}
	return m[1], true
}

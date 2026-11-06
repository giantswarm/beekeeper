package guard

import (
	"regexp"
	"slices"
	"strings"
)

// Deliberately a small query language, not a SQL read/write classifier.
// No expressions, user functions, locks, comments, subqueries or extra statements.
// Operators review exact queries (including the named relation) in config.
// Relations must be trusted tables or views without side-effecting functions.
var readOnlySelect = regexp.MustCompile(`(?i)^select (\*|count\(\*\)|[a-z_][a-z_0-9]*(, *[a-z_][a-z_0-9]*)*) from [a-z_][a-z_0-9]*(\.[a-z_][a-z_0-9]*)?( limit [0-9]+)?;?$`)

func (h Hook) readOnlyExec(args []string) string {
	i := slices.Index(args, "--")
	if i < 0 {
		return "explicit -- command separator required"
	}
	if slices.ContainsFunc(args[:i], interactiveFlag) {
		return "the exec must be non-interactive, without -i or -t"
	}
	cmd := args[i+1:]
	// A direct binary only. No shell, stdin, scripts, startup files, extra
	// commands or arbitrary client options (notably output pipes).
	if len(cmd) < 1 || cmd[0] != "psql" {
		return "only direct psql is allowed"
	}
	cmd = cmd[1:]
	if len(cmd) == 0 || cmd[0] != "-X" {
		return "psql -X is required to disable startup files"
	}
	cmd = cmd[1:]
	// Optional database/user names, never connection strings or options.
	for len(cmd) >= 2 && (cmd[0] == "-U" || cmd[0] == "-d") {
		if !execName.MatchString(cmd[1]) {
			return "database and user must be literal names"
		}
		cmd = cmd[2:]
	}
	if len(cmd) != 2 || cmd[0] != "-c" {
		return "expected exactly one psql -c query; other options are not allowed"
	}
	query := cmd[1]
	if !readOnlySelect.MatchString(query) {
		return "query is outside the restricted SELECT syntax"
	}
	if !slices.Contains(h.ReadOnlyExecQueries, query) {
		return "query is not in kube.readOnlyExecQueries"
	}
	return ""
}

// interactiveFlag: a kubectl flag that asks for stdin or a terminal, alone or
// in a short cluster. A terminal gives psql a pager and a pager a shell escape.
// A short flag's attached value can match too; that only refuses.
func interactiveFlag(a string) bool {
	name, _, _ := strings.Cut(a, "=")
	if name == "--stdin" || name == "--tty" {
		return true
	}
	return strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(name, "it")
}

var execName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9-]*$`)

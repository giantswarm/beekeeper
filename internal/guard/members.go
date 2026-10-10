package guard

import (
	"cmp"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ghAPISub is gh's api subcommand.
const ghAPISub = "api"

var (
	// membersPath: a REST path whose answer under the App's token an agent's
	// gh carries misclassifies a colleague: the org's members and a login's
	// membership (404 for a private member), the org's teams and a team's
	// members (403), a login's orgs (the public ones only). The groups are
	// the org and the login the path names.
	membersPath = regexp.MustCompile(`^(?:https?://api\.github\.com)?/?(?:` +
		`orgs/([^/?#]+)/(?:members|memberships)(?:/([^/?#]+))?/?` +
		`|orgs/([^/?#]+)/teams(?:/[^?#]*)?` +
		`|teams/[^/?#]+/(?:members|memberships)(?:/[^?#]*)?` +
		`|users/([^/?#]+)/orgs/?` +
		`)(?:[?#].*)?$`)
	// membersQuery: the same reads in a GraphQL body: an organization's
	// membersWithRole, its teams or a team's members, a user's organizations.
	membersQuery = regexp.MustCompile(`\bmembersWithRole\b|\borganization\s*\([\s\S]*?\b(?:teams?|members)\s*\(|\buser\s*\([\s\S]*?\borganizations\s*\(`)
	// ghAPIValued are gh api's flags that take the next word as their value.
	ghAPIValued = map[string]bool{"-X": true, "--method": true, "-H": true, "--header": true, "-f": true, "--raw-field": true,
		"-F": true, "--field": true, flagInput: true, "-t": true, flagTemplate: true, "-q": true, "--jq": true,
		"-p": true, "--preview": true, "--hostname": true, "--cache": true}
)

// membersRefusal says why cmd is refused in a session beekeeper started: a
// gh api call that reads org membership or teams, which the App's token the
// agent's gh carries answers for public memberships only, so a colleague
// reads as an outsider. The refusal names beekeeper person. A person's own
// session passes every call; the start is read only for a matching one.
func (h Hook) membersRefusal(cmd, session, cwd string) string {
	sc := scanShell(cmd)
	for _, sg := range sc.segments() {
		words := shellWords(sc.plain[sg.start:sg.end])
		ep := ghAPIEndpoint(words)
		if ep == "" {
			continue
		}
		var what, person string
		switch m := membersPath.FindStringSubmatch(ep); {
		case m != nil:
			what = "`gh api " + ep + "`"
			person = "beekeeper person " + cmp.Or(m[2], m[4], "<login>") + " --org " + cmp.Or(m[1], m[3], "<org>")
		case ep == "graphql" && membersQuery.MatchString(cmd[sg.start:]+"\n"+ghAPIBodies(words, cwd)):
			what, person = "this `gh api graphql` query", "beekeeper person <login> --org <org>"
		default:
			continue
		}
		if session == "" || h.Started == nil || !h.Started(session) {
			return ""
		}
		return "Refused: " + what + " reads membership with the App's token an agent's `gh` carries, which sees public " +
			"memberships only: its 404 for a private member reads as \"not a member\", and the team endpoints answer 403. " +
			"Read membership with `" + person + "`, which answers member, not a member, or permission missing from the " +
			"person's own login; a team's roster is a question to the supervisor."
	}
	return ""
}

// ghAPIEndpoint is the endpoint the words call gh api with, past the
// call's flags and their values, "" when they are no gh api call.
func ghAPIEndpoint(words []string) string {
	k := commandAt(words)
	if k+1 >= len(words) || path.Base(words[k]) != "gh" || words[k+1] != ghAPISub {
		return ""
	}
	for i := k + 2; i < len(words); i++ {
		switch w := words[i]; {
		case ghAPIValued[w]:
			i++
		case strings.HasPrefix(w, "-"):
		default:
			return w
		}
	}
	return ""
}

// ghAPIBodies is the text of the files a gh api call's words read a field
// from (-f query=@file, -F query=@file, --input file), under cwd when
// relative; an unreadable or large file adds nothing.
func ghAPIBodies(words []string, cwd string) string {
	var files []string
	for i, w := range words {
		switch {
		case w == flagInput && i+1 < len(words):
			files = append(files, words[i+1])
		case strings.HasPrefix(w, flagInput+"="):
			files = append(files, strings.TrimPrefix(w, flagInput+"="))
		default:
			if _, v, ok := strings.Cut(w, "=@"); ok {
				files = append(files, v)
			}
		}
	}
	var parts []string
	for _, f := range files {
		if f == "" || f == "-" {
			continue
		}
		f = expandHome(f)
		if !filepath.IsAbs(f) {
			f = filepath.Join(cwd, f)
		}
		if b, err := os.ReadFile(f); err == nil && len(b) < 1<<20 { //nolint:gosec // the call's own query file
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}

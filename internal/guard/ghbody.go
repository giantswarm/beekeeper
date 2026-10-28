package guard

import (
	"path"
	"strings"

	"github.com/giantswarm/beekeeper/internal/post"
)

const ghIssueSub = "issue"

// ghBodyVerbs are the gh pr and issue verbs whose bodies are checked for
// unlinked #n and owner/repo#n references.
var ghBodyVerbs = map[string]map[string]bool{
	"pr":       {verbCreate: true, verbEdit: true, verbComment: true},
	ghIssueSub: {verbCreate: true, verbEdit: true, verbComment: true},
}

// ghBodyRefusal refuses a gh pr or issue create, edit or comment command
// whose --body or --body-file names a bare #n or owner/repo#n without its
// full GitHub URL.
func (h Hook) ghBodyRefusal(cmd, cwd string) string {
	return h.scanGHBody(cmd, cwd, false, 0)
}

func (h Hook) scanGHBody(cmd, cwd string, forceUnknown bool, depth int) string {
	if depth > 4 {
		return ""
	}
	rawSc := scanShell(cmd)
	outerPlain, inners, hadSubst := opaquePlain(rawSc.plain)
	sc := scanShell(outerPlain)
	sc.heredocs = rawSc.heredocs
	segs := sc.segments()
	vars := scanVars(sc, segs)
	states := walkSegments(sc, cwd, h.Upstream.Git != nil)
	anyCD := hasAnyCD(rawSc, 0) || hasAnyCD(sc, 0)
	for _, in := range inners {
		if hasAnyCD(scanShell(in), 0) {
			anyCD = true
		}
	}
	lineMoved := (!isPlain(sc) || hadSubst) && anyCD

	for _, in := range inners {
		if r := h.scanGHBody(in, cwd, lineMoved || forceUnknown, depth+1); r != "" {
			return r
		}
	}

	var docs []string
	for _, hd := range rawSc.heredocs {
		docs = append(docs, cmd[hd.start:hd.end])
	}

	for _, st := range states {
		sg := st.seg
		rawWords := shellWords(sc.plain[sg.start:sg.end])
		known := st.known && !lineMoved && !forceUnknown
		for _, words := range expandWordsList(rawWords, vars) {
			if r := h.checkGHWords(words, st.dir, known, cmd, docs); r != "" {
				return r
			}
			if envCmd := envSplitCommand(words); envCmd != "" {
				if r := h.scanGHBody(envCmd, st.dir, !known, depth+1); r != "" {
					return r
				}
			}
			if flockCmd := flockCommand(words); flockCmd != "" {
				if r := h.scanGHBody(flockCmd, st.dir, !known, depth+1); r != "" {
					return r
				}
			}
			if k := commandAt(words); k == len(words) || !shells[path.Base(words[k])] {
				continue
			}
			for _, arg := range words[1:] {
				if strings.ContainsAny(arg, " \t\n") {
					if r := h.scanGHBody(arg, st.dir, !known, depth+1); r != "" {
						return r
					}
				}
			}
			for _, hd := range sc.heredocs {
				if hd.op >= sg.start && hd.op < sg.end {
					if r := h.scanGHBody(cmd[hd.start:hd.end], st.dir, !known, depth+1); r != "" {
						return r
					}
				}
			}
		}
	}
	return ""
}

func (h Hook) checkGHWords(words []string, cwd string, known bool, fullCmd string, docs []string) string {
	k := commandAt(words)
	if k == len(words) || path.Base(words[k]) != "gh" {
		return ""
	}
	env := ""
	for _, w := range words[:k] {
		if v, ok := strings.CutPrefix(w, "GH_REPO="); ok {
			env = v
		}
	}
	args := words[k+1:]
	var pos, bodies, files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if _, ok := repoFlag(a); ok {
			continue
		}
		if v, ok := strings.CutPrefix(a, "--body="); ok {
			bodies = append(bodies, v)
			continue
		}
		if v, ok := strings.CutPrefix(a, "-b"); ok && v != "" && !strings.HasPrefix(a, "--") {
			bodies = append(bodies, strings.TrimPrefix(v, "="))
			continue
		}
		if v, ok := strings.CutPrefix(a, flagBodyFile+"="); ok {
			files = append(files, v)
			continue
		}
		if v, ok := strings.CutPrefix(a, "-F"); ok && v != "" && !strings.HasPrefix(a, "--") {
			files = append(files, strings.TrimPrefix(v, "="))
			continue
		}
		if ghValued[a] {
			if i+1 < len(args) {
				switch a {
				case "-b", "--body":
					bodies = append(bodies, args[i+1])
				case "-F", flagBodyFile:
					files = append(files, args[i+1])
				}
			}
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 || !ghBodyVerbs[pos[0]][pos[1]] || (len(bodies) == 0 && len(files) == 0) {
		return ""
	}
	var parts []string
	sub := false
	for _, b := range bodies {
		if strings.Contains(b, unknownMarker) {
			sub = true
			b = strings.ReplaceAll(b, unknownMarker, " ")
		}
		parts = append(parts, b)
	}
	for _, f := range files {
		if f == "-" {
			sub = true
			continue
		}
		parts = append(parts, readFile(f, cwd))
	}
	if sub {
		parts = append(parts, docs...)
		parts = append(parts, catFiles(fullCmd, cwd))
	}
	what, repo := h.Upstream.ghWrite(args, cwd, env, known)
	if what == "" {
		what = "gh " + pos[0] + " " + pos[1]
	}
	problems := post.Unlinked(strings.Join(parts, "\n"), repoSlug(repo))
	if len(problems) == 0 {
		return ""
	}
	return "Refused: the body of `" + what + "` names an issue or pull request without its full URL: " + strings.Join(problems, "; ") + "."
}

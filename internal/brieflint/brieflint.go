// Package brieflint finds what makes a skill or a brief go stale: dates,
// waits on something to ship, notes on a release that fixed something,
// a role's run where the role's name belongs, and a shell example whose
// bare > a rerun under noclobber refuses. Skills and briefs say the
// current state; their history lives in the issues and the changelog.
package brieflint

import (
	"bufio"
	"bytes"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/giantswarm/beekeeper/internal/post"
	"mvdan.cc/sh/v3/syntax"
)

// Finding is one line a rule refuses.
type Finding struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Rule string `json:"rule"`
	// Why says what to write instead.
	Why  string `json:"why"`
	Text string `json:"text"`
}

// rule is one pattern a line must not match.
type rule struct {
	name, why string
	re        *regexp.Regexp
}

const versionRe = `v\d+\.\d+(\.\d+)?`

var rules = []rule{
	{"date", "a dated line: say what holds now, the date belongs in the log",
		// A date on its own, not one inside a file name or a version.
		regexp.MustCompile(`(^|[\s(\[])(19|20)\d\d-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])($|[\s).,;:\]])`)},
	{"until-ships", `a wait on something to ship: say what to do now, and change the line when it ships`,
		regexp.MustCompile(`(?i)\buntil\b[^.;\n]{1,120}?\b(ships|shipped|lands|landed|merges|is merged|is released|is fixed|is installed|is out|is live|goes live)\b`)},
	{"fixed-in", "a note on the release that changed something: describe the behaviour, not its history",
		regexp.MustCompile(`(?i)\(\s*(beekeeper |devctl )?` + versionRe + `\s*:|\b(since|as of|starting with|fixed in) (beekeeper |devctl )?` + versionRe)},
	{"workaround", "a workaround: once the fix ships the line is wrong; describe the way that works",
		regexp.MustCompile(`(?i)\bwork-?arounds?\b|\bfor now\b|\btemporar(y|ily)\b`)},
	{"role-run", `a role's run: a relay makes it stale; address "the supervisor" or "the guide"`,
		regexp.MustCompile(`\b(Supervisor|Guide) run \d+\b`)},
}

// clobber is the rule on a shell example's redirect: a bare > into a file
// fails on a rerun under noclobber (zsh's included), the command never
// runs, and the first run's file is read back as this run's output.
const (
	clobberRule = "clobber"
	clobberWhy  = "a bare > into a file fails on a rerun under noclobber: write >| or a fresh name per attempt"
	fullURLRule = "full-url"
)

var (
	// fence opens a fenced code block; its info string names the language.
	fence      = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([A-Za-z]*)\\s*(.*)$")
	fenceClose = regexp.MustCompile("^\\s*(```+|~~~+)\\s*$")
	// codeSpan is one inline code span.
	codeSpan = regexp.MustCompile("`+[^`]+`+")
	// placeholder is a <word> to fill in, which closes with > and is no redirect.
	placeholder = regexp.MustCompile(`<[A-Za-z_][A-Za-z0-9_./-]*>`)
	// inlineComparison preserves prose comparisons without exempting numeric file names.
	inlineComparison = regexp.MustCompile(`^\s*[^\s]+\s+(>\s+[0-9]+|>=\s+[^\s]+)\s*$`)
	// shellFences are the info strings whose blocks are shell examples.
	shellFences = map[string]bool{"": true, "sh": true, "bash": true, "zsh": true, "shell": true, "console": true}
)

// clobberLines parses a complete example, preserving shell lexical context.
// Parse failures are findings too: an incomplete example cannot prove that
// its apparent redirects are safe. No example is ever executed or expanded.
func clobberLines(code string, inline bool) map[int]string {
	out := make(map[int]string)
	if inline && inlineComparison.MatchString(code) {
		return out
	}
	normalized := placeholder.ReplaceAllString(code, "PLACEHOLDER")
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(normalized), "")
	if err != nil {
		for i, line := range strings.Split(code, "\n") {
			if strings.Contains(line, ">") {
				out[i+1] = "cannot verify redirects in incomplete or unsupported shell syntax: complete the example and use >| or a fresh name per attempt"
			}
		}
		return out
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		r, ok := n.(*syntax.Redirect)
		if !ok || (r.Op != syntax.RdrOut && r.Op != syntax.RdrAll) {
			return true
		}
		if len(r.Word.Parts) == 1 {
			if _, ok := r.Word.Parts[0].(*syntax.ProcSubst); ok {
				return true
			}
		}
		if strings.HasPrefix(literalWord(r.Word.Parts), "/dev/") {
			return true
		}
		out[int(r.OpPos.Line())] = clobberWhy
		return true
	})
	return out
}

func literalWord(parts []syntax.WordPart) string {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			b.WriteString(literalWord(p.Parts))
		default:
			return ""
		}
	}
	return b.String()
}

// Lint reads one file's lines and returns what the rules refuse in them.
// The clobber rule reads only shell examples: inline code spans and fenced
// blocks without a language or with a shell's.
func Lint(name string, r io.Reader) ([]Finding, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	here := post.SingleRepo(text)
	unlinked := post.UnlinkedLines(text, here)
	var out []Finding
	var inFence, shellFence bool
	var fenceMark string
	var block []string
	var blockStart int
	flush := func() {
		for line, why := range clobberLines(strings.Join(block, "\n"), false) {
			out = append(out, Finding{Path: name, Line: blockStart + line - 1, Rule: clobberRule, Why: why, Text: strings.TrimSpace(block[line-1])})
		}
		block = nil
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		add := func(rule, why string) {
			out = append(out, Finding{Path: name, Line: n, Rule: rule, Why: why, Text: strings.TrimSpace(line)})
		}
		for _, ru := range rules {
			if ru.re.MatchString(line) {
				add(ru.name, ru.why)
			}
		}
		if n-1 < len(unlinked) {
			for _, why := range unlinked[n-1] {
				add(fullURLRule, why)
			}
		}
		switch m := fence.FindStringSubmatch(line); {
		case m != nil && !inFence && (m[1][0] == '~' || !strings.ContainsRune(m[2]+m[3], '`')):
			inFence, fenceMark, shellFence = true, m[1], shellFences[strings.ToLower(m[2])]
			blockStart = n + 1
			continue
		case inFence && fenceClose.MatchString(line) && strings.HasPrefix(strings.TrimSpace(line), fenceMark):
			if shellFence {
				flush()
			}
			inFence = false
			continue
		case inFence && shellFence:
			block = append(block, line)
		case !inFence:
			for _, span := range codeSpan.FindAllString(line, -1) {
				if found := clobberLines(strings.Trim(span, "`"), true); len(found) > 0 {
					for _, why := range found {
						add(clobberRule, why)
						break
					}
					break
				}
			}
		}
	}
	if inFence && shellFence {
		flush()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out, sc.Err()
}

// LintFS lints root in fsys: the file, or every Markdown file below the
// folder. Paths are named as fsys names them, joined to prefix.
func LintFS(fsys fs.FS, root, prefix string) ([]Finding, error) {
	var out []Finding
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if p != root && !strings.EqualFold(path.Ext(p), ".md") {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		found, err := Lint(path.Join(prefix, p), f)
		out = append(out, found...)
		return err
	})
	return out, err
}

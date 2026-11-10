// Package brieflint finds what makes a skill or a brief go stale: dates,
// waits on something to ship, notes on a release that fixed something,
// a role's run where the role's name belongs, and a shell example whose
// bare > a rerun under noclobber refuses. Skills and briefs say the
// current state; their history lives in the issues and the changelog.
package brieflint

import (
	"bufio"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/giantswarm/beekeeper/internal/post"
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
		// A period is punctuation only when it does not introduce an extension.
		regexp.MustCompile(`(^|[\s(\[])(19|20)\d\d-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])($|[\s),;:\]]|\.($|[^\pL\pM\pN_-]))`)},
	{"until-ships", `a wait on something to ship: say what to do now, and change the line when it ships`,
		// A period before a word character belongs to a version or dotted name.
		regexp.MustCompile(`(?i)\buntil\b(?:[^.;\n]|\.\b){1,120}?\b(ships|shipped|lands|landed|merges|is merged|is released|is fixed|is installed|is out|is live|goes live)\b`)},
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
	// placeholder is a <word> to fill in, which closes with > and is no redirect.
	placeholder = regexp.MustCompile(`<[^<>\s][^<>\r\n]*[^<>\s]>|<[^<>\s]>`)
	// redirect is the conservative fallback for incomplete shell examples.
	redirect   = regexp.MustCompile(`(?:^|[^<>=&|\-])(?:[0-9]?>|&>)[ \t]*([^\s>|&=;)` + "`" + `]+)`)
	number     = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
	descriptor = regexp.MustCompile(`^(?:[0-9]+-?|-)$`)
	// shellFences are the info strings whose blocks are shell examples.
	shellFences = map[string]bool{"": true, "sh": true, "bash": true, "zsh": true, "shell": true, "console": true}
)

func unsafeTarget(target string, inline bool) bool {
	target = strings.Trim(target, `"'`)
	return target != "" && !strings.HasPrefix(target, "/dev/") &&
		!strings.HasPrefix(target, "(") && !strings.HasPrefix(target, ">(") &&
		(!inline || !number.MatchString(target))
}

// redirectLines reads a complete example so quotes, comments and here-documents
// stay data, while nested command substitutions are still checked. It only
// parses text: shell commands, expansions and file targets are never evaluated.
func redirectLines(code string, lang syntax.LangVariant, inline bool) []int {
	code = placeholder.ReplaceAllStringFunc(code, func(s string) string {
		return "P" + strings.Repeat("\n", strings.Count(s, "\n"))
	})
	f, err := syntax.NewParser(syntax.Variant(lang), syntax.RecoverErrors(4)).Parse(strings.NewReader(code), "")
	var out []int
	if err != nil {
		// Briefs also contain incomplete examples. Keep checking those rather
		// than silently accepting every redirect after a parse error.
		for i, line := range strings.Split(code, "\n") {
			for _, m := range redirect.FindAllStringSubmatch(line, -1) {
				if unsafeTarget(m[1], inline) {
					out = append(out, i+1)
					break
				}
			}
		}
		return out
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		r, ok := n.(*syntax.Redirect)
		if !ok || r.Word == nil || r.Op != syntax.RdrOut && r.Op != syntax.RdrAll && (r.Op != syntax.DplOut || r.N != nil) {
			return true
		}
		pos := r.OpPos.Offset()
		if pos >= uint(len(code)) {
			return true
		}
		if inline && (pos > 0 && strings.ContainsRune("-=", rune(code[pos-1])) || pos+1 < uint(len(code)) && code[pos+1] == '=') {
			// Inline spans also hold comparisons and arrows, not only shell.
			return true
		}
		start, end := r.Word.Pos().Offset(), r.Word.End().Offset()
		if start > end || end > uint(len(code)) {
			return true
		}
		target := strings.Trim(code[start:end], `"'`)
		// With no explicit source descriptor, >&file redirects both streams.
		// Numeric descriptors, moves and closure remain descriptor operations.
		if r.Op == syntax.DplOut && descriptor.MatchString(target) {
			return true
		}
		if unsafeTarget(target, inline && r.Op != syntax.DplOut) {
			out = append(out, int(r.OpPos.Line()))
		}
		return true
	})
	return out
}

// shellClobberLines keeps fenced examples whole, including multiline strings
// and here-documents, and maps each unsafe operator to its document line.
func shellClobberLines(text string) map[int]bool {
	out := make(map[int]bool)
	add := func(code string, first int, lang syntax.LangVariant, inline bool) {
		for _, n := range redirectLines(code, lang, inline) {
			out[first+n-1] = true
		}
	}
	lines := strings.Split(text, "\n")
	var inFence, shellFence bool
	var mark string
	var start int
	lang := syntax.LangBash
	for i, line := range lines {
		switch m := fence.FindStringSubmatch(line); {
		case m != nil && !inFence && (m[1][0] == '~' || !strings.ContainsRune(m[2]+m[3], '`')):
			inFence, mark, shellFence = true, m[1], shellFences[strings.ToLower(m[2])]
			start, lang = i+1, syntax.LangBash
			if strings.EqualFold(m[2], "zsh") {
				lang = syntax.LangZsh
			}
		case inFence && fenceClose.MatchString(line) && strings.HasPrefix(strings.TrimSpace(line), mark):
			if shellFence {
				add(strings.Join(lines[start:i], "\n"), start+1, lang, false)
			}
			inFence = false
		case !inFence:
			for _, span := range post.InlineCodeRanges(text) {
				first := strings.Count(text[:span[0]], "\n")
				if first == i {
					add(text[span[0]:span[1]], i+1, syntax.LangBash, true)
				}
			}
		}
	}
	if inFence && shellFence {
		add(strings.Join(lines[start:], "\n"), start+1, lang, false)
	}
	return out
}

// Lint reads one file's lines and returns what the rules refuse in them.
// The clobber rule reads only shell examples: inline code spans and fenced
// blocks without a language or with a shell's.
func Lint(name string, r io.Reader) ([]Finding, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	text := WithoutData(string(raw))
	here := post.SingleRepo(text)
	unlinked := post.UnlinkedLines(text, here)
	clobber := shellClobberLines(text)
	var out []Finding
	sc := bufio.NewScanner(strings.NewReader(text))
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
		if clobber[n] {
			add(clobberRule, clobberWhy)
		}
	}
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

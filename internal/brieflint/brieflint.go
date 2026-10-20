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
)

var (
	// fence opens or closes a fenced code block; its info string names the language.
	fence = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([A-Za-z]*)")
	// codeSpan is one inline code span.
	codeSpan = regexp.MustCompile("`+[^`]+`+")
	// placeholder is a <word> to fill in, which closes with > and is no redirect.
	placeholder = regexp.MustCompile(`<[^<>\s][^<>]*[^<>\s]>|<[^<>\s]>`)
	// redirect is a > into a target: not >>, >|, >&, >=, -> or =>.
	redirect = regexp.MustCompile(`(?:^|[^<>=&|\-])[0-9]?>[ \t]*([^\s>|&=;)` + "`" + `]+)`)
	// shellFences are the info strings whose blocks are shell examples.
	shellFences = map[string]bool{"": true, "sh": true, "bash": true, "zsh": true, "shell": true, "console": true}
)

// clobbers says whether shell code redirects with a bare > into a file.
func clobbers(code string) bool {
	for _, m := range redirect.FindAllStringSubmatch(placeholder.ReplaceAllString(code, "P"), -1) {
		t := strings.TrimLeft(m[1], `"'`)
		switch {
		case strings.HasPrefix(t, "/dev/"), strings.HasPrefix(t, "("), t == "" || t[0] >= '0' && t[0] <= '9':
			// A device, a process substitution, a comparison with a number.
		default:
			return true
		}
	}
	return false
}

// Lint reads one file's lines and returns what the rules refuse in them.
// The clobber rule reads only shell examples: inline code spans and fenced
// blocks without a language or with a shell's.
func Lint(name string, r io.Reader) ([]Finding, error) {
	var out []Finding
	var inFence, shellFence bool
	var fenceMark string
	sc := bufio.NewScanner(r)
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
		var code string
		switch m := fence.FindStringSubmatch(line); {
		case m != nil && !inFence:
			inFence, fenceMark, shellFence = true, m[1], shellFences[strings.ToLower(m[2])]
			continue
		case m != nil && strings.HasPrefix(m[1], fenceMark) && m[2] == "":
			inFence = false
			continue
		case inFence && shellFence:
			code = line
		case !inFence:
			code = strings.Join(codeSpan.FindAllString(line, -1), " ")
		}
		if clobbers(code) {
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

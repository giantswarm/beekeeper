// Package brieflint finds what makes a skill or a brief go stale: dates,
// waits on something to ship, notes on a release that fixed something, and
// a role's run where the role's name belongs. Skills and briefs say the
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

// Lint reads one file's lines and returns what the rules refuse in them.
func Lint(name string, r io.Reader) ([]Finding, error) {
	var out []Finding
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		for _, ru := range rules {
			if ru.re.MatchString(line) {
				out = append(out, Finding{Path: name, Line: n, Rule: ru.name, Why: ru.why, Text: strings.TrimSpace(line)})
			}
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

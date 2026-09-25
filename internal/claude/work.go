package claude

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Work is what a session's recent transcript is about: the GitHub issues and
// pull requests it mentions and the repositories it works in, most recent
// first. It is read from the transcript's tail, so a brief read hours ago
// does not count and the session's latest turns do.
type Work struct {
	Repos []string `json:"repos,omitempty"`
	Refs  []string `json:"refs,omitempty"`
}

// Current is how many of the most recent refs count as what a session is on
// now.
const Current = 3

// workWindow is how much of a transcript's end ReadWork scans.
const workWindow = 512 << 10

// ReadWork scans the tail of the transcript at path.
func ReadWork(path string) Work {
	buf, _ := readWindow(path)
	return scanWork(string(buf))
}

// readWindow returns the last workWindow bytes of the transcript at path
// and whether they are all of it.
func readWindow(path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, false
	}
	off := max(fi.Size()-workWindow, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, false
	}
	return buf, off == 0
}

// scanWork finds "github.com/o/r/pull/N", "github.com/o/r/issues/N",
// "o/r#N", "github.com/o/r" and "--repo o/r" in one linear pass per form
// (regular expressions without a literal prefix are too slow for the
// megabytes of transcript a sessions call reads).
func scanWork(text string) Work {
	refs := map[string]int{}
	repos := map[string]int{}
	note := func(m map[string]int, k string, pos int) {
		if p, ok := m[k]; !ok || pos > p {
			m[k] = pos
		}
	}
	for i := 0; ; {
		j := strings.Index(text[i:], "github.com")
		if j < 0 {
			break
		}
		at := i + j
		i = at + len("github.com")
		if i >= len(text) || (text[i] != '/' && text[i] != ':') {
			continue
		}
		repo, n := ownerRepo(text[i+1:])
		if repo == "" {
			continue
		}
		note(repos, repo, at)
		rest := text[i+1+n:]
		for _, kind := range []string{"/pull/", "/issues/"} {
			if num := leadingDigits(strings.TrimPrefix(rest, kind)); strings.HasPrefix(rest, kind) && num != "" {
				note(refs, repo+"#"+num, at)
			}
		}
	}
	for i := 0; ; {
		j := strings.Index(text[i:], "--repo")
		if j < 0 {
			break
		}
		at := i + j
		i = at + len("--repo")
		if i < len(text) && (text[i] == ' ' || text[i] == '=') {
			if repo, _ := ownerRepo(text[i+1:]); repo != "" {
				note(repos, repo, at)
			}
		}
	}
	for i := 0; ; {
		j := strings.IndexByte(text[i:], '#')
		if j < 0 {
			break
		}
		at := i + j
		i = at + 1
		num := leadingDigits(text[i:])
		if num == "" || (i+len(num) < len(text) && isWordEnd(text[i+len(num)])) {
			continue
		}
		if repo := ownerRepoBefore(text[:at]); repo != "" {
			note(refs, repo+"#"+num, at)
			note(repos, repo, at)
		}
	}
	return Work{Repos: byRecency(repos), Refs: byRecency(refs)}
}

func isOwnerChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

func isRepoChar(c byte) bool { return isOwnerChar(c) || c == '_' || c == '.' }

// isWordEnd reports whether c continues a number's word ("#12abc", "#12_x");
// a full stop ends it.
func isWordEnd(c byte) bool { return c == '_' || (isOwnerChar(c) && c != '-') }

// ownerRepo reads "owner/repo" at the start of s and returns it with its
// length.
func ownerRepo(s string) (string, int) {
	o := 0
	for o < len(s) && isOwnerChar(s[o]) {
		o++
	}
	if o == 0 || o >= len(s) || s[o] != '/' {
		return "", 0
	}
	r := o + 1
	for r < len(s) && isRepoChar(s[r]) {
		r++
	}
	repo := strings.TrimSuffix(strings.TrimRight(s[o+1:r], "."), ".git")
	if repo == "" {
		return "", 0
	}
	return s[:o] + "/" + repo, r
}

// ownerRepoBefore reads "owner/repo" ending at the end of s.
func ownerRepoBefore(s string) string {
	r := len(s)
	for r > 0 && isRepoChar(s[r-1]) {
		r--
	}
	if r == len(s) || r == 0 || s[r-1] != '/' {
		return ""
	}
	o := r - 1
	for o > 0 && isOwnerChar(s[o-1]) {
		o--
	}
	if o == r-1 || (o > 0 && (isRepoChar(s[o-1]) || s[o-1] == '/')) {
		return ""
	}
	return s[o:r-1] + "/" + s[r:]
}

func leadingDigits(s string) string {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return s[:n]
}

func byRecency(pos map[string]int) []string {
	out := make([]string, 0, len(pos))
	for k := range pos {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b string) int {
		if pos[a] != pos[b] {
			return pos[b] - pos[a]
		}
		return strings.Compare(a, b)
	})
	return out
}

// Overlap is something two or more sessions are on at once.
type Overlap struct {
	// Kind is "ref" (the same issue or pull request) or "repo".
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Sessions []string `json:"sessions"`
}

// OverlapOptions narrows what counts as an overlap.
type OverlapOptions struct {
	// Skip leaves a session out (the supervisor mentions everything).
	Skip func(*Session) bool
	// Ignore lists repositories whose refs and name form no overlap:
	// issue trackers and notebooks every session writes to.
	Ignore []string
	// ActiveSince leaves out sessions idle since before it: they are not
	// on anything now.
	ActiveSince time.Time
}

// Primary is the repository a session works in now: the most recently
// mentioned one outside ignore.
func (w Work) Primary(ignore []string) string {
	for _, r := range w.Repos {
		if !slices.Contains(ignore, r) {
			return r
		}
	}
	return ""
}

// CurrentRefs are the most recent refs outside ignore.
func (w Work) CurrentRefs(ignore []string) []string {
	var out []string
	for _, r := range w.Refs {
		if repo, _, _ := strings.Cut(r, "#"); !slices.Contains(ignore, repo) {
			out = append(out, r)
			if len(out) == Current {
				break
			}
		}
	}
	return out
}

// Overlaps finds the refs and repositories that more than one session is on
// now.
func Overlaps(sessions []*Session, work map[int]Work, o OverlapOptions) []Overlap {
	byRef := map[string][]string{}
	byRepo := map[string][]string{}
	for _, s := range sessions {
		if (o.Skip != nil && o.Skip(s)) || s.LastActive.Before(o.ActiveSince) {
			continue
		}
		w := work[s.PID]
		for _, r := range w.CurrentRefs(o.Ignore) {
			byRef[r] = append(byRef[r], s.Name)
		}
		if p := w.Primary(o.Ignore); p != "" {
			byRepo[p] = append(byRepo[p], s.Name)
		}
	}
	var out []Overlap
	for k, v := range byRef {
		if len(v) > 1 {
			out = append(out, Overlap{Kind: "ref", Key: k, Sessions: v})
		}
	}
	for k, v := range byRepo {
		if len(v) > 1 {
			out = append(out, Overlap{Kind: "repo", Key: k, Sessions: v})
		}
	}
	slices.SortFunc(out, func(a, b Overlap) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind) // "ref" before "repo"
		}
		return strings.Compare(a.Key, b.Key)
	})
	for i := range out {
		slices.Sort(out[i].Sessions)
	}
	return out
}

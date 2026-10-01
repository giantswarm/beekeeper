package guard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The value scanner matches secret values, not command shapes: beekeeper
// keeps keyed fingerprints (HMAC-SHA256 under a key only beekeeper reads)
// of every value it can reach, each naming the reference to rotate, and
// fingerprints the candidate strings of a text against them. The index
// never holds a value, and a hit names the reference, never the match.

// MinSecretLen is the default shortest value the index takes: shorter ones
// (ports, flags, words) would match ordinary output.
const MinSecretLen = 12

// maxSeparators bounds the separators a token is split at, so that a long
// path or URL costs a bounded number of candidates.
const maxSeparators = 16

const (
	indexKeyFile  = "key"
	indexFile     = "index.json"
	redactedOpen  = "[redacted: "
	redactedClose = "]"
)

// Index is the fingerprint index in its directory.
type Index struct {
	dir string
	key []byte
	// MinLen is the shortest value Add takes.
	MinLen int
	data   indexData
}

type indexData struct {
	Built time.Time `json:"built,omitzero"`
	// Entries maps a fingerprint (hex) to the reference it names.
	Entries map[string]string `json:"entries"`
	// Lengths are the byte lengths of the indexed forms: a candidate of
	// another length is not fingerprinted.
	Lengths []int `json:"lengths"`
}

// OpenIndex reads the index in dir, creating its key on first use. The key
// and the index are readable by the user only.
func OpenIndex(dir string) (*Index, error) {
	ix, err := LoadIndex(dir)
	if err != nil {
		return nil, err
	}
	if ix.key != nil {
		return ix, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, indexKeyFile), key, 0o600); err != nil {
		return nil, err
	}
	ix.key = key
	return ix, nil
}

// LoadIndex reads the index in dir without creating anything: with no key
// yet, the index is empty and matches nothing.
func LoadIndex(dir string) (*Index, error) {
	ix := &Index{dir: dir, MinLen: MinSecretLen, data: indexData{Entries: map[string]string{}}}
	key, err := os.ReadFile(filepath.Join(dir, indexKeyFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ix, nil
	case err != nil:
		return nil, err
	case len(key) < 16:
		return nil, fmt.Errorf("%s: the index key is too short", filepath.Join(dir, indexKeyFile))
	}
	ix.key = key
	raw, err := os.ReadFile(filepath.Join(dir, indexFile))
	if errors.Is(err, fs.ErrNotExist) {
		return ix, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &ix.data); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, indexFile), err)
	}
	if ix.data.Entries == nil {
		ix.data.Entries = map[string]string{}
	}
	return ix, nil
}

// Len is the number of fingerprints in the index.
func (ix *Index) Len() int { return len(ix.data.Entries) }

// Refs are the distinct references in the index, sorted.
func (ix *Index) Refs() []string {
	seen := map[string]bool{}
	for _, r := range ix.data.Entries {
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Built is when the index was last saved.
func (ix *Index) Built() time.Time { return ix.data.Built }

// Add indexes value under ref: the value itself, its base64 forms, each of
// its lines, and for a value that is base64 text its decoded form. It
// returns the number of forms added; a value shorter than MinLen adds none.
func (ix *Index) Add(ref, value string) int {
	if ix.key == nil {
		return 0
	}
	n := 0
	for _, f := range forms(strings.TrimSpace(value), ix.MinLen) {
		fp := ix.fingerprint(f)
		if _, ok := ix.data.Entries[fp]; !ok {
			n++
		}
		ix.data.Entries[fp] = ref
		if !slices.Contains(ix.data.Lengths, len(f)) {
			ix.data.Lengths = append(ix.data.Lengths, len(f))
		}
	}
	return n
}

// Drop removes every entry whose reference starts with one of prefixes.
func (ix *Index) Drop(prefixes ...string) {
	for fp, ref := range ix.data.Entries {
		for _, p := range prefixes {
			if strings.HasPrefix(ref, p) {
				delete(ix.data.Entries, fp)
				break
			}
		}
	}
}

// Save writes the index, readable by the user only.
func (ix *Index) Save(now time.Time) error {
	if ix.key == nil {
		return errors.New("the index has no key: open it with OpenIndex")
	}
	sort.Ints(ix.data.Lengths)
	ix.data.Built = now.UTC()
	raw, err := json.Marshal(ix.data)
	if err != nil {
		return err
	}
	tmp := filepath.Join(ix.dir, indexFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(ix.dir, indexFile))
}

func (ix *Index) fingerprint(s string) string {
	m := hmac.New(sha256.New, ix.key)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

// forms are the strings a value can show up as in a tool's output.
func forms(v string, minLen int) []string {
	if len(v) < minLen {
		return nil
	}
	out := []string{v,
		base64.StdEncoding.EncodeToString([]byte(v)),
		base64.RawStdEncoding.EncodeToString([]byte(v)),
		base64.URLEncoding.EncodeToString([]byte(v)),
		base64.RawURLEncoding.EncodeToString([]byte(v))}
	if strings.ContainsRune(v, '\n') {
		// Each line, and of a key: value or KEY=value line its value: a
		// configuration file in one secret.
		for l := range strings.Lines(v) {
			l = strings.TrimSpace(l)
			if len(l) >= minLen {
				out = append(out, l)
			}
			for _, sep := range []string{": ", "="} {
				if _, val, ok := strings.Cut(l, sep); ok {
					if val = strings.Trim(strings.TrimSpace(val), `"'`); len(val) >= minLen {
						out = append(out, val)
					}
				}
			}
		}
	}
	if d, err := base64.StdEncoding.DecodeString(v); err == nil && utf8.Valid(d) {
		if s := strings.TrimSpace(string(d)); len(s) >= minLen {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// A Finding is what a text carried: an indexed reference or a token
// pattern's rule id, and how often.
type Finding struct {
	// Ref is the indexed reference; empty for a pattern hit.
	Ref string `json:"ref,omitempty"`
	// Rule is the token pattern's rule id; empty for an index hit.
	Rule  string `json:"rule,omitempty"`
	Count int    `json:"count"`
}

// Name is the reference, or "pattern <rule>".
func (f Finding) Name() string {
	if f.Ref != "" {
		return f.Ref
	}
	return "pattern " + f.Rule
}

type span struct {
	start, end int
	name       string
	ref        bool
}

// Redact replaces every indexed value and every token pattern match in s
// with a marker naming its reference or rule, and returns what it found. A
// line marked gitleaks:allow keeps its pattern matches, not its indexed
// values. An Index without a key matches patterns only.
func (ix *Index) Redact(s string) (string, []Finding) {
	// An indexed value wins over a pattern match it overlaps, since it names
	// what to rotate; among either, the widest span starting first wins.
	spans := disjoint(ix.matches(s), nil)
	var patterns []span
	for _, r := range tokenRules {
		if !slices.ContainsFunc(r.keywords, func(k string) bool { return strings.Contains(s, k) }) {
			continue
		}
		for _, m := range r.re.FindAllStringIndex(s, -1) {
			if r.id == urlCredentials {
				m[1]-- // the pattern ends on the host's first character
			}
			if !allowed(s, m[0]) {
				patterns = append(patterns, span{m[0], m[1], r.id, false})
			}
		}
	}
	spans = disjoint(patterns, spans)
	if len(spans) == 0 {
		return s, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	counts := map[span]int{}
	var order []span
	at := 0
	for _, sp := range spans {
		b.WriteString(s[at:sp.start])
		b.WriteString(redactedOpen + sp.name + redactedClose)
		at = sp.end
		k := span{name: sp.name, ref: sp.ref}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	b.WriteString(s[at:])
	out := make([]Finding, 0, len(order))
	for _, k := range order {
		f := Finding{Count: counts[k]}
		if k.ref {
			f.Ref = k.name
		} else {
			f.Rule = k.name
		}
		out = append(out, f)
	}
	return b.String(), out
}

// urlCredentials is the rule of a password in a URL.
const urlCredentials = "url-credentials"

// disjoint adds to kept the spans of add, widest first at each start, that
// overlap neither kept nor each other.
func disjoint(add, kept []span) []span {
	sort.Slice(add, func(i, j int) bool {
		if add[i].start != add[j].start {
			return add[i].start < add[j].start
		}
		return add[i].end > add[j].end
	})
	for _, sp := range add {
		if !slices.ContainsFunc(kept, func(k span) bool { return sp.start < k.end && k.start < sp.end }) {
			kept = append(kept, sp)
		}
	}
	return kept
}

// matches are the spans of s whose fingerprint is in the index.
func (ix *Index) matches(s string) []span {
	if ix.key == nil || len(ix.data.Entries) == 0 {
		return nil
	}
	lengths := map[int]bool{}
	for _, n := range ix.data.Lengths {
		lengths[n] = true
	}
	minLen := slices.Min(ix.data.Lengths)
	var out []span
	for _, t := range tokens(s) {
		if t[1]-t[0] < minLen {
			continue
		}
		for _, c := range candidates(s, t[0], t[1]) {
			if !lengths[c[1]-c[0]] {
				continue
			}
			if ref, ok := ix.data.Entries[ix.fingerprint(s[c[0]:c[1]])]; ok {
				out = append(out, span{c[0], c[1], ref, true})
			}
		}
	}
	return out
}

// tokenBreak ends a token: whitespace, quotes, brackets and list
// separators, none of which a generated secret carries.
func tokenBreak(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', '"', '\'', '`', '<', '>', '(', ')', '[', ']', '{', '}', ',', ';', '|', '\\':
		return true
	}
	return false
}

// tokens are the byte spans of s between token breaks.
func tokens(s string) [][2]int {
	var out [][2]int
	start := -1
	for i, r := range s {
		if tokenBreak(r) {
			if start >= 0 {
				out = append(out, [2]int{start, i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(s)})
	}
	return out
}

// candidates are the spans of the token s[start:end] between any two of its
// separators (= : @ /) or its ends, a trailing full stop dropped too: the
// value in KEY=value, user:value@host or a path segment.
func candidates(s string, start, end int) [][2]int {
	cuts := []int{start - 1}
	for i := start; i < end && len(cuts) <= maxSeparators; i++ {
		switch s[i] {
		case '=', ':', '@', '/':
			cuts = append(cuts, i)
		}
	}
	cuts = append(cuts, end)
	var out [][2]int
	for i := 0; i < len(cuts); i++ {
		for j := i + 1; j < len(cuts); j++ {
			a, b := cuts[i]+1, cuts[j]
			if a >= b {
				continue
			}
			out = append(out, [2]int{a, b})
			if s[b-1] == '.' && b-1 > a {
				out = append(out, [2]int{a, b - 1})
			}
		}
	}
	return out
}

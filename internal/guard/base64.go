package guard

import (
	"encoding/base64"
	"slices"
	"strings"
	"unicode/utf8"
)

// A secret is often carried base64-encoded: a Kubernetes Secret's data, a
// credential store's file attachment, `base64` output wrapped at 76
// columns. The scanner decodes such a run and looks into the decoded text
// with the token patterns and the index; a hit redacts the encoded run.

// minBase64Run is the shortest run decoded: 24 bytes, below the shortest
// token pattern.
const minBase64Run = 32

// maxBase64Depth bounds the encodings looked through: a file encoded twice,
// as a Secret's data holding a kubeconfig's base64 fields.
const maxBase64Depth = 2

// base64Runs are the spans of s holding a run of at least minBase64Run
// base64 characters, standard or URL alphabet, with the whole lines of
// base64 that may continue it, indented or not: a value wrapped at a fixed
// width. A byte scan: a regular expression costs a large image dearly.
func base64Runs(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); {
		if !b64Char(s[i], true) {
			i++
			continue
		}
		start := i
		i = b64Span(s, i, true)
		if i-start < minBase64Run {
			continue
		}
		end := i
		for {
			j := end
			if j < len(s) && s[j] == '\r' {
				j++
			}
			if j >= len(s) || s[j] != '\n' {
				break
			}
			j++
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			k := b64Span(s, j, false)
			if k-j < 4 || (k < len(s) && s[k] != '\n' && s[k] != '\r') {
				break
			}
			end = k
		}
		out = append(out, [2]int{start, end})
		i = end
	}
	return out
}

// b64Span is the end of the base64 characters from i on, up to two
// padding characters included.
func b64Span(s string, i int, url bool) int {
	for i < len(s) && b64Char(s[i], url) {
		i++
	}
	for n := 0; n < 2 && i < len(s) && s[i] == '='; n++ {
		i++
	}
	return i
}

func b64Char(c byte, url bool) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/' ||
		url && (c == '-' || c == '_')
}

// encodedSpans are the spans of s whose base64 decoding carries a token
// pattern or an indexed value, each named by what it carries.
func (ix *Index) encodedSpans(s string, depth int) []span {
	if depth > maxBase64Depth {
		return nil
	}
	var out []span
	for _, m := range base64Runs(s) {
		if allowed(s, m[0]) {
			continue
		}
		for _, run := range runs(s, m[0], m[1]) {
			d, ok := decodeBase64(s[run[0]:run[1]])
			if !ok {
				continue
			}
			if name, ref, found := ix.carries(d, depth); found {
				out = append(out, span{run[0], run[1], name, ref})
				break
			}
		}
	}
	return out
}

// runs are what the match s[start:end] is tried as: all its lines as one
// wrapped value, then, of several lines, each long enough on its own.
func runs(s string, start, end int) [][2]int {
	end = start + len(strings.TrimRight(s[start:end], "\r"))
	out := [][2]int{{start, end}}
	if !strings.Contains(s[start:end], "\n") {
		return out
	}
	at := start
	for l := range strings.Lines(s[start:end]) {
		trimmed := strings.TrimSpace(l)
		if len(trimmed) >= minBase64Run {
			from := at + strings.Index(l, trimmed)
			out = append(out, [2]int{from, from + len(trimmed)})
		}
		at += len(l)
	}
	return out
}

// decodeBase64 decodes run, its line breaks and indentation dropped, in the
// alphabet it is written in, padded or not; binary (an image) is not looked
// into, as no secret shape shows in it.
func decodeBase64(run string) (string, bool) {
	run = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, run)
	run = strings.TrimRight(run, "=")
	enc := base64.RawStdEncoding
	if strings.ContainsAny(run, "-_") {
		enc = base64.RawURLEncoding
	}
	d, err := enc.DecodeString(run)
	return string(d), err == nil && utf8.Valid(d)
}

// carries names what the decoded text d holds: an indexed reference, a
// token pattern's rule, or either within a further encoding.
func (ix *Index) carries(d string, depth int) (name string, ref, found bool) {
	if m := ix.matches(d); len(m) > 0 {
		return m[0].name, true, true
	}
	for _, r := range tokenRules {
		if slices.ContainsFunc(r.keywords, func(k string) bool { return strings.Contains(d, k) }) && r.re.MatchString(d) {
			return r.id, false, true
		}
	}
	if inner := ix.encodedSpans(d, depth+1); len(inner) > 0 {
		return inner[0].name, inner[0].ref, true
	}
	return "", false, false
}

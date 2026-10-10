package secret

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// What a Secret's key holds, as a refusal names it.
const (
	shapeYAMLMapping = "a YAML mapping"
	shapeJSONMapping = "a JSON mapping"
	shapeYAMLList    = "a YAML list"
	shapeJSONList    = "a JSON list"
	shapeMapping     = "a mapping"
	shapeList        = "a list"
	shapeStream      = "a YAML stream of several documents"
	shapeText        = "text, no YAML document"
	shapeDotenv      = "dotenv lines"
	shapeLines       = "key-value lines"
	shapeScalar      = "a scalar"
	shapeBinary      = "binary"
	shapeNothing     = "nothing"
)

// shownKeys is how many keys a shape names; the rest are counted.
const shownKeys = 20

// dotenvKey is the key of a dotenv or key-value line.
var dotenvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// counted is a key or line head a shape counts instead of naming.
var counted = regexp.MustCompile(`^<[0-9]+ characters>$`)

// lineHead is a text line's head a refusal names: a key's characters and
// spaces, nothing a value's encoding carries (/, +, quotes).
var lineHead = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_. -]*$`)

// shape is what a Secret's key, or a node inside it, holds, as far as a
// refusal may say: the format, the top-level or dotenv keys, a list's
// length, and for a whole key its line count and byte length. Never a
// value: a key named like a generated value is counted, not named.
type shape struct {
	format string
	keys   []string
	items  int
	lines  int
	bytes  int
	whole  bool // a whole key's content: the sizes are said
}

func (s shape) String() string {
	var b strings.Builder
	b.WriteString(s.format)
	switch {
	case len(s.keys) > 0 && s.format == shapeText:
		b.WriteString(" with the line heads " + namedKeys(s.keys))
	case len(s.keys) > 0 && s.whole && s.format != shapeDotenv && s.format != shapeLines:
		b.WriteString(" with the top-level keys " + namedKeys(s.keys))
	case len(s.keys) > 0:
		b.WriteString(" with the keys " + namedKeys(s.keys))
	case s.items > 0:
		fmt.Fprintf(&b, " of %s", plural(s.items, "item"))
	case s.format == shapeMapping, s.format == shapeYAMLMapping, s.format == shapeJSONMapping:
		b.WriteString(" with no keys")
	}
	switch {
	case !s.whole:
	case s.format == shapeBinary, s.format == shapeNothing:
		fmt.Fprintf(&b, " (%d bytes)", s.bytes)
	default:
		fmt.Fprintf(&b, " (%s, %d bytes)", plural(s.lines, "line"), s.bytes)
	}
	return b.String()
}

// namedKeys lists keys, at most shownKeys of them; a key named like a
// generated value, or longer than any key, is counted instead, and a
// count (<n characters>) stays one.
func namedKeys(keys []string) string {
	named := make([]string, 0, len(keys))
	for _, k := range keys {
		if !counted.MatchString(k) && (len(k) > 64 || keyLike(k)) {
			k = fmt.Sprintf("<%d characters>", len(k))
		}
		named = append(named, k)
	}
	if len(named) > shownKeys {
		return strings.Join(named[:shownKeys], ", ") + fmt.Sprintf(" and %d more", len(named)-shownKeys)
	}
	return strings.Join(named, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// content is what a key holds, parsed as far as a path reaches into it:
// a mapping (doc), dotenv lines (env), or nothing a path reaches into.
type content struct {
	shape shape
	doc   *document
	env   map[string]string
}

// parseContent reads what a key holds: dotenv lines first, which a YAML
// reader takes for a scalar or a mapping keyed by whole lines, then one
// YAML or JSON document, as written or with its indenting tabs as spaces,
// then key-value lines; text none of them reads is named by its line
// heads.
func parseContent(raw []byte) content {
	s := shape{bytes: len(raw), lines: countLines(raw), whole: true}
	switch {
	case len(raw) == 0:
		s.format = shapeNothing
		return content{shape: s}
	case binary(raw):
		s.format = shapeBinary
		return content{shape: s}
	}
	if env, keys := parseDotenv(raw); env != nil {
		s.format, s.keys = shapeDotenv, keys
		return content{shape: s, env: env}
	}
	json := bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("{")) || bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("["))
	n, err := parseNode(raw)
	if err != nil && !errors.Is(err, errSeveralDocuments) {
		if tn, terr := parseNode(untab(raw)); terr == nil && tn.Kind == yaml.MappingNode {
			n, err = tn, nil
		}
	}
	if err != nil && !errors.Is(err, errSeveralDocuments) {
		if env, keys := parseLines(raw); env != nil {
			s.format, s.keys = shapeLines, keys
			return content{shape: s, env: env}
		}
	}
	switch {
	case errors.Is(err, errSeveralDocuments):
		s.format = shapeStream
	case err != nil:
		s.format, s.keys = shapeText, lineHeads(raw)
	case n.Kind == yaml.MappingNode:
		doc := &document{root: n}
		s.format, s.keys = shapeYAMLMapping, doc.topKeys()
		if json {
			s.format = shapeJSONMapping
		}
		return content{shape: s, doc: doc}
	case n.Kind == yaml.SequenceNode:
		s.format, s.items = shapeYAMLList, len(n.Content)
		if json {
			s.format = shapeJSONList
		}
	default:
		s.format = shapeScalar
	}
	return content{shape: s}
}

// nodeShape is what a node inside a mapping holds; a scalar is read like
// a whole key, since a path may reach into the document it holds.
func nodeShape(n *yaml.Node) shape {
	switch n.Kind {
	case yaml.MappingNode:
		return shape{format: shapeMapping, keys: (&document{root: n}).topKeys()}
	case yaml.SequenceNode:
		return shape{format: shapeList, items: len(n.Content)}
	case yaml.AliasNode:
		return nodeShape(n.Alias)
	}
	return parseContent([]byte(n.Value)).shape
}

// countLines is the number of lines raw holds, a last line without its
// newline included.
func countLines(raw []byte) int {
	n := bytes.Count(raw, []byte("\n"))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		n++
	}
	return n
}

// binary reports whether raw is no text: invalid UTF-8 or a control
// character besides tab, newline and carriage return.
func binary(raw []byte) bool {
	if !utf8.Valid(raw) {
		return true
	}
	return bytes.ContainsFunc(raw, func(r rune) bool {
		return (r < ' ' && r != '\t' && r != '\n' && r != '\r') || r == unicode.MaxASCII
	})
}

// parseDotenv reads KEY=value lines, an optional export in front, matching
// quotes stripped, a trailing comment after an unquoted value dropped, #
// comments and blank lines ignored; the keys come in their order, a
// repeated key's last value wins. Any other line, a value beginning with
// =, or one line with an empty value (a padded encoding, no dotenv) means
// raw is no dotenv: nil.
func parseDotenv(raw []byte) (map[string]string, []string) {
	env := map[string]string{}
	var keys []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && unicode.IsSpace(rune(rest[0])) {
			line = strings.TrimSpace(rest)
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !dotenvKey.MatchString(k) || strings.HasPrefix(v, "=") {
			return nil, nil
		}
		if _, seen := env[k]; !seen {
			keys = append(keys, k)
		}
		env[k] = dotenvValue(v)
	}
	if len(keys) == 0 || (len(keys) == 1 && env[keys[0]] == "" && strings.Count(strings.TrimSpace(string(raw)), "\n") == 0) {
		return nil, nil
	}
	return env, keys
}

// untab is raw with CRLF line ends as LF and every tab of a line's
// indent as two spaces, the YAML a hand-written key often means.
func untab(raw []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i, l := range lines {
		rest := strings.TrimLeft(l, " \t")
		indent := l[:len(l)-len(rest)]
		lines[i] = strings.ReplaceAll(indent, "\t", "  ") + rest
	}
	return []byte(strings.Join(lines, "\n"))
}

// parseLines reads key-value lines: a key, then : or = with any spacing
// around it, then the value as dotenv reads one (quotes stripped, a
// trailing comment dropped), whichever separator comes first; an export in
// front, # comments, blank lines and CR line ends are ignored, a key in
// matching quotes unquoted. Any other line means raw is no key-value
// lines: nil.
func parseLines(raw []byte) (map[string]string, []string) {
	env := map[string]string{}
	var keys []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := splitLine(line)
		k = unquote(k)
		if !ok || !dotenvKey.MatchString(k) {
			return nil, nil
		}
		if _, seen := env[k]; !seen {
			keys = append(keys, k)
		}
		env[k] = dotenvValue(v)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return env, keys
}

// splitLine cuts a trimmed line at its first : or =, an export in front
// stripped from the head, which comes trimmed.
func splitLine(line string) (head, rest string, ok bool) {
	i := strings.IndexAny(line, ":=")
	if i < 0 {
		return "", "", false
	}
	head = strings.TrimSpace(line[:i])
	if h, found := strings.CutPrefix(head, "export"); found && h != "" && unicode.IsSpace(rune(h[0])) {
		head = strings.TrimSpace(h)
	}
	return head, line[i+1:], true
}

// unquote is s without matching quotes around it.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// lineHeads are the heads of text's lines, as a refusal names them: the
// text before a line's first : or = (trimmed, an export stripped), never
// anything after it. A line without either, with a head no key would
// carry, or with nothing but = after its head (a padded encoding) is
// counted by its length; text without a single head names none.
func lineHeads(raw []byte) []string {
	var heads []string
	named := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		h, rest, ok := splitLine(line)
		h = unquote(h)
		padded := ok && line[len(line)-len(rest)-1] == '=' && strings.Trim(rest, "= \t") == ""
		if !ok || padded || !lineHead.MatchString(h) {
			heads = append(heads, fmt.Sprintf("<%d characters>", len(line)))
			continue
		}
		heads, named = append(heads, h), true
	}
	if !named {
		return nil
	}
	return heads
}

// dotenvValue is a dotenv line's value: the inside of matching quotes (a
// double-quoted one with \" and \\ unescaped), else the value up to a
// trailing comment, trimmed.
func dotenvValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '\'' {
		if i := strings.IndexByte(v[1:], '\''); i >= 0 {
			return v[1 : i+1]
		}
	}
	if len(v) >= 2 && v[0] == '"' {
		for i := 1; i < len(v); i++ {
			switch v[i] {
			case '\\':
				i++
			case '"':
				return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(v[1:i])
			}
		}
	}
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		if j := strings.Index(v[i:], "#"); j >= 0 {
			v = v[:i+j]
		}
	}
	return strings.TrimSpace(v)
}

// valueAt is the value a dotted path names inside raw, a Secret key's
// content: a key of the YAML or JSON mapping it holds, a dotenv line's
// value, or, one level down, a value of the document a block scalar on
// the way holds. A path that reaches nothing fails naming the path and
// the shape of what the key, or the node on the way, holds: the format,
// the keys, the lines and the bytes, never a value.
func valueAt(raw []byte, path string) (string, error) {
	return lookup(raw, "", strings.Split(path, "."))
}

// lookup resolves keys in raw, the content under the dotted prefix ("" for
// the Secret's key itself). Only the key's own mapping descends into a
// block scalar, so a document nests one level.
func lookup(raw []byte, prefix string, keys []string) (string, error) {
	c := parseContent(raw)
	path := strings.Join(keys, ".")
	if c.env != nil {
		if v, ok := c.env[path]; ok {
			return v, nil
		}
	}
	if c.doc == nil {
		return "", noValue(prefix, path, "", c.shape)
	}
	n := c.doc.root
	for i := 0; i < len(keys); {
		at := strings.Join(keys[:i], ".")
		if n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		if n.Kind == yaml.ScalarNode {
			// a block scalar on the way: the document it holds, one level down
			if prefix != "" {
				return "", noValue(prefix, path, at, nodeShape(n))
			}
			return lookup([]byte(n.Value), at, keys[i:])
		}
		next, width := childAt(n, keys[i:])
		if next == nil {
			if i == 0 {
				return "", noValue(prefix, path, "", c.shape)
			}
			return "", noValue(prefix, path, at, nodeShape(n))
		}
		n, i = next, i+width
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind != yaml.ScalarNode {
		return "", noValue(prefix, path, path, nodeShape(n))
	}
	return n.Value, nil
}

// childAt is the child the first of keys names, or, for a key holding
// dots (config.yaml), the one the fewest leading keys joined name; with
// how many keys it took, 0 for none.
func childAt(n *yaml.Node, keys []string) (*yaml.Node, int) {
	for width := 1; width <= len(keys); width++ {
		if c := child(n, strings.Join(keys[:width], ".")); c != nil {
			return c, width
		}
	}
	return nil, 0
}

// noValue is the refusal of a path that reaches nothing: the full path,
// then what the key, or the node at the dotted at inside it, holds.
func noValue(prefix, path, at string, s shape) error {
	where := "the key"
	if p := joinPath(prefix, at); p != "" {
		where = p
	}
	return fmt.Errorf("no value at %s: %s holds %s", joinPath(prefix, path), where, s)
}

// joinPath joins two dotted paths, either one empty.
func joinPath(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "." + b
}

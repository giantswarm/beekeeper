//go:build scenario

package fakegithub

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A GraphQL document as gh and beekeeper write them: queries and mutations
// with variables (and their defaults), aliases, arguments, inline fragments,
// fragment spreads and definitions, and the @include and @skip directives.

// gqlDoc is a parsed document.
type gqlDoc struct {
	Ops       []*gqlOp
	Fragments map[string]*gqlFragment
}

// gqlOp is one operation.
type gqlOp struct {
	Kind string // "query" or "mutation"
	Name string
	Vars []gqlVarDef
	Sels []gqlSel
}

type gqlVarDef struct {
	Name    string
	Type    string // as written: "Int!", "[PullRequestState!]"
	Default *gqlValue
}

// gqlSel is one selection: exactly one of Field, Spread and Inline.
type gqlSel struct {
	Field  *gqlField
	Spread string
	Inline *gqlInline
	Dirs   []gqlDirective
}

type gqlField struct {
	Alias string
	Name  string
	Args  []gqlArg
	Sels  []gqlSel
	Line  int
	Col   int
}

// Key is the field's key in its result: the alias, else the name.
func (f *gqlField) Key() string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}

type gqlInline struct {
	On   string // "" for a fragment without a type condition
	Sels []gqlSel
}

type gqlFragment struct {
	Name string
	On   string
	Sels []gqlSel
}

type gqlArg struct {
	Name  string
	Value gqlValue
}

type gqlDirective struct {
	Name string
	Args []gqlArg
}

// The kinds of a value.
const (
	valVar = iota
	valInt
	valFloat
	valString
	valBool
	valNull
	valEnum
	valList
	valObject
)

type gqlValue struct {
	Kind   int
	Str    string // variable name, string, enum
	Int    int
	Float  float64
	Bool   bool
	List   []gqlValue
	Fields []gqlArg
}

// The token kinds.
const (
	tokEOF = iota
	tokPunct
	tokName
	tokInt
	tokFloat
	tokString
)

type gqlToken struct {
	kind int
	text string
	line int
	col  int
}

type gqlLexer struct {
	src  string
	pos  int
	line int
	col  int
}

// gqlSyntaxError is a document GitHub would refuse to parse.
type gqlSyntaxError struct {
	Line, Col int
	Msg       string
}

func (e *gqlSyntaxError) Error() string {
	return fmt.Sprintf("Parse error on line %d column %d: %s", e.Line, e.Col, e.Msg)
}

func (l *gqlLexer) errorf(format string, args ...any) error {
	return &gqlSyntaxError{Line: l.line, Col: l.col, Msg: fmt.Sprintf(format, args...)}
}

func (l *gqlLexer) advance(n int) {
	for i := 0; i < n && l.pos < len(l.src); i++ {
		if l.src[l.pos] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (l *gqlLexer) next() (gqlToken, error) {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			l.advance(1)
		case c == '\xef' && strings.HasPrefix(l.src[l.pos:], "\xef\xbb\xbf"):
			l.advance(3)
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advance(1)
			}
		default:
			return l.token()
		}
	}
	return gqlToken{kind: tokEOF, line: l.line, col: l.col}, nil
}

func (l *gqlLexer) token() (gqlToken, error) {
	t := gqlToken{line: l.line, col: l.col}
	c := l.src[l.pos]
	switch {
	case strings.HasPrefix(l.src[l.pos:], "..."):
		t.kind, t.text = tokPunct, "..."
		l.advance(3)
	case strings.ContainsRune("!$()[]{}:=@|&", rune(c)):
		t.kind, t.text = tokPunct, string(c)
		l.advance(1)
	case isNameStart(c):
		start := l.pos
		for l.pos < len(l.src) && (isNameStart(l.src[l.pos]) || isDigit(l.src[l.pos])) {
			l.advance(1)
		}
		t.kind, t.text = tokName, l.src[start:l.pos]
	case c == '-' || isDigit(c):
		start := l.pos
		l.advance(1)
		float := false
		for l.pos < len(l.src) {
			d := l.src[l.pos]
			if isDigit(d) {
				l.advance(1)
				continue
			}
			if d == '.' || d == 'e' || d == 'E' || (d == '+' || d == '-') && (l.src[l.pos-1] == 'e' || l.src[l.pos-1] == 'E') {
				float = true
				l.advance(1)
				continue
			}
			break
		}
		t.kind, t.text = tokInt, l.src[start:l.pos]
		if float {
			t.kind = tokFloat
		}
	case c == '"':
		s, err := l.str()
		if err != nil {
			return t, err
		}
		t.kind, t.text = tokString, s
	default:
		r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
		return t, l.errorf("unexpected character %q", r)
	}
	return t, nil
}

func (l *gqlLexer) str() (string, error) {
	if strings.HasPrefix(l.src[l.pos:], `"""`) {
		l.advance(3)
		end := strings.Index(l.src[l.pos:], `"""`)
		if end < 0 {
			return "", l.errorf("unterminated block string")
		}
		raw := l.src[l.pos : l.pos+end]
		l.advance(end + 3)
		return blockString(raw), nil
	}
	l.advance(1)
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.advance(1)
			return b.String(), nil
		case '\n':
			return "", l.errorf("unterminated string")
		case '\\':
			if l.pos+1 >= len(l.src) {
				return "", l.errorf("unterminated string")
			}
			e := l.src[l.pos+1]
			l.advance(2)
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if l.pos+4 > len(l.src) {
					return "", l.errorf("bad unicode escape")
				}
				n, err := strconv.ParseUint(l.src[l.pos:l.pos+4], 16, 32)
				if err != nil {
					return "", l.errorf("bad unicode escape")
				}
				b.WriteRune(rune(n))
				l.advance(4)
			default:
				return "", l.errorf("bad escape \\%c", e)
			}
		default:
			b.WriteByte(c)
			l.advance(1)
		}
	}
	return "", l.errorf("unterminated string")
}

// blockString removes a block string's common indentation and its blank
// first and last lines.
func blockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	indent := -1
	for i, line := range lines {
		if i == 0 {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		if n := len(line) - len(trimmed); indent < 0 || n < indent {
			indent = n
		}
	}
	for i := range lines {
		if i > 0 && indent > 0 && len(lines[i]) >= indent {
			lines[i] = lines[i][indent:]
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

type gqlParser struct {
	lex  gqlLexer
	tok  gqlToken
	deep int
}

// maxSelectionDepth bounds a document's nesting.
const maxSelectionDepth = 40

// parseGraphQL parses a document.
func parseGraphQL(src string) (*gqlDoc, error) {
	p := &gqlParser{lex: gqlLexer{src: src, line: 1, col: 1}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	doc := &gqlDoc{Fragments: map[string]*gqlFragment{}}
	for p.tok.kind != tokEOF {
		switch {
		case p.isPunct("{"):
			sels, err := p.selections()
			if err != nil {
				return nil, err
			}
			doc.Ops = append(doc.Ops, &gqlOp{Kind: "query", Sels: sels})
		case p.isName("query"), p.isName("mutation"):
			op, err := p.operation()
			if err != nil {
				return nil, err
			}
			doc.Ops = append(doc.Ops, op)
		case p.isName("fragment"):
			f, err := p.fragment()
			if err != nil {
				return nil, err
			}
			if _, dup := doc.Fragments[f.Name]; dup {
				return nil, p.errorf("a second fragment %q", f.Name)
			}
			doc.Fragments[f.Name] = f
		case p.isName("subscription"):
			return nil, p.errorf("subscriptions are not served")
		default:
			return nil, p.unexpected()
		}
	}
	if len(doc.Ops) == 0 {
		return nil, &gqlSyntaxError{Line: 1, Col: 1, Msg: "A query attribute must be specified and must be a string."}
	}
	return doc, nil
}

func (p *gqlParser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *gqlParser) errorf(format string, args ...any) error {
	return &gqlSyntaxError{Line: p.tok.line, Col: p.tok.col, Msg: fmt.Sprintf(format, args...)}
}

func (p *gqlParser) unexpected() error {
	if p.tok.kind == tokEOF {
		return p.errorf("Unexpected end of document")
	}
	return p.errorf("unexpected %q", p.tok.text)
}

func (p *gqlParser) isPunct(s string) bool { return p.tok.kind == tokPunct && p.tok.text == s }
func (p *gqlParser) isName(s string) bool  { return p.tok.kind == tokName && p.tok.text == s }

func (p *gqlParser) punct(s string) error {
	if !p.isPunct(s) {
		if p.tok.kind == tokEOF {
			return p.errorf("Unexpected end of document, expected %q", s)
		}
		return p.errorf("unexpected %q, expected %q", p.tok.text, s)
	}
	return p.advance()
}

func (p *gqlParser) name() (string, error) {
	if p.tok.kind != tokName {
		return "", p.unexpected()
	}
	s := p.tok.text
	return s, p.advance()
}

func (p *gqlParser) operation() (*gqlOp, error) {
	op := &gqlOp{Kind: p.tok.text}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokName {
		op.Name = p.tok.text
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if p.isPunct("(") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		for !p.isPunct(")") {
			v, err := p.varDef()
			if err != nil {
				return nil, err
			}
			op.Vars = append(op.Vars, v)
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if _, err := p.directives(); err != nil {
		return nil, err
	}
	sels, err := p.selections()
	if err != nil {
		return nil, err
	}
	op.Sels = sels
	return op, nil
}

func (p *gqlParser) varDef() (gqlVarDef, error) {
	var v gqlVarDef
	if err := p.punct("$"); err != nil {
		return v, err
	}
	name, err := p.name()
	if err != nil {
		return v, err
	}
	v.Name = name
	if err := p.punct(":"); err != nil {
		return v, err
	}
	if v.Type, err = p.typeRef(); err != nil {
		return v, err
	}
	if p.isPunct("=") {
		if err := p.advance(); err != nil {
			return v, err
		}
		val, err := p.value(true)
		if err != nil {
			return v, err
		}
		v.Default = &val
	}
	_, err = p.directives()
	return v, err
}

func (p *gqlParser) typeRef() (string, error) {
	var b strings.Builder
	if p.isPunct("[") {
		if err := p.advance(); err != nil {
			return "", err
		}
		inner, err := p.typeRef()
		if err != nil {
			return "", err
		}
		if err := p.punct("]"); err != nil {
			return "", err
		}
		b.WriteString("[" + inner + "]")
	} else {
		name, err := p.name()
		if err != nil {
			return "", err
		}
		b.WriteString(name)
	}
	if p.isPunct("!") {
		b.WriteString("!")
		if err := p.advance(); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func (p *gqlParser) fragment() (*gqlFragment, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	name, err := p.name()
	if err != nil {
		return nil, err
	}
	if !p.isName("on") {
		return nil, p.unexpected()
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	on, err := p.name()
	if err != nil {
		return nil, err
	}
	if _, err := p.directives(); err != nil {
		return nil, err
	}
	sels, err := p.selections()
	if err != nil {
		return nil, err
	}
	return &gqlFragment{Name: name, On: on, Sels: sels}, nil
}

func (p *gqlParser) selections() ([]gqlSel, error) {
	if err := p.punct("{"); err != nil {
		return nil, err
	}
	p.deep++
	defer func() { p.deep-- }()
	if p.deep > maxSelectionDepth {
		return nil, p.errorf("selections nest deeper than %d", maxSelectionDepth)
	}
	var out []gqlSel
	for !p.isPunct("}") {
		if p.tok.kind == tokEOF {
			return nil, p.unexpected()
		}
		s, err := p.selection()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, p.errorf("an empty selection")
	}
	return out, p.advance()
}

func (p *gqlParser) selection() (gqlSel, error) {
	var s gqlSel
	if p.isPunct("...") {
		if err := p.advance(); err != nil {
			return s, err
		}
		if p.tok.kind == tokName && p.tok.text != "on" {
			s.Spread = p.tok.text
			if err := p.advance(); err != nil {
				return s, err
			}
			dirs, err := p.directives()
			s.Dirs = dirs
			return s, err
		}
		in := &gqlInline{}
		if p.isName("on") {
			if err := p.advance(); err != nil {
				return s, err
			}
			on, err := p.name()
			if err != nil {
				return s, err
			}
			in.On = on
		}
		dirs, err := p.directives()
		if err != nil {
			return s, err
		}
		if in.Sels, err = p.selections(); err != nil {
			return s, err
		}
		s.Inline, s.Dirs = in, dirs
		return s, nil
	}
	f := &gqlField{Line: p.tok.line, Col: p.tok.col}
	name, err := p.name()
	if err != nil {
		return s, err
	}
	f.Name = name
	if p.isPunct(":") {
		if err := p.advance(); err != nil {
			return s, err
		}
		f.Alias = name
		if f.Name, err = p.name(); err != nil {
			return s, err
		}
	}
	if p.isPunct("(") {
		if f.Args, err = p.arguments(false); err != nil {
			return s, err
		}
	}
	dirs, err := p.directives()
	if err != nil {
		return s, err
	}
	if p.isPunct("{") {
		if f.Sels, err = p.selections(); err != nil {
			return s, err
		}
	}
	s.Field, s.Dirs = f, dirs
	return s, nil
}

func (p *gqlParser) arguments(constant bool) ([]gqlArg, error) {
	if err := p.punct("("); err != nil {
		return nil, err
	}
	var out []gqlArg
	for !p.isPunct(")") {
		name, err := p.name()
		if err != nil {
			return nil, err
		}
		if err := p.punct(":"); err != nil {
			return nil, err
		}
		v, err := p.value(constant)
		if err != nil {
			return nil, err
		}
		out = append(out, gqlArg{Name: name, Value: v})
	}
	return out, p.advance()
}

func (p *gqlParser) directives() ([]gqlDirective, error) {
	var out []gqlDirective
	for p.isPunct("@") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		name, err := p.name()
		if err != nil {
			return nil, err
		}
		d := gqlDirective{Name: name}
		if p.isPunct("(") {
			if d.Args, err = p.arguments(false); err != nil {
				return nil, err
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (p *gqlParser) value(constant bool) (gqlValue, error) {
	t := p.tok
	switch {
	case p.isPunct("$"):
		if constant {
			return gqlValue{}, p.errorf("a variable where a constant is needed")
		}
		if err := p.advance(); err != nil {
			return gqlValue{}, err
		}
		name, err := p.name()
		return gqlValue{Kind: valVar, Str: name}, err
	case t.kind == tokInt:
		n, err := strconv.Atoi(t.text)
		if err != nil {
			return gqlValue{}, p.errorf("bad integer %q", t.text)
		}
		return gqlValue{Kind: valInt, Int: n}, p.advance()
	case t.kind == tokFloat:
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return gqlValue{}, p.errorf("bad number %q", t.text)
		}
		return gqlValue{Kind: valFloat, Float: f}, p.advance()
	case t.kind == tokString:
		return gqlValue{Kind: valString, Str: t.text}, p.advance()
	case t.kind == tokName:
		switch t.text {
		case "true", "false":
			return gqlValue{Kind: valBool, Bool: t.text == "true"}, p.advance()
		case "null":
			return gqlValue{Kind: valNull}, p.advance()
		}
		return gqlValue{Kind: valEnum, Str: t.text}, p.advance()
	case p.isPunct("["):
		if err := p.advance(); err != nil {
			return gqlValue{}, err
		}
		v := gqlValue{Kind: valList, List: []gqlValue{}}
		for !p.isPunct("]") {
			if p.tok.kind == tokEOF {
				return v, p.unexpected()
			}
			e, err := p.value(constant)
			if err != nil {
				return v, err
			}
			v.List = append(v.List, e)
		}
		return v, p.advance()
	case p.isPunct("{"):
		if err := p.advance(); err != nil {
			return gqlValue{}, err
		}
		v := gqlValue{Kind: valObject}
		for !p.isPunct("}") {
			name, err := p.name()
			if err != nil {
				return v, err
			}
			if err := p.punct(":"); err != nil {
				return v, err
			}
			e, err := p.value(constant)
			if err != nil {
				return v, err
			}
			v.Fields = append(v.Fields, gqlArg{Name: name, Value: e})
		}
		return v, p.advance()
	}
	return gqlValue{}, p.unexpected()
}

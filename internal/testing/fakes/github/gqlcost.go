package fakegithub

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// A small GraphQL document parser: enough to find an operation's name and type, and to price it the
// way GitHub does (contract §3.1: "with point costs like GitHub's").
//
// GitHub's rule: add up the requests needed to fulfil each connection (a field with a first or last
// argument), assuming every connection reaches its limit, so a connection nested in one with
// first: n needs n requests; divide by 100 and round; the minimum is 1.

type gqlDoc struct {
	ops   []*gqlOp
	frags map[string][]*gqlSel
}

type gqlOp struct {
	typ      string // query, mutation, subscription
	name     string
	defaults map[string]any
	sel      []*gqlSel
}

type gqlSel struct {
	field     string // field name; "" for fragments
	alias     string
	args      map[string]any
	dirs      []gqlDir
	sel       []*gqlSel
	spread    string // named fragment spread
	inline    bool   // inline fragment
	condition string // type condition of an inline fragment
}

type gqlDir struct {
	name string
	args map[string]any
}

// gqlVar is a variable reference in a value.
type gqlVar string

type gqlEnum string

type gqlTok struct {
	kind byte // n name, p punctuator, s string, i number, e end
	val  string
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func lexGraphQL(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "..."):
			toks = append(toks, gqlTok{'p', "..."})
			i += 3
		case strings.ContainsRune("!$():=@[]{}|&", rune(c)):
			toks = append(toks, gqlTok{'p', string(c)})
			i++
		case c == '"':
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				toks = append(toks, gqlTok{'s', src[i+3 : i+3+end]})
				i += 6 + end
				continue
			}
			j := i + 1
			var b strings.Builder
			for ; j < len(src) && src[j] != '"'; j++ {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(src[j])
					}
					continue
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{'s', b.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(src) && strings.ContainsRune("0123456789.eE+-", rune(src[j])) {
				j++
			}
			toks = append(toks, gqlTok{'i', src[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i + 1
			for j < len(src) && (src[j] == '_' || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			toks = append(toks, gqlTok{'n', src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q at %d", c, i)
		}
	}
	return append(toks, gqlTok{'e', ""}), nil
}

func parseGraphQL(src string) (*gqlDoc, error) {
	toks, err := lexGraphQL(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDoc{frags: map[string][]*gqlSel{}}
	for p.peek().kind != 'e' {
		t := p.peek()
		switch {
		case t.kind == 'p' && t.val == "{":
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, &gqlOp{typ: opQuery, sel: sel, defaults: map[string]any{}})
		case t.kind == 'n' && (t.val == opQuery || t.val == "mutation" || t.val == "subscription"):
			op, err := p.operation()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, op)
		case t.kind == 'n' && t.val == "fragment":
			p.next()
			name := p.next()
			if on := p.next(); on.val != "on" {
				return nil, fmt.Errorf("fragment %s: expected 'on'", name.val)
			}
			p.next() // type
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.frags[name.val] = sel
		default:
			return nil, fmt.Errorf("unexpected %q", t.val)
		}
	}
	if len(doc.ops) == 0 {
		return nil, fmt.Errorf("no operation")
	}
	return doc, nil
}

func (p *gqlParser) peek() gqlTok { return p.toks[p.pos] }

func (p *gqlParser) next() gqlTok {
	t := p.toks[p.pos]
	if t.kind != 'e' {
		p.pos++
	}
	return t
}

func (p *gqlParser) expect(val string) error {
	if t := p.next(); t.kind != 'p' || t.val != val {
		return fmt.Errorf("expected %q, got %q", val, t.val)
	}
	return nil
}

func (p *gqlParser) operation() (*gqlOp, error) {
	op := &gqlOp{typ: p.next().val, defaults: map[string]any{}}
	if p.peek().kind == 'n' {
		op.name = p.next().val
	}
	if p.peek().kind == 'p' && p.peek().val == "(" {
		p.next()
		for p.peek().kind != 'p' || p.peek().val != ")" {
			if err := p.expect("$"); err != nil {
				return nil, err
			}
			name := p.next().val
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			if err := p.typeRef(); err != nil {
				return nil, err
			}
			if p.peek().kind == 'p' && p.peek().val == "=" {
				p.next()
				v, err := p.value()
				if err != nil {
					return nil, err
				}
				op.defaults[name] = v
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			if p.peek().kind == 'e' {
				return nil, fmt.Errorf("unterminated variable definitions")
			}
		}
		p.next()
	}
	if _, err := p.directives(); err != nil {
		return nil, err
	}
	sel, err := p.selectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

func (p *gqlParser) typeRef() error {
	if p.peek().kind == 'p' && p.peek().val == "[" {
		p.next()
		if err := p.typeRef(); err != nil {
			return err
		}
		if err := p.expect("]"); err != nil {
			return err
		}
	} else if t := p.next(); t.kind != 'n' {
		return fmt.Errorf("expected a type, got %q", t.val)
	}
	if p.peek().kind == 'p' && p.peek().val == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) selectionSet() ([]*gqlSel, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var sels []*gqlSel
	for p.peek().kind != 'p' || p.peek().val != "}" {
		if p.peek().kind == 'e' {
			return nil, fmt.Errorf("unterminated selection set")
		}
		s, err := p.selection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, s)
	}
	p.next()
	return sels, nil
}

func (p *gqlParser) selection() (*gqlSel, error) {
	if p.peek().kind == 'p' && p.peek().val == "..." {
		p.next()
		s := &gqlSel{}
		if p.peek().kind == 'n' && p.peek().val != "on" {
			s.spread = p.next().val
			dirs, err := p.directives()
			s.dirs = dirs
			return s, err
		}
		s.inline = true
		if p.peek().kind == 'n' && p.peek().val == "on" {
			p.next()
			s.condition = p.next().val
		}
		dirs, err := p.directives()
		if err != nil {
			return nil, err
		}
		s.dirs = dirs
		if s.sel, err = p.selectionSet(); err != nil {
			return nil, err
		}
		return s, nil
	}
	t := p.next()
	if t.kind != 'n' {
		return nil, fmt.Errorf("expected a field, got %q", t.val)
	}
	s := &gqlSel{field: t.val}
	if p.peek().kind == 'p' && p.peek().val == ":" {
		p.next()
		s.alias = s.field
		s.field = p.next().val
	}
	if p.peek().kind == 'p' && p.peek().val == "(" {
		args, err := p.arguments()
		if err != nil {
			return nil, err
		}
		s.args = args
	}
	dirs, err := p.directives()
	if err != nil {
		return nil, err
	}
	s.dirs = dirs
	if p.peek().kind == 'p' && p.peek().val == "{" {
		if s.sel, err = p.selectionSet(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (p *gqlParser) arguments() (map[string]any, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	args := map[string]any{}
	for p.peek().kind != 'p' || p.peek().val != ")" {
		if p.peek().kind == 'e' {
			return nil, fmt.Errorf("unterminated arguments")
		}
		name := p.next().val
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
	p.next()
	return args, nil
}

func (p *gqlParser) directives() ([]gqlDir, error) {
	var dirs []gqlDir
	for p.peek().kind == 'p' && p.peek().val == "@" {
		p.next()
		d := gqlDir{name: p.next().val}
		if p.peek().kind == 'p' && p.peek().val == "(" {
			args, err := p.arguments()
			if err != nil {
				return nil, err
			}
			d.args = args
		}
		dirs = append(dirs, d)
	}
	return dirs, nil
}

func (p *gqlParser) value() (any, error) {
	t := p.next()
	switch t.kind {
	case 'p':
		switch t.val {
		case "$":
			return gqlVar(p.next().val), nil
		case "[":
			var list []any
			for p.peek().kind != 'p' || p.peek().val != "]" {
				if p.peek().kind == 'e' {
					return nil, fmt.Errorf("unterminated list")
				}
				v, err := p.value()
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			p.next()
			return list, nil
		case "{":
			obj := map[string]any{}
			for p.peek().kind != 'p' || p.peek().val != "}" {
				if p.peek().kind == 'e' {
					return nil, fmt.Errorf("unterminated object")
				}
				name := p.next().val
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				v, err := p.value()
				if err != nil {
					return nil, err
				}
				obj[name] = v
			}
			p.next()
			return obj, nil
		}
	case 's':
		return t.val, nil
	case 'i':
		f, err := strconv.ParseFloat(t.val, 64)
		return f, err
	case 'n':
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return gqlEnum(t.val), nil
	}
	return nil, fmt.Errorf("unexpected %q in a value", t.val)
}

// operation picks the operation a request names (or its only one).
func (d *gqlDoc) operation(name string) (*gqlOp, error) {
	if name == "" {
		if len(d.ops) != 1 {
			return nil, fmt.Errorf("operationName is required for a document with %d operations", len(d.ops))
		}
		return d.ops[0], nil
	}
	for _, op := range d.ops {
		if op.name == name {
			return op, nil
		}
	}
	return nil, fmt.Errorf("no operation named %q", name)
}

// cost prices an operation with the request's variables.
func (d *gqlDoc) cost(op *gqlOp, vars map[string]any) int {
	resolve := func(v any) any {
		if name, ok := v.(gqlVar); ok {
			if x, ok := vars[string(name)]; ok {
				return x
			}
			return op.defaults[string(name)]
		}
		return v
	}
	var requests float64
	var walk func(sels []*gqlSel, mult float64, depth int)
	walk = func(sels []*gqlSel, mult float64, depth int) {
		if depth > 50 {
			return
		}
		for _, s := range sels {
			if !included(s.dirs, resolve) {
				continue
			}
			switch {
			case s.spread != "":
				walk(d.frags[s.spread], mult, depth+1)
			case s.inline:
				walk(s.sel, mult, depth+1)
			default:
				child := mult
				for _, a := range []string{"first", "last"} {
					if v, ok := s.args[a]; ok {
						requests += mult
						child = mult * number(resolve(v))
						break
					}
				}
				walk(s.sel, child, depth+1)
			}
		}
	}
	walk(op.sel, 1, 0)
	points := int(math.Round(requests / 100))
	return max(points, 1)
}

func included(dirs []gqlDir, resolve func(any) any) bool {
	for _, d := range dirs {
		v, _ := resolve(d.args["if"]).(bool)
		switch d.name {
		case "include":
			if !v {
				return false
			}
		case "skip":
			if v {
				return false
			}
		}
	}
	return true
}

func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

//go:build scenario

package fakegithub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// gqlTyped is an object of the GraphQL schema: it names its type.
type gqlTyped interface{ gqlType() string }

// gqlError is one field's error: the field is null and the response
// carries the error beside its data, as GitHub answers NOT_FOUND.
type gqlError struct {
	Type    string
	Message string
}

func (e *gqlError) Error() string { return e.Message }

func notFound(format string, args ...any) *gqlError {
	return &gqlError{Type: "NOT_FOUND", Message: fmt.Sprintf(format, args...)}
}

// unsupportedError is a request for something the model does not have: a
// field, an argument, a route. It fails the scenario instead of being
// answered with a guess.
type unsupportedError struct{ What string }

func (e *unsupportedError) Error() string { return e.What }

func unsupported(format string, args ...any) *unsupportedError {
	return &unsupportedError{What: fmt.Sprintf(format, args...)}
}

// args are a field's evaluated arguments: strings (enums too), ints,
// floats, bools, nil, lists and maps.
type args map[string]any

func (a args) has(name string) bool {
	v, ok := a[name]
	return ok && v != nil
}

func (a args) str(name string) string {
	switch v := a[name].(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	}
	return ""
}

func (a args) num(name string) (int, bool) {
	switch v := a[name].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	return 0, false
}

func (a args) flag(name string) bool {
	v, _ := a[name].(bool)
	return v
}

// strs is a list argument's strings; a single value is a list of one.
func (a args) strs(name string) []string {
	switch v := a[name].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (a args) obj(name string) args {
	if v, ok := a[name].(map[string]any); ok {
		return args(v)
	}
	return args{}
}

// omap is a JSON object that keeps its keys in the order they were set,
// as a GraphQL response keeps the query's.
type omap struct {
	keys []string
	vals map[string]any
}

func newOmap() *omap { return &omap{vals: map[string]any{}} }

// set sets a key; two objects under one key merge, as two selections of
// one field do.
func (o *omap) set(k string, v any) {
	old, seen := o.vals[k]
	if !seen {
		o.keys = append(o.keys, k)
		o.vals[k] = v
		return
	}
	if a, ok := old.(*omap); ok {
		if b, ok := v.(*omap); ok {
			for _, bk := range b.keys {
				a.set(bk, b.vals[bk])
			}
			return
		}
	}
	o.vals[k] = v
}

// MarshalJSON writes the object in key order.
func (o *omap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshalNoEscape(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalNoEscape is json.Marshal without HTML escaping: GitHub writes <
// and & as they are.
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// gqlResponseError is an entry of a response's errors.
type gqlResponseError struct {
	Type       string         `json:"type,omitempty"`
	Path       []any          `json:"path,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
	Locations  []gqlLocation  `json:"locations,omitempty"`
	Message    string         `json:"message"`
}

type gqlLocation struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// gqlResult is what one request executed to.
type gqlResult struct {
	Data   *omap
	Errors []gqlResponseError
	// Unsupported names what the model lacks, when the request asked for
	// it.
	Unsupported string
	// Mutated says the operation was a mutation.
	Mutated bool
}

// body is the response body.
func (r *gqlResult) body() []byte {
	doc := newOmap()
	if r.Data != nil {
		doc.set("data", r.Data)
	}
	if len(r.Errors) > 0 {
		doc.set("errors", r.Errors)
	}
	b, _ := marshalNoEscape(doc)
	return b
}

// gqlExec executes one operation against the model.
type gqlExec struct {
	srv  *Server
	m    *Model
	doc  *gqlDoc
	vars map[string]any
	errs []gqlResponseError
	// rate is what the rateLimit field reads.
	rate rateView
	// spreads guards against a fragment that spreads itself.
	spreads []string
}

// rateView is the GraphQL budget as the rateLimit field shows it.
type rateView struct {
	Limit     int
	Remaining int
	Used      int
	Cost      int
	ResetAt   time.Time
}

// gqlOperation is one request's operation, parsed and named.
type gqlOperation struct {
	Doc  *gqlDoc
	Op   *gqlOp
	Vars map[string]any
	// Roots are the operation's top-level field names.
	Roots []string
}

// Name is the operation's name, or its first root field for an anonymous
// one.
func (o *gqlOperation) Name() string {
	if o.Op.Name != "" {
		return o.Op.Name
	}
	if len(o.Roots) > 0 {
		return o.Roots[0]
	}
	return ""
}

// prepareGraphQL parses a request's query and picks its operation.
func prepareGraphQL(query, operationName string, vars map[string]any) (*gqlOperation, error) {
	doc, err := parseGraphQL(query)
	if err != nil {
		return nil, err
	}
	op := doc.Ops[0]
	if operationName != "" {
		op = nil
		for _, o := range doc.Ops {
			if o.Name == operationName {
				op = o
			}
		}
		if op == nil {
			return nil, &gqlSyntaxError{Line: 1, Col: 1, Msg: fmt.Sprintf("No operation named %q", operationName)}
		}
	} else if len(doc.Ops) > 1 {
		return nil, &gqlSyntaxError{Line: 1, Col: 1, Msg: "An operation name is required"}
	}
	out := &gqlOperation{Doc: doc, Op: op, Vars: map[string]any{}}
	for k, v := range vars {
		out.Vars[k] = normalizeJSON(v)
	}
	for _, def := range op.Vars {
		if _, given := out.Vars[def.Name]; given {
			continue
		}
		switch {
		case def.Default != nil:
			v, err := evalValue(*def.Default, nil)
			if err != nil {
				return nil, err
			}
			out.Vars[def.Name] = v
		case strings.HasSuffix(def.Type, "!"):
			return nil, &gqlSyntaxError{Line: 1, Col: 1, Msg: fmt.Sprintf("Variable $%s of type %s was provided invalid value", def.Name, def.Type)}
		}
	}
	out.Roots = rootFields(doc, op.Sels, nil)
	return out, nil
}

func rootFields(doc *gqlDoc, sels []gqlSel, seen []string) []string {
	var out []string
	for _, s := range sels {
		switch {
		case s.Field != nil:
			if !slices.Contains(out, s.Field.Name) {
				out = append(out, s.Field.Name)
			}
		case s.Inline != nil:
			out = append(out, rootFields(doc, s.Inline.Sels, seen)...)
		case s.Spread != "" && !slices.Contains(seen, s.Spread):
			if f, ok := doc.Fragments[s.Spread]; ok {
				out = append(out, rootFields(doc, f.Sels, append(seen, s.Spread))...)
			}
		}
	}
	return out
}

// normalizeJSON turns a decoded JSON number that is whole into an int.
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<52 {
			return int(x)
		}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
		f, _ := x.Float64()
		return f
	case []any:
		for i := range x {
			x[i] = normalizeJSON(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = normalizeJSON(x[k])
		}
	}
	return v
}

func evalValue(v gqlValue, vars map[string]any) (any, error) {
	switch v.Kind {
	case valVar:
		return vars[v.Str], nil
	case valInt:
		return v.Int, nil
	case valFloat:
		return v.Float, nil
	case valString, valEnum:
		return v.Str, nil
	case valBool:
		return v.Bool, nil
	case valNull:
		return nil, nil
	case valList:
		out := make([]any, 0, len(v.List))
		for _, e := range v.List {
			x, err := evalValue(e, vars)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	case valObject:
		out := map[string]any{}
		for _, f := range v.Fields {
			x, err := evalValue(f.Value, vars)
			if err != nil {
				return nil, err
			}
			out[f.Name] = x
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown value kind %d", v.Kind)
}

func (x *gqlExec) args(list []gqlArg) (args, error) {
	out := args{}
	for _, a := range list {
		v, err := evalValue(a.Value, x.vars)
		if err != nil {
			return nil, err
		}
		out[a.Name] = v
	}
	return out, nil
}

// skipped applies @skip and @include.
func (x *gqlExec) skipped(dirs []gqlDirective) (bool, error) {
	for _, d := range dirs {
		if d.Name != "skip" && d.Name != "include" {
			return false, unsupported("the GraphQL directive @%s", d.Name)
		}
		a, err := x.args(d.Args)
		if err != nil {
			return false, err
		}
		cond := a.flag("if")
		if d.Name == "skip" && cond || d.Name == "include" && !cond {
			return true, nil
		}
	}
	return false, nil
}

// execute runs the operation: its data, the field errors beside it, and
// what the model lacked.
func (x *gqlExec) execute(op *gqlOperation) *gqlResult {
	x.doc, x.vars = op.Doc, op.Vars
	var root any = queryRoot{}
	res := &gqlResult{}
	if op.Op.Kind == "mutation" {
		root, res.Mutated = mutationRoot{}, true
	}
	data, err := x.object(root, op.Op.Sels, []any{})
	if err != nil {
		var un *unsupportedError
		var und *undefinedField
		switch {
		case errors.As(err, &und):
			res.Unsupported = und.Error()
			res.Errors = []gqlResponseError{{
				Path:       []any{op.Op.Kind, und.Field},
				Extensions: map[string]any{"code": "undefinedField", "typeName": und.Type, "fieldName": und.Field},
				Locations:  []gqlLocation{{Line: und.Line, Column: und.Col}},
				Message:    und.Error(),
			}}
		case errors.As(err, &un):
			res.Unsupported = un.What
			res.Errors = []gqlResponseError{{Message: "scenario GitHub does not model " + un.What}}
		default:
			res.Errors = []gqlResponseError{{Message: err.Error()}}
		}
		return res
	}
	res.Data, res.Errors = data, x.errs
	return res
}

// undefinedField is a field the model's schema lacks, in GitHub's words.
type undefinedField struct {
	Type, Field string
	Line, Col   int
}

func (e *undefinedField) Error() string {
	return fmt.Sprintf("Field '%s' doesn't exist on type '%s'", e.Field, e.Type)
}

// object resolves a selection set on an object.
func (x *gqlExec) object(obj any, sels []gqlSel, path []any) (*omap, error) {
	out := newOmap()
	if err := x.collect(out, obj, sels, path); err != nil {
		return nil, err
	}
	return out, nil
}

func typeName(obj any) string {
	if t, ok := obj.(gqlTyped); ok {
		return t.gqlType()
	}
	return fmt.Sprintf("%T", obj)
}

// typeMatches says whether a type condition applies to an object: its own
// type, or an interface or union it belongs to. A condition on a type the
// model never produces applies to nothing.
func typeMatches(obj any, on string) bool {
	if on == "" {
		return true
	}
	t := typeName(obj)
	return t == on || slices.Contains(abstractTypes[t], on)
}

func (x *gqlExec) collect(out *omap, obj any, sels []gqlSel, path []any) error {
	for _, s := range sels {
		skip, err := x.skipped(s.Dirs)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		switch {
		case s.Field != nil:
			f := s.Field
			v, err := x.field(obj, f, append(path, f.Key()))
			if err != nil {
				return err
			}
			out.set(f.Key(), v)
		case s.Inline != nil:
			if typeMatches(obj, s.Inline.On) {
				if err := x.collect(out, obj, s.Inline.Sels, path); err != nil {
					return err
				}
			}
		default:
			frag, ok := x.doc.Fragments[s.Spread]
			if !ok {
				return &gqlSyntaxError{Line: 1, Col: 1, Msg: fmt.Sprintf("Fragment %s was used, but not defined", s.Spread)}
			}
			if slices.Contains(x.spreads, s.Spread) {
				return &gqlSyntaxError{Line: 1, Col: 1, Msg: fmt.Sprintf("Fragment %s contains an infinite loop", s.Spread)}
			}
			if typeMatches(obj, frag.On) {
				x.spreads = append(x.spreads, s.Spread)
				err := x.collect(out, obj, frag.Sels, path)
				x.spreads = x.spreads[:len(x.spreads)-1]
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// field resolves one field of an object and completes its value.
func (x *gqlExec) field(obj any, f *gqlField, path []any) (any, error) {
	if f.Name == "__typename" {
		return typeName(obj), nil
	}
	a, err := x.args(f.Args)
	if err != nil {
		return nil, err
	}
	var v any
	if c, ok := obj.(*connection); ok {
		v = c.field(x, f.Name, a)
	} else if p, ok := obj.(*payload); ok {
		got, known := p.fields[f.Name]
		if !known {
			return nil, &undefinedField{Type: p.typ, Field: f.Name, Line: f.Line, Col: f.Col}
		}
		v = got
	} else {
		t := typeName(obj)
		fields, known := schema[t]
		if !known {
			return nil, unsupported("the GraphQL type %s (field %s)", t, f.Name)
		}
		resolve, ok := fields[f.Name]
		if !ok {
			return nil, &undefinedField{Type: t, Field: f.Name, Line: f.Line, Col: f.Col}
		}
		v = resolve(x, obj, a)
	}
	return x.complete(v, f, path)
}

// complete turns a resolved value into its JSON: scalars as they are,
// times in RFC 3339, lists by element and objects by their selections.
func (x *gqlExec) complete(v any, f *gqlField, path []any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case *gqlError:
		x.errs = append(x.errs, gqlResponseError{Type: t.Type, Path: slices.Clone(path), Locations: []gqlLocation{{Line: f.Line, Column: f.Col}}, Message: t.Message})
		return nil, nil
	case error:
		return nil, t
	case string, bool, int, int64, float64:
		if len(f.Sels) > 0 {
			return nil, &gqlSyntaxError{Line: f.Line, Col: f.Col, Msg: fmt.Sprintf("Selections can't be made on scalars (field '%s' returns a scalar but has selections)", f.Name)}
		}
		return v, nil
	case time.Time:
		if t.IsZero() {
			return nil, nil
		}
		return t.UTC().Format(time.RFC3339), nil
	case *time.Time:
		if t == nil || t.IsZero() {
			return nil, nil
		}
		return t.UTC().Format(time.RFC3339), nil
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for i, e := range t {
			c, err := x.complete(e, f, append(path, i))
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	}
	if isNilObject(v) {
		return nil, nil
	}
	if len(f.Sels) == 0 {
		return nil, &gqlSyntaxError{Line: f.Line, Col: f.Col, Msg: fmt.Sprintf("Field must have selections (field '%s' returns %s but has no selections. Did you mean '%s { ... }'?)", f.Name, typeName(v), f.Name)}
	}
	return x.object(v, f.Sels, path)
}

// isNilObject says whether an object value is a nil pointer of a model
// type: a field of no object is null.
func isNilObject(v any) bool {
	switch t := v.(type) {
	case *Issue:
		return t == nil
	case *Pull:
		return t == nil
	case *Repo:
		return t == nil
	case *Account:
		return t == nil
	case *Project:
		return t == nil
	case *ProjectItem:
		return t == nil
	case *connection:
		return t == nil
	case *commitObj:
		return t == nil
	case *fieldValueObj:
		return t == nil
	case *Release:
		return t == nil
	case *Protection:
		return t == nil
	case *refObj:
		return t == nil
	case *payload:
		return t == nil
	case *Label:
		return t == nil
	case *Review:
		return t == nil
	case *Comment:
		return t == nil
	}
	return false
}

// connection is a paginated list. Its page is cut when a field needs it,
// so a query of totalCount alone needs no first or last.
type connection struct {
	name  string // the field, for GitHub's messages
	items []any
	a     args
	// extra are the fields a kind of connection adds (issueCount).
	extra map[string]func() any
	// noLimit lifts GitHub's 100 records a page.
	noLimit bool
}

func (c *connection) gqlType() string { return "Connection" }

func connect[T any](name string, items []T, a args) *connection {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return &connection{name: name, items: out, a: a}
}

func cursorOf(i int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(i+1)))
}

func cursorIndex(c string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	return n - 1, err == nil && n >= 1
}

// page cuts the connection by first/after or last/before: the half-open
// range of its items.
func (c *connection) page() (from, to int, gerr *gqlError) {
	n := len(c.items)
	first, hasFirst := c.a.num("first")
	last, hasLast := c.a.num("last")
	if !hasFirst && !hasLast {
		return 0, 0, &gqlError{Type: "MISSING_PAGINATION_BOUNDARIES",
			Message: fmt.Sprintf("You must provide a `first` or `last` value to properly paginate the `%s` connection.", c.name)}
	}
	for arg, size := range map[string]int{"first": first, "last": last} {
		if size > 100 && !c.noLimit {
			return 0, 0, &gqlError{Type: "EXCESSIVE_PAGINATION",
				Message: fmt.Sprintf("Requesting %d records on the `%s` connection exceeds the `%s` limit of 100 records.", size, c.name, arg)}
		}
		if size < 0 {
			return 0, 0, &gqlError{Type: "ARGUMENT_LIMIT", Message: fmt.Sprintf("`%s` on the `%s` connection cannot be less than zero.", arg, c.name)}
		}
	}
	from, to = 0, n
	if after := c.a.str("after"); after != "" {
		i, ok := cursorIndex(after)
		if !ok {
			return 0, 0, &gqlError{Type: "INVALID_CURSOR_ARGUMENTS", Message: fmt.Sprintf("`%s` does not appear to be a valid cursor.", after)}
		}
		from = min(i+1, n)
	}
	if before := c.a.str("before"); before != "" {
		i, ok := cursorIndex(before)
		if !ok {
			return 0, 0, &gqlError{Type: "INVALID_CURSOR_ARGUMENTS", Message: fmt.Sprintf("`%s` does not appear to be a valid cursor.", before)}
		}
		to = max(min(i, n), from)
	}
	if hasFirst {
		to = min(to, from+first)
	}
	if hasLast {
		from = max(from, to-last)
	}
	return from, to, nil
}

func (c *connection) field(x *gqlExec, name string, _ args) any {
	switch name {
	case "totalCount":
		return len(c.items)
	case "nodes":
		from, to, gerr := c.page()
		if gerr != nil {
			return gerr
		}
		return slices.Clone(c.items[from:to])
	case "edges":
		from, to, gerr := c.page()
		if gerr != nil {
			return gerr
		}
		out := make([]any, 0, to-from)
		for i := from; i < to; i++ {
			out = append(out, &payload{typ: "Edge", fields: map[string]any{"node": c.items[i], "cursor": cursorOf(i)}})
		}
		return out
	case "pageInfo":
		from, to, gerr := c.page()
		if gerr != nil {
			return gerr
		}
		info := map[string]any{"hasNextPage": to < len(c.items), "hasPreviousPage": from > 0, "startCursor": nil, "endCursor": nil}
		if to > from {
			info["startCursor"], info["endCursor"] = cursorOf(from), cursorOf(to-1)
		}
		return &payload{typ: "PageInfo", fields: info}
	}
	if fn, ok := c.extra[name]; ok {
		return fn()
	}
	return &undefinedField{Type: "Connection of " + c.name, Field: name}
}

// payload is an object of fixed fields: a mutation's payload, a page's
// info, a summary.
type payload struct {
	typ    string
	fields map[string]any
}

func (p *payload) gqlType() string { return p.typ }

// queryCost is a query's cost in points as GitHub documents it: the
// requests needed to fulfil every connection, each assumed to reach its
// first or last, divided by 100 and rounded, at least 1.
func queryCost(op *gqlOperation) int {
	requests := costOf(op.Doc, op.Op.Sels, op.Vars, 1, nil)
	return max(1, int(math.Round(float64(requests)/100)))
}

func costOf(doc *gqlDoc, sels []gqlSel, vars map[string]any, parents int, seen []string) int {
	total := 0
	for _, s := range sels {
		switch {
		case s.Field != nil:
			children := parents
			size := 0
			for _, a := range s.Field.Args {
				if a.Name != "first" && a.Name != "last" {
					continue
				}
				if v, err := evalValue(a.Value, vars); err == nil {
					if n, ok := (args{"n": v}).num("n"); ok {
						size = max(size, n)
					}
				}
			}
			if size > 0 {
				total += parents
				children = parents * size
			}
			total += costOf(doc, s.Field.Sels, vars, children, seen)
		case s.Inline != nil:
			total += costOf(doc, s.Inline.Sels, vars, parents, seen)
		case s.Spread != "" && !slices.Contains(seen, s.Spread):
			if f, ok := doc.Fragments[s.Spread]; ok {
				total += costOf(doc, f.Sels, vars, parents, append(seen, s.Spread))
			}
		}
	}
	return total
}

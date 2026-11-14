package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Expected is what a step expects of its tool's result (contract §4.3). One evaluator handles every
// kind for every tool.
type Expected struct {
	// Success defaults to true.
	Success *bool `yaml:"success"`
	// ErrorContains are case-insensitive substrings of the error (with success: false).
	ErrorContains []string `yaml:"error_contains"`
	// JSONPath maps dotted paths to expected values: maps match partially, arrays exactly (their
	// elements partially), null means absent or empty.
	JSONPath map[string]any `yaml:"json_path"`
	// JSONPathMax and JSONPathMin are numeric bounds (inclusive).
	JSONPathMax map[string]any `yaml:"json_path_max"`
	JSONPathMin map[string]any `yaml:"json_path_min"`
	// Contains and NotContains are case-insensitive substrings of the result's JSON.
	Contains    []string `yaml:"contains"`
	NotContains []string `yaml:"not_contains"`
	// WaitForState retries the step until the other expectations hold, for at most this long.
	WaitForState string `yaml:"wait_for_state"`
}

// wantSuccess reports whether the step must succeed.
func (e *Expected) wantSuccess() bool { return e == nil || e.Success == nil || *e.Success }

// wait returns the wait_for_state duration.
func (e *Expected) wait() time.Duration {
	if e == nil || e.WaitForState == "" {
		return 0
	}
	d, _ := time.ParseDuration(e.WaitForState)
	return d
}

// asserts reports whether e checks anything beyond success.
func (e *Expected) asserts() bool {
	return e != nil && len(e.JSONPath)+len(e.JSONPathMax)+len(e.JSONPathMin)+len(e.Contains)+len(e.NotContains)+len(e.ErrorContains) > 0
}

// check validates e at load time.
func (e *Expected) check() []string {
	var p []string
	if e.WaitForState != "" {
		if d, err := time.ParseDuration(e.WaitForState); err != nil || d <= 0 {
			p = append(p, fmt.Sprintf("bad wait_for_state %q", e.WaitForState))
		}
	}
	if len(e.ErrorContains) > 0 && e.wantSuccess() {
		p = append(p, "error_contains needs success: false")
	}
	if !e.wantSuccess() && len(e.JSONPath)+len(e.JSONPathMax)+len(e.JSONPathMin)+len(e.Contains)+len(e.NotContains) > 0 {
		p = append(p, "success: false can only be combined with error_contains")
	}
	for _, m := range []map[string]any{e.JSONPath, e.JSONPathMax, e.JSONPathMin} {
		for _, k := range sortedKeys(m) {
			if _, err := parsePath(k); err != nil {
				p = append(p, err.Error())
			}
		}
	}
	for name, m := range map[string]map[string]any{"json_path_max": e.JSONPathMax, "json_path_min": e.JSONPathMin} {
		for _, k := range sortedKeys(m) {
			if _, ok := toFloat(m[k]); !ok {
				p = append(p, fmt.Sprintf("%s %s: %v is not a number", name, k, m[k]))
			}
		}
	}
	for _, s := range append(append(append([]string{}, e.Contains...), e.NotContains...), e.ErrorContains...) {
		if s == "" {
			p = append(p, "empty substring")
		}
	}
	return p
}

// Evaluate checks a tool's result and error against e and returns every unmet expectation.
func (e *Expected) Evaluate(result any, err error) []string {
	if err != nil {
		if e.wantSuccess() {
			return []string{fmt.Sprintf("the step failed: %v", err)}
		}
		msg := strings.ToLower(err.Error())
		if strings.TrimSpace(msg) == "" {
			return []string{"the step failed with an empty error"}
		}
		var out []string
		for _, s := range e.ErrorContains {
			if !strings.Contains(msg, strings.ToLower(s)) {
				out = append(out, fmt.Sprintf("error_contains %q: error is %q", s, err.Error()))
			}
		}
		return out
	}
	if !e.wantSuccess() {
		return []string{fmt.Sprintf("expected the step to fail, it returned %s", render(result))}
	}
	if e == nil {
		return nil
	}
	doc, nerr := Normalize(result)
	if nerr != nil {
		return []string{fmt.Sprintf("result is not JSON: %v", nerr)}
	}
	var out []string
	for _, k := range sortedKeys(e.JSONPathMax) {
		out = append(out, bound("json_path_max", k, doc, e.JSONPathMax[k], func(got, lim float64) bool { return got <= lim })...)
	}
	for _, k := range sortedKeys(e.JSONPathMin) {
		out = append(out, bound("json_path_min", k, doc, e.JSONPathMin[k], func(got, lim float64) bool { return got >= lim })...)
	}
	for _, k := range sortedKeys(e.JSONPath) {
		want, nerr := Normalize(e.JSONPath[k])
		if nerr != nil {
			out = append(out, fmt.Sprintf("json_path %s: %v", k, nerr))
			continue
		}
		got, found, rerr := resolve(doc, k)
		if rerr != nil {
			out = append(out, fmt.Sprintf("json_path %s: %v", k, rerr))
			continue
		}
		out = append(out, match("json_path "+k, want, got, found)...)
	}
	text := strings.ToLower(render(doc))
	for _, s := range e.Contains {
		if !strings.Contains(text, strings.ToLower(s)) {
			out = append(out, fmt.Sprintf("contains %q: not in %s", s, abbreviate(render(doc))))
		}
	}
	for _, s := range e.NotContains {
		if strings.Contains(text, strings.ToLower(s)) {
			out = append(out, fmt.Sprintf("not_contains %q: found in %s", s, abbreviate(render(doc))))
		}
	}
	return out
}

func bound(kind, path string, doc, limit any, ok func(got, lim float64) bool) []string {
	lim, _ := toFloat(limit)
	v, found, err := resolve(doc, path)
	if err != nil {
		return []string{fmt.Sprintf("%s %s: %v", kind, path, err)}
	}
	if !found {
		return []string{fmt.Sprintf("%s %s: not found", kind, path)}
	}
	got, isNum := toFloat(v)
	if !isNum {
		return []string{fmt.Sprintf("%s %s: %s is not a number", kind, path, render(v))}
	}
	if !ok(got, lim) {
		return []string{fmt.Sprintf("%s %s: measured %s, bound %s", kind, path, fmtNum(got), fmtNum(lim))}
	}
	return nil
}

// match compares an expected value with the value found at path.
func match(path string, want, got any, found bool) []string {
	if want == nil {
		if !found || isEmpty(got) {
			return nil
		}
		return []string{fmt.Sprintf("%s: want absent or empty, got %s", path, render(got))}
	}
	if !found {
		return []string{fmt.Sprintf("%s: want %s, not found", path, render(want))}
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want a map %s, got %s", path, render(want), render(got))}
		}
		var out []string
		for _, k := range sortedKeys(w) {
			v, f, err := resolve(g, k)
			if err != nil {
				out = append(out, fmt.Sprintf("%s.%s: %v", path, k, err))
				continue
			}
			out = append(out, match(path+"."+k, w[k], v, f)...)
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want a list %s, got %s", path, render(want), render(got))}
		}
		if len(g) != len(w) {
			return []string{fmt.Sprintf("%s: want %d elements %s, got %d %s", path, len(w), render(want), len(g), render(got))}
		}
		var out []string
		for i := range w {
			out = append(out, match(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], true)...)
		}
		return out
	default:
		if scalarEqual(want, got) {
			return nil
		}
		return []string{fmt.Sprintf("%s: want %s, got %s", path, render(want), render(got))}
	}
}

// scalarEqual compares loosely: numbers numerically, booleans with "true"/"false", times as instants,
// durations by value, everything else by its string form.
func scalarEqual(want, got any) bool {
	if wf, ok := toFloat(want); ok {
		if gf, ok := toFloat(got); ok {
			return wf == gf || math.Abs(wf-gf) <= 1e-9*math.Max(math.Abs(wf), math.Abs(gf))
		}
	}
	ws, gs := scalarString(want), scalarString(got)
	if ws == gs {
		return true
	}
	if _, isMap := got.(map[string]any); isMap {
		return false
	}
	if _, isList := got.([]any); isList {
		return false
	}
	if wt, err := time.Parse(time.RFC3339Nano, ws); err == nil {
		if gt, err := time.Parse(time.RFC3339Nano, gs); err == nil {
			return wt.Equal(gt)
		}
	}
	if wd, err := time.ParseDuration(ws); err == nil {
		if gd, err := time.ParseDuration(gs); err == nil {
			return wd == gd
		}
	}
	return false
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case float64:
		return fmtNum(x)
	default:
		return fmt.Sprint(x)
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// Normalize turns a value into its JSON form: maps, lists, float64, string, bool and nil.
func Normalize(v any) (any, error) {
	b, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func render(v any) string {
	b, err := marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func abbreviate(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("… (%d bytes)", len(s))
}

// segment is one step of a path: a map key, an array index, or a [key=value] selector.
type segment struct {
	key    string
	index  int // -1 when not an index
	selKey string
	selVal string
}

func (s segment) String() string {
	switch {
	case s.selKey != "":
		return "[" + s.selKey + "=" + s.selVal + "]"
	case s.index >= 0:
		return strconv.Itoa(s.index)
	default:
		return s.key
	}
}

// parsePath splits a dotted path: a.b.0.c, a[k=v].b, a[0].
func parsePath(p string) ([]segment, error) {
	if p == "" {
		return nil, fmt.Errorf("empty path")
	}
	var segs []segment
	i := 0
	for i < len(p) {
		switch p[i] {
		case '.':
			if i == 0 || i == len(p)-1 || p[i+1] == '.' {
				return nil, fmt.Errorf("path %q: empty segment", p)
			}
			i++
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q: unclosed [", p)
			}
			inner := p[i+1 : i+end]
			if k, v, ok := strings.Cut(inner, "="); ok {
				if k == "" {
					return nil, fmt.Errorf("path %q: empty selector key", p)
				}
				segs = append(segs, segment{index: -1, selKey: k, selVal: v})
			} else if n, err := strconv.Atoi(inner); err == nil && n >= 0 {
				segs = append(segs, segment{index: n, key: inner})
			} else {
				return nil, fmt.Errorf("path %q: bad selector [%s]", p, inner)
			}
			i += end + 1
			if i < len(p) && p[i] != '.' && p[i] != '[' {
				return nil, fmt.Errorf("path %q: junk after ]", p)
			}
		default:
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			name := p[i:j]
			seg := segment{key: name, index: -1}
			if n, err := strconv.Atoi(name); err == nil && n >= 0 {
				seg.index = n
			}
			segs = append(segs, seg)
			i = j
		}
	}
	return segs, nil
}

// resolve finds path in doc. found is false when a step of the path doesn't exist; err is set for a
// path that can't apply to the document's shape (indexing a string).
func resolve(doc any, path string) (any, bool, error) {
	segs, err := parsePath(path)
	if err != nil {
		return nil, false, err
	}
	cur := doc
	for _, s := range segs {
		switch c := cur.(type) {
		case map[string]any:
			if s.selKey != "" {
				return nil, false, fmt.Errorf("selector %s on a map", s)
			}
			v, ok := c[s.key]
			if !ok {
				return nil, false, nil
			}
			cur = v
		case []any:
			switch {
			case s.selKey != "":
				var hit any
				found := false
				for _, e := range c {
					if m, ok := e.(map[string]any); ok {
						if v, ok := m[s.selKey]; ok && scalarEqual(s.selVal, v) {
							hit, found = e, true
							break
						}
					}
				}
				if !found {
					return nil, false, nil
				}
				cur = hit
			case s.index >= 0:
				if s.index >= len(c) {
					return nil, false, nil
				}
				cur = c[s.index]
			default:
				return nil, false, fmt.Errorf("key %q on a list", s.key)
			}
		case nil:
			return nil, false, nil
		default:
			return nil, false, fmt.Errorf("%s on %s", s, render(cur))
		}
	}
	return cur, true, nil
}

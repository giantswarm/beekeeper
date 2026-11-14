package harness

import (
	"fmt"
	"sort"
)

// identityKeys name the objects of a list, in the order they are tried: issues and pulls by ref,
// teams, repos and checks by name, people and org members by login, durations by repo.
var identityKeys = []string{"ref", "name", "login", "context", "repo"}

// Merge returns over merged onto base (contract §4.1): maps merge key by key, lists of named objects
// merge element by element on their identity key (base order first, new elements appended in over's
// order), and everything else (scalars, lists of scalars, unnamed lists) is replaced by over. Neither
// argument is modified.
func Merge(base, over any) any {
	switch o := over.(type) {
	case map[string]any:
		b, ok := base.(map[string]any)
		if !ok {
			return clone(o)
		}
		out := clone(b).(map[string]any)
		for k, v := range o {
			out[k] = Merge(b[k], v)
		}
		return out
	case []any:
		b, ok := base.([]any)
		if !ok {
			return clone(o)
		}
		key := identityKey(b, o)
		if key == "" {
			return clone(o)
		}
		out := make([]any, 0, len(b)+len(o))
		pos := map[string]int{}
		for _, e := range b {
			pos[idOf(e, key)] = len(out)
			out = append(out, clone(e))
		}
		for _, e := range o {
			id := idOf(e, key)
			if i, ok := pos[id]; ok {
				out[i] = Merge(out[i], e)
				continue
			}
			pos[id] = len(out)
			out = append(out, clone(e))
		}
		return out
	default:
		return clone(over)
	}
}

// identityKey returns the key every element of both lists is named by, or "" when there is none.
func identityKey(a, b []any) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	for _, k := range identityKeys {
		ok := true
		for _, l := range [][]any{a, b} {
			for _, e := range l {
				m, isMap := e.(map[string]any)
				if !isMap {
					return ""
				}
				switch m[k].(type) {
				case string, int, int64, float64:
				default:
					ok = false
				}
			}
		}
		if ok {
			return k
		}
	}
	return ""
}

func idOf(e any, key string) string { return fmt.Sprint(e.(map[string]any)[key]) }

// clone deep-copies maps and lists.
func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clone(e)
		}
		return out
	default:
		return v
	}
}

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

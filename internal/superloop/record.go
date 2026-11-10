package superloop

import (
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Recording projects planner data into publishable JSON values. Unknown string
// fields are dropped by default; adding a field cannot silently publish text.
// Functions are omitted: recordings contain data, never live query callbacks.
func Recording(input any) any {
	r := recorder{ids: map[string]string{}}
	return r.value(reflect.ValueOf(input), "")
}

type recorder struct{ ids map[string]string }

func (r *recorder) token(s string) string {
	if s == "" {
		return ""
	}
	if id, ok := r.ids[s]; ok {
		return id
	}
	id := fmt.Sprintf("id%08d", len(r.ids)+1)
	r.ids[s] = id
	return id
}

func (r *recorder) ref(s string) string {
	if s == "" {
		return ""
	}
	repo, number, hasNumber := strings.Cut(s, "#")
	owner, name, hasRepo := strings.Cut(repo, "/")
	if !hasRepo {
		return r.token(s)
	}
	result := r.token(owner) + "/" + r.token(name)
	if hasNumber {
		result += "#" + strings.TrimPrefix(r.token("number:"+number), "id")
	}
	return result
}

func (r *recorder) value(v reflect.Value, field string) any {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		return r.value(v.Elem(), field)
	}
	if v.Type() == reflect.TypeFor[time.Time]() {
		return v.Interface()
	}
	switch v.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() || v.Field(i).Kind() == reflect.Func {
				continue
			}
			// Preserve JSON names for state types, which use lower-case tags.
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			value := r.value(v.Field(i), f.Name)
			if f.Anonymous && f.Tag.Get("json") == "" {
				if fields, ok := value.(map[string]any); ok {
					for key, item := range fields {
						out[key] = item
					}
					continue
				}
			}
			out[name] = value
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := range v.Len() {
			out[i] = r.value(v.Index(i), field)
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, key := range keys {
			out[r.ref(key.String())] = r.value(v.MapIndex(key), field)
		}
		return out
	case reflect.String:
		s := v.String()
		switch field {
		case "Task":
			if s != "" {
				return "task"
			}
		case "URL":
			u, err := url.Parse(s)
			if err == nil && u.Host == "github.com" {
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				if len(parts) == 4 && (parts[2] == "pull" || parts[2] == "issues") {
					ref := r.ref(parts[0] + "/" + parts[1] + "#" + parts[3])
					repo, number, _ := strings.Cut(ref, "#")
					return "https://github.com/" + repo + "/" + parts[2] + "/" + number
				}
			}
			if s != "" {
				return "https://example.invalid/" + r.token(s)
			}
		case "Ref", "On", "ParkedOn", "Serves", "From", "Closes", "Absorbs", "Issues", "Mentions", "OpenPulls", "Review", "Never", "Admin", "Pulls":
			return r.ref(s)
		case "Name", "Agent", "AgentName", "Session", "Author", "Authors", "Bots", "By", "For", "Person", "Orgs", "DevctlOwners", "Team", "Head", "Heads", "ParkedHead", "ReviewHead", "Commit", "MergeCommit", "ID":
			return r.token(s)
		case "State", "Step", "Kind", "Verb", "Verdict", "Mergeable", "MergeState", "ReviewDecision", "RefKind", "MergeMethods":
			if slices.Contains([]string{"open", "base-merged", "nits-checked", "OPEN", "CLOSED", "MERGED", "APPROVED", "REVIEW_REQUIRED", "CHANGES_REQUESTED", "CONFLICTING", "DIRTY", "CLEAN", "MERGEABLE", "UNKNOWN", "approved", "changes requested", "review-queued", "fixing", "reviewing", "merging", "pull", "issue", "squash", "merge", "rebase"}, s) || slices.Contains(Verbs(), s) || slices.Contains([]string{VerbDone, VerbIdle, VerbReport, VerbProblem, VerbPark}, s) {
				return s
			}
		case "Continuation":
			if s != "" {
				return "event"
			}
		}
		return ""
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if (field == "ID" || field == "Note") && v.Int() != 0 {
			var n int
			_, _ = fmt.Sscanf(r.token(fmt.Sprintf("number:%d", v.Int())), "id%d", &n)
			return n
		}
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		return v.Float()
	}
	return nil
}

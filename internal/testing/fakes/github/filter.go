package fakegithub

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The board's filter language (ProjectV2.items(query:)), as far as the engine uses it: qualifiers
// with comma-separated values (OR), quoted names with emoji, negation, is:, no:, has:, updated: with
// day granularity. Like GitHub, an unknown option name, an unknown qualifier or a timestamp matches
// nothing, silently.

type filterTerm struct {
	neg    bool
	qual   string // "" for free text
	values []string
}

func parseFilter(q string) ([]filterTerm, error) {
	var terms []filterTerm
	i := 0
	for i < len(q) {
		if q[i] == ' ' {
			i++
			continue
		}
		var t filterTerm
		if q[i] == '-' {
			t.neg = true
			i++
		}
		// qualifier: letters, digits and hyphens up to ':'
		j := i
		for j < len(q) && (q[j] == '-' || q[j] == '_' || (q[j] >= 'a' && q[j] <= 'z') || (q[j] >= 'A' && q[j] <= 'Z') || (q[j] >= '0' && q[j] <= '9')) {
			j++
		}
		if j < len(q) && q[j] == ':' && j > i {
			t.qual = strings.ToLower(q[i:j])
			i = j + 1
		}
		// values
		for {
			var v string
			if i < len(q) && q[i] == '"' {
				var b strings.Builder
				k := i + 1
				for ; k < len(q) && q[k] != '"'; k++ {
					if q[k] == '\\' && k+1 < len(q) {
						k++
					}
					b.WriteByte(q[k])
				}
				if k >= len(q) {
					return nil, fmt.Errorf("unterminated quote in filter %q", q)
				}
				v, i = b.String(), k+1
			} else {
				k := i
				for k < len(q) && q[k] != ' ' && q[k] != ',' {
					k++
				}
				v, i = q[i:k], k
			}
			t.values = append(t.values, v)
			if i < len(q) && q[i] == ',' {
				i++
				continue
			}
			break
		}
		terms = append(terms, t)
	}
	return terms, nil
}

// matches reports whether an item matches every term.
func (s *state) matches(it *boardItem, terms []filterTerm, now time.Time) bool {
	for _, t := range terms {
		if s.matchTerm(it, t, now) == t.neg {
			return false
		}
	}
	return true
}

func (s *state) fieldValue(it *boardItem, field string) (string, bool) {
	is := s.issues[it.issue]
	switch field {
	case "status":
		return it.status, true
	case "team":
		return it.team, true
	case "kind":
		return it.kind, true
	case "priority":
		if it.priority == nil {
			return "", true
		}
		return strconv.FormatFloat(*it.priority, 'f', -1, 64), true
	case "assignee", "assignees":
		if is == nil || len(is.assignees) == 0 {
			return "", true
		}
		return strings.Join(is.assignees, ","), true
	case "parent-issue":
		if is == nil {
			return "", true
		}
		return is.parent, true
	case "label", "labels":
		if is == nil || len(is.labels) == 0 {
			return "", true
		}
		return strings.Join(is.labels, ","), true
	}
	return "", false
}

func (s *state) matchTerm(it *boardItem, t filterTerm, now time.Time) bool {
	is := s.issues[it.issue]
	anyOf := func(f func(v string) bool) bool {
		for _, v := range t.values {
			if f(v) {
				return true
			}
		}
		return false
	}
	switch t.qual {
	case "":
		return is != nil && anyOf(func(v string) bool { return strings.Contains(strings.ToLower(is.title), strings.ToLower(v)) })
	case "status", "team", "kind":
		val, _ := s.fieldValue(it, t.qual)
		return anyOf(func(v string) bool { return v == val })
	case "is":
		return anyOf(func(v string) bool {
			switch v {
			case stateOpen:
				return is != nil && is.state == stateOpen
			case stateClosed:
				return is != nil && is.state == stateClosed
			case "issue":
				return is != nil
			case "pr", "draft":
				return false
			case "archived":
				return it.archived
			}
			return false
		})
	case "no", "has":
		return anyOf(func(v string) bool {
			val, known := s.fieldValue(it, strings.ToLower(v))
			if !known {
				return false
			}
			return (val == "") == (t.qual == "no")
		})
	case "repo":
		return is != nil && anyOf(func(v string) bool { return v == is.repo })
	case "assignee":
		return is != nil && anyOf(func(v string) bool { return contains(is.assignees, v) })
	case "label":
		return is != nil && anyOf(func(v string) bool { return contains(is.labels, v) })
	case "updated":
		return anyOf(func(v string) bool { return matchDate(it.updatedAt, v, now) })
	case "priority":
		return it.priority != nil && anyOf(func(v string) bool { return matchNumber(*it.priority, v) })
	}
	return false // unknown qualifiers match nothing, as on GitHub
}

func splitComparator(v string) (op, rest string) {
	for _, p := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(v, p) {
			return p, strings.TrimPrefix(v, p)
		}
	}
	return "=", v
}

// matchDate compares a time's UTC day with a date (YYYY-MM-DD or @today[-Nd]); anything finer than
// a day matches nothing.
func matchDate(t time.Time, v string, now time.Time) bool {
	op, rest := splitComparator(v)
	var day string
	switch {
	case strings.HasPrefix(rest, "@today"):
		d := now.UTC()
		if off := strings.TrimPrefix(rest, "@today"); off != "" {
			if !strings.HasPrefix(off, "-") || !strings.HasSuffix(off, "d") {
				return false
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(off, "-"), "d"))
			if err != nil {
				return false
			}
			d = d.AddDate(0, 0, -n)
		}
		day = d.Format(time.DateOnly)
	default:
		if _, err := time.Parse(time.DateOnly, rest); err != nil {
			return false
		}
		day = rest
	}
	got := t.UTC().Format(time.DateOnly)
	switch op {
	case ">=":
		return got >= day
	case "<=":
		return got <= day
	case ">":
		return got > day
	case "<":
		return got < day
	}
	return got == day
}

func matchNumber(n float64, v string) bool {
	op, rest := splitComparator(v)
	x, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return false
	}
	switch op {
	case ">=":
		return n >= x
	case "<=":
		return n <= x
	case ">":
		return n > x
	case "<":
		return n < x
	}
	return n == x
}

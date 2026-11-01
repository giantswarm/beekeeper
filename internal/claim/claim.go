// Package claim is a desk's claim of an issue as GitHub carries it: one
// marker comment per issue and desk, which every desk working the same
// repository reads, whatever its configuration, board or machine.
//
// The comment's first line is the marker, an HTML comment GitHub does not
// render; a sentence for people follows it:
//
//	<!-- beekeeper:claim desk=platform-desk session=0b5c… at=2026-11-01T00:12:00Z seen=2026-11-01T06:12:00Z -->
//	Claimed by beekeeper desk platform-desk for session "0b5c…" since 2026-11-01 00:12 UTC.
//
// A desk refreshes seen while its claim holds and adds released=<time> when
// the claim ends; it edits its one comment in place, never adding another.
package claim

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// prefix opens a marker; markerEnd closes it.
const (
	prefix    = "<!-- beekeeper:claim "
	markerEnd = "-->"
)

// Marker is one desk's claim of an issue: the desk's configured name, the
// session holding it, since when, when the desk last refreshed it and, once
// it ended, when.
type Marker struct {
	Desk     string
	Session  string
	At       time.Time
	Seen     time.Time
	Released time.Time
}

// Comment is a marker comment as GitHub keeps it: its id and its marker.
type Comment struct {
	ID int64 `json:"id"`
	Marker
}

// Open says whether the claim holds: not released.
func (m Marker) Open() bool { return m.Released.IsZero() }

// Stale says whether an open claim went unrefreshed for longer than after
// at now: shown, not honoured.
func (m Marker) Stale(now time.Time, after time.Duration) bool {
	return after > 0 && now.Sub(cmp.Or(m.Seen, m.At)) > after
}

// Body is the comment's text: the marker line and the sentence for people.
func (m Marker) Body() string {
	var b strings.Builder
	b.WriteString(prefix)
	fmt.Fprintf(&b, "desk=%s session=%s at=%s", quote(m.Desk), quote(m.Session), stamp(m.At))
	if !m.Seen.IsZero() {
		fmt.Fprintf(&b, " seen=%s", stamp(m.Seen))
	}
	if !m.Released.IsZero() {
		fmt.Fprintf(&b, " released=%s", stamp(m.Released))
	}
	b.WriteString(" " + markerEnd + "\n")
	if m.Open() {
		fmt.Fprintf(&b, "Claimed by beekeeper desk %s for session %q since %s.", clean(m.Desk), clean(m.Session), human(m.At))
	} else {
		fmt.Fprintf(&b, "Released by beekeeper desk %s at %s (claimed for session %q since %s).", clean(m.Desk), human(m.Released), clean(m.Session), human(m.At))
	}
	return b.String()
}

// Parse reads the marker a comment's body opens with; ok is false for any
// other comment, or a marker without a desk or a valid time.
func Parse(body string) (m Marker, ok bool) {
	body = strings.TrimLeft(body, " \t\r\n")
	rest, found := strings.CutPrefix(body, prefix)
	if !found {
		return Marker{}, false
	}
	inner, _, found := strings.Cut(rest, markerEnd)
	if !found {
		return Marker{}, false
	}
	fields, ok := split(inner)
	if !ok {
		return Marker{}, false
	}
	for k, v := range fields {
		var err error
		switch k {
		case "desk":
			m.Desk = v
		case "session":
			m.Session = v
		case "at":
			m.At, err = time.Parse(time.RFC3339, v)
		case "seen":
			m.Seen, err = time.Parse(time.RFC3339, v)
		case "released":
			m.Released, err = time.Parse(time.RFC3339, v)
		}
		if err != nil {
			return Marker{}, false
		}
	}
	return m, m.Desk != "" && !m.At.IsZero()
}

// split reads key=value pairs, a value bare or a Go-quoted string; a later
// key of the same name wins.
func split(s string) (map[string]string, bool) {
	out := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if s == "" {
			return out, true
		}
		k, rest, found := strings.Cut(s, "=")
		if !found || k == "" || strings.ContainsAny(k, " \t\r\n") {
			return nil, false
		}
		var v string
		if strings.HasPrefix(rest, `"`) {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return nil, false
			}
			if v, err = strconv.Unquote(q); err != nil {
				return nil, false
			}
			rest = rest[len(q):]
		} else {
			end := strings.IndexAny(rest, " \t\r\n")
			if end < 0 {
				end = len(rest)
			}
			v, rest = rest[:end], rest[end:]
		}
		out[k] = v
		s = rest
	}
}

// Desk is the comment of desk among cs, the oldest when there are several;
// nil when it has none.
func Desk(cs []Comment, desk string) *Comment {
	var mine *Comment
	for i := range cs {
		if strings.EqualFold(cs[i].Desk, desk) && (mine == nil || cs[i].ID < mine.ID) {
			mine = &cs[i]
		}
	}
	return mine
}

// Others are the open claims of desks other than desk among cs, oldest
// first: live ones, which hold, and stale ones (Stale), which do not.
func Others(cs []Comment, desk string, now time.Time, staleAfter time.Duration) (live, stale []Comment) {
	for _, c := range cs {
		if strings.EqualFold(c.Desk, desk) || !c.Open() {
			continue
		}
		if c.Stale(now, staleAfter) {
			stale = append(stale, c)
		} else {
			live = append(live, c)
		}
	}
	byID := func(a, b Comment) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(live, byID)
	slices.SortFunc(stale, byID)
	return live, stale
}

// Says is how a line names a claim: "desk lab (session "BK 1813") since
// 00:12 UTC", with "stale, " ahead of a stale one's.
func (m Marker) Says(now time.Time, staleAfter time.Duration) string {
	s := fmt.Sprintf("desk %s (session %q) since %s", m.Desk, m.Session, human(m.At))
	if m.Stale(now, staleAfter) {
		s = fmt.Sprintf("stale, %s, unrefreshed since %s", s, human(cmp.Or(m.Seen, m.At)))
	}
	return s
}

// quote is v as a marker value: bare when it is one token, quoted
// otherwise, never closing the HTML comment.
func quote(v string) string {
	v = clean(v)
	if v != "" && !strings.ContainsAny(v, " \t\r\n\"=\\") && strconv.CanBackquote(v) {
		return v
	}
	return strconv.Quote(v)
}

// clean keeps v on one line and out of the way of the comment's end.
func clean(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	for strings.Contains(v, "--") {
		v = strings.ReplaceAll(v, "--", "-")
	}
	return v
}

func stamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

func human(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

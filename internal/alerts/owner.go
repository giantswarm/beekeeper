package alerts

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Key is a firing alert's identity in the baseline: its fingerprint and its
// start, so an alert that resolves and fires again is a new one, owned by
// nobody.
func Key(fingerprint string, a Alert) string { return fingerprint + "@" + a.Since }

// Firing is one alert of the baseline with its key.
type Firing struct {
	Installation string
	Key          string
	Alert
}

// Name is the alert as alerts own takes it: <installation>/<alertname>.
func (f Firing) Name() string { return f.Installation + "/" + f.Alertname }

// Firing are the alerts of the reachable installations' baselines, at
// severity when it is not empty, by installation, alertname and object. An
// unreachable installation's last set is not firing: it may have resolved.
func (st *State) Firing(severity string) []Firing {
	var out []Firing
	for name, in := range st.Installations {
		if in == nil || !in.Reachable {
			continue
		}
		for fp, a := range in.Alerts {
			if severity == "" || a.Severity == severity {
				out = append(out, Firing{Installation: name, Key: Key(fp, a), Alert: a})
			}
		}
	}
	slices.SortFunc(out, func(a, b Firing) int {
		return cmp.Or(strings.Compare(a.Installation, b.Installation), strings.Compare(a.Alertname, b.Alertname),
			strings.Compare(a.Where, b.Where), strings.Compare(a.Key, b.Key))
	})
	return out
}

// Match are the firing alerts q names: <installation>/<alertname>, an
// alertname on any installation, or a fingerprint.
func Match(firing []Firing, q string) []Firing {
	var out []Firing
	for _, f := range firing {
		fp, _, _ := strings.Cut(f.Key, "@")
		if q == f.Name() || q == f.Alertname || q == fp {
			out = append(out, f)
		}
	}
	return out
}

// Unowned is a page that has had no owner for at least the grace period:
// the longest unowned of its installation's alerts of the same name.
type Unowned struct {
	Firing
	// More is how many more alerts of the name are unowned.
	More int
	// Since is when it was last owned, or its start when it never was.
	Since time.Time
	// For is how long it has had no owner.
	For time.Duration
	// Mark is the grace period it is in, unique per page and period: the
	// line is said once per mark.
	Mark string
}

// Line is the watch's line for the page.
func (u Unowned) Line() string {
	where := u.Where
	if u.More > 0 {
		where += fmt.Sprintf(" (+%d)", u.More)
	}
	return fmt.Sprintf("PAGE UNOWNED %s %s %s for %s: beekeeper alerts own %s", u.Installation, u.Alertname, where, ago(u.For), u.Name())
}

// UnownedPages are the pages among firing that have had no owner for grace
// or longer, one per installation and alertname. ownedUntil tells of an
// alert's key when its last owner stopped owning it: zero while one owns it,
// false when it never had one. A page is due once per grace period it stays
// unowned: its Mark changes with each.
func UnownedPages(firing []Firing, grace time.Duration, ownedUntil func(key string) (time.Time, bool), now time.Time) []Unowned {
	if grace <= 0 {
		return nil
	}
	var out []Unowned
	byName := map[string]int{}
	for _, f := range firing {
		since, err := time.Parse(time.RFC3339Nano, f.Since)
		if err != nil {
			continue
		}
		if until, ok := ownedUntil(f.Key); ok {
			if until.IsZero() {
				continue
			}
			if until.After(since) {
				since = until
			}
		}
		unowned := now.Sub(since)
		period := int(unowned / grace)
		if period < 1 {
			continue
		}
		u := Unowned{Firing: f, Since: since, For: unowned, Mark: fmt.Sprintf("%s %d #%d", f.Key, since.Unix(), period)}
		i, seen := byName[f.Name()]
		switch {
		case !seen:
			byName[f.Name()] = len(out)
			out = append(out, u)
		case unowned > out[i].For:
			u.More = out[i].More + 1
			out[i] = u
		default:
			out[i].More++
		}
	}
	return out
}

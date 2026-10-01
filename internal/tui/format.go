package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// ellipsis is the mark a truncated string ends with.
const ellipsis = "…"

// dur renders a duration the way the screen always does: 4s, 12m,
// 2h10m, 3d4h. Minutes inside an hour are zero-padded so the shapes
// column up; a negative duration renders as the age of nothing: 0s.
func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	default:
		dy, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", dy)
		}
		return fmt.Sprintf("%dd%dh", dy, h)
	}
}

// age renders the time since t ("3s", "12m", …); a zero t renders "-".
func age(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return dur(now.Sub(t))
}

// mib renders a memory figure: MiB below a GiB, x.y GiB above.
func mib(m int) string {
	if m < 1024 {
		return fmt.Sprintf("%d MiB", m)
	}
	return fmt.Sprintf("%.1f GiB", float64(m)/1024)
}

// clock renders a wall-clock time as local HH:MM; a time that is not
// today's carries +1d when it falls tomorrow and "Sep 25 19:48" when it
// is older, so the body never shows a raw ISO stamp.
func clock(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	l, n := t.Local(), now.Local()
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
	switch d := l.Sub(day); {
	case d >= 0 && d < 24*time.Hour:
		return l.Format("15:04")
	case d >= 24*time.Hour && d < 48*time.Hour:
		return "+1d " + l.Format("15:04")
	default:
		return l.Format("Jan 2 15:04")
	}
}

// stamp renders an absolute local time ("Sep 25 12:37") for the detail
// pane, where the exact moment matters more than the age.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("Jan 2 15:04")
}

// tokens renders a token count abbreviated: 12k, 1.2M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// cost renders a cost in dollars; nil, meaning the model has no price,
// renders as "?".
func cost(c *float64) string {
	if c == nil {
		return "?"
	}
	return fmt.Sprintf("$%.2f", *c)
}

// fit shortens s to w terminal columns, ending with an ellipsis when it
// cut; SGR sequences and wide runes survive the cut. w <= 0 yields "".
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, ellipsis)
}

// fitLeft keeps the right end of s within w columns, for values whose
// tail matters more than their head.
func fitLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	n := ansi.StringWidth(s) - w + ansi.StringWidth(ellipsis)
	return ansi.TruncateLeft(s, n, ellipsis)
}

// pct renders a share (0..1) as an integer percentage.
func pct(f float64) string {
	return fmt.Sprintf("%d%%", int(math.Round(f*100)))
}

// sinceLabel renders an alert's Since: the collector hands it over as
// Alertmanager's RFC 3339 stamp (or, in fixtures, a bare duration), and
// the screen always shows it as an age. Anything unparseable shows as
// it came in — a wrong clock is better shown than hidden.
func sinceLabel(s string, now time.Time) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return dur(now.Sub(t))
	}
	if d, err := time.ParseDuration(s); err == nil {
		return dur(d)
	}
	return s
}

// limitGiB renders a cgroup limit (whole MiB as digits, or "max") in the
// GiB shape a person reads; anything else ("12G", "?") passes through.
func limitGiB(s string) string {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	return mib(n)
}

// bar draws an n-cell usage bar filled to frac.
func bar(frac float64, n int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(n)))
	return strings.Repeat("▰", filled) + strings.Repeat("▱", n-filled)
}

// trimWord shortens s to w columns, cutting at the last word, slash or
// dash where it can, so a truncated name stays readable.
func trimWord(s string, w int) string {
	if ansiWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return fit(s, w)
	}
	cut := strings.TrimRight(fit(s, w-1), " "+ellipsis)
	if i := strings.LastIndexAny(cut, " /-#"); i*2 >= w {
		cut = cut[:i]
	}
	return cut + ellipsis
}

// rpad fills s on the left to w columns, for right-aligned figures.
func rpad(s string, w int) string {
	if n := w - ansiWidth(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

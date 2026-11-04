package lease

import (
	"regexp"
	"strings"
	"time"
)

// Accept compact Go durations and the way a purpose names a round in prose.
var roundDuration = regexp.MustCompile(`(?i)(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[- ]?(?:hours?|minutes?|seconds?|hrs?|mins?|secs?)|(?:h|m|s)(?:(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:h|m|s))*)\b`)
var roundUnits = strings.NewReplacer(
	"hours", "h", "hour", "h", "hrs", "h", "hr", "h",
	"minutes", "m", "minute", "m", "mins", "m", "min", "m",
	"seconds", "s", "second", "s", "secs", "s", "sec", "s",
	"-", "", " ", "",
)

// Round is the first positive duration in the purpose, or the configured round.
func (h Holder) Round(fallback time.Duration) time.Duration {
	for _, span := range roundDuration.FindAllStringIndex(h.Purpose, -1) {
		if span[0] > 0 {
			before := h.Purpose[span[0]-1]
			// Keep signed and embedded tokens whole, including a decimal's
			// leading point, rather than accepting a positive suffix.
			if before == '-' || before == '.' || before == '_' || before >= '0' && before <= '9' || before >= 'a' && before <= 'z' || before >= 'A' && before <= 'Z' {
				continue
			}
		}
		word := h.Purpose[span[0]:span[1]]
		if d, err := time.ParseDuration(roundUnits.Replace(strings.ToLower(word))); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// Overrun reports how far a valid claim time is past its round.
func (h Holder) Overrun(now time.Time, fallback time.Duration) (time.Duration, time.Duration) {
	round := h.Round(fallback)
	if since := h.SinceTime(); !since.IsZero() && round > 0 && now.Sub(since) > round {
		return now.Sub(since) - round, round
	}
	return 0, round
}

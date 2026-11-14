package cmd

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// timerParkWait recognizes the explicit timer wait used by parked workers.
// Anchor it so mentioning an old timer in a person wait cannot settle it.
var timerParkWait = regexp.MustCompile(`(?i)^timer\s+#?([0-9]+)\b`)

// TimerSettlesPark permits a timer for a plain external wait or a typed
// condition on the very reference the park names. Named waits on a note,
// hold, lease or alert belong to their own settlement machinery. A plain
// wait on a person belongs to that person, never a reminder timer.
func TimerSettlesPark(t state.Timer, p *state.Park, person string) bool {
	if p.On == "" {
		if match := timerParkWait.FindStringSubmatch(strings.TrimSpace(p.Waits)); match != nil {
			id, err := strconv.Atoi(match[1])
			return err == nil && id == t.ID
		}
	}
	if ParkOnPerson(p, person) {
		return false
	}
	if p.On == "" {
		return true
	}
	kind, ref, _ := strings.Cut(t.When, " ")
	switch kind {
	case "pr-merged", timerPRChecks, "issue-closed":
		return strings.TrimSpace(ref) == p.On
	}
	// A worker's own one-shot deadline/probe is itself the condition for
	// an external PR/issue wait. The reference is context, not a second
	// condition. Keep unrelated typed conditions and repeating reminders out.
	_, external := parseRef(p.On)
	return external && t.When == "" && t.Repeat == "" && t.By.Is(p.By)
}

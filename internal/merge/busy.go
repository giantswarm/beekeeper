package merge

import (
	"encoding/json"
	"regexp"
	"time"
)

// busyBase is devctl pr merge's refusal of a head whose base moved during
// every one of its --update-branch waits ("… (main moved during the wait)
// after 3 waits; run the merge again once main is quiet"): its exit 3 once
// its own update rounds are spent.
var busyBase = regexp.MustCompile(`moved during the wait\) after \d+ waits; run the merge again once \S+ is quiet`)

// BusyBase says whether devctl's document is that refusal: nothing merged,
// the pull request only behind a base that moves faster than its checks.
func BusyBase(doc []byte) bool {
	var d struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason"`
	}
	return json.Unmarshal(doc, &d) == nil && d.ExitCode == 3 && busyBase.MatchString(d.Reason)
}

// BusyPauses are the pauses before the runs of a busy base's merge after
// the first, one per run, the last repeating: a base that moved during every
// wait gets longer to fall quiet each time, so the next run's one update
// lands on a base that stays put while its checks run.
var BusyPauses = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// BusyNext is the pause of pauses (BusyPauses) before devctl's next run
// after run runs ended on a busy base, the first started at start, and
// whether that run fits the budget: one that would start later than
// start+budget is none, and a budget of zero allows no second run.
func BusyNext(pauses []time.Duration, run int, start, now time.Time, budget time.Duration) (time.Duration, bool) {
	if len(pauses) == 0 {
		return 0, budget > 0 && !now.After(start.Add(budget))
	}
	pause := pauses[min(max(run, 1), len(pauses))-1]
	return pause, budget > 0 && !now.Add(pause).After(start.Add(budget))
}

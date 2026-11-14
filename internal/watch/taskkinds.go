package watch

import (
	"time"
)

// kindSpendEvery is how often the watch sums the day's spend per kind.
const kindSpendEvery = 10 * time.Minute

// KindSpending runs in the command, which holds what it needs (watchKindSpending).
func (w *Watcher) KindSpending(now time.Time) {
	Cmd.WatchKindSpending(w, now)
}

package cmd

import (
	"time"
)

// kindSpendEvery is how often the watch sums the day's spend per kind.
const kindSpendEvery = 10 * time.Minute

// kindSpending runs in the command, which holds what it needs (watchKindSpending).
func (w *watcher) kindSpending(now time.Time) {
	watchKindSpending(w, now)
}

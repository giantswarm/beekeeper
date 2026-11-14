package cmd

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/free"
)

// freeMark is the watches' shared memory of the automatic free: when it ran
// last and what it freed then ("" for nothing).
type freeMark struct {
	At    time.Time `json:"at"`
	Freed string    `json:"freed,omitempty"`
}

// readFreeMark reads when the automatic free ran last and keeps it for the
// TMPFS line.
func (w *watcher) readFreeMark() freeMark {
	var m freeMark
	if found, _ := w.a.store.ReadFile(freeFile, &m); found {
		w.lastFree.Store(&m)
	}
	return m
}

// autoFree runs in the command, which holds what it needs (watchAutoFree).
func (w *watcher) autoFree(now time.Time) {
	watchAutoFree(w, now)
}

// freeDue runs in the command, which holds what it needs (watchFreeDue).
func (w *watcher) freeDue(read func() (free.Machine, error), now time.Time, every time.Duration) {
	watchFreeDue(w, read, now, every)
}

// lastFreeNote says, for the TMPFS line, when the automatic free ran last
// and what it freed.
func (w *watcher) lastFreeNote() string {
	if w.a.cfg.Doctor.FreeEvery.Duration <= 0 {
		return "the automatic free is off (doctor.freeEvery)"
	}
	m := w.lastFree.Load()
	switch {
	case m == nil || m.At.IsZero():
		return "the automatic free has not run yet"
	case m.Freed == "":
		return fmt.Sprintf("the automatic free ran at %s, nothing to free", m.At.Local().Format("15:04"))
	}
	return fmt.Sprintf("the automatic free ran at %s and freed %s", m.At.Local().Format("15:04"), m.Freed)
}

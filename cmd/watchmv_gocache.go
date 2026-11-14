package cmd

import (
	"time"
)

// goCache reads the size of the Go build cache every doctor.goCacheEvery
// and trims it over its cap, in the background so the sample goes on
// (goCacheLook). The goCaching flag orders the looks: the
// next look's time is read only with the flag held, after the look that
// wrote it gave the flag back.
func (w *watcher) goCache(now time.Time) {
	if w.a.cfg.Disk.Auto() || !w.goCacheOn || w.a.cfg.Doctor.GoCacheMax() == 0 || !w.goCaching.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.goCacheNext) {
		w.goCaching.Store(false)
		return
	}
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer w.goCaching.Store(false)
		w.goCacheLook(now, goBuilds)
	}()
}

// goCacheLook runs in the command, which holds what it needs (watchGoCacheLook).
func (w *watcher) goCacheLook(now time.Time, builds func() []int) {
	watchGoCacheLook(w, now, builds)
}

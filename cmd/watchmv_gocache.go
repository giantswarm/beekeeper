package cmd

import (
	"time"
)

// GoCache reads the size of the Go build cache every doctor.goCacheEvery
// and trims it over its cap, in the background so the sample goes on
// (goCacheLook). The goCaching flag orders the looks: the
// next look's time is read only with the flag held, after the look that
// wrote it gave the flag back.
func (w *Watcher) GoCache(now time.Time) {
	if w.Host.Cfg().Disk.Auto() || !w.GoCacheOn || w.Host.Cfg().Doctor.GoCacheMax() == 0 || !w.goCaching.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.GoCacheNext) {
		w.goCaching.Store(false)
		return
	}
	w.Bg.Add(1)
	go func() {
		defer w.Bg.Done()
		defer w.goCaching.Store(false)
		w.GoCacheLook(now, Cmd.GoBuilds)
	}()
}

// GoCacheLook runs in the command, which holds what it needs (watchGoCacheLook).
func (w *Watcher) GoCacheLook(now time.Time, builds func() []int) {
	Cmd.WatchGoCacheLook(w, now, builds)
}

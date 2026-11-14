package cmd

func (w *Watcher) SayAppDrifts(drift map[string]string) {
	active := map[string]bool{}
	for key, line := range drift {
		key = "app-drift " + key
		active[key] = true
		w.Check(key, true, "%s", line)
	}
	w.ClearMissing("app-drift ", active)
}

package cmd

func (w *watcher) sayAppDrifts(drift map[string]string) {
	active := map[string]bool{}
	for key, line := range drift {
		key = "app-drift " + key
		active[key] = true
		w.check(key, true, "%s", line)
	}
	w.clearMissing("app-drift ", active)
}

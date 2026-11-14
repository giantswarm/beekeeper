package watch

// ClaudeLogin runs in the command, which holds what it needs (watchClaudeLogin).
func (w *Watcher) ClaudeLogin() {
	Cmd.WatchClaudeLogin(w)
}

package cmd

import (
	"path/filepath"

	"github.com/giantswarm/beekeeper/internal/guard"
)

func (a *app) transcriptPolicy() guard.TranscriptAccess {
	return guard.TranscriptAccess{
		Roots: []string{a.cfg.Claude.ProjectsDir, a.cfg.Codex.SessionsDir,
			filepath.Join(filepath.Dir(a.cfg.Codex.SessionsDir), "archived_sessions"),
			filepath.Join(filepath.Dir(a.cfg.Codex.SessionsDir), "history.jsonl"),
			filepath.Join(a.cfg.StateDir, "codex", "home", "sessions"),
			filepath.Join(a.cfg.StateDir, "codex", "home", "archived_sessions"),
			filepath.Join(a.cfg.StateDir, "codex", "home", "history.jsonl"),
			a.cfg.Agy.BrainDir, a.cfg.Agy.ConversationsDir, a.cfg.Omp.SessionsDir},
		Grants: a.cfg.Hooks.TranscriptGrants,
	}
}

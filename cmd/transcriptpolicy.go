package cmd

import (
	"os"
	"path/filepath"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

const transcriptHostEnv = "CLAUDE_CODE_HOST_SESSION_ID"

func (a *app) transcriptPolicy() guard.TranscriptAccess {
	return guard.TranscriptAccess{
		Roots: []string{a.cfg.Claude.ProjectsDir, a.cfg.Codex.SessionsDir,
			filepath.Join(filepath.Dir(a.cfg.Codex.SessionsDir), "archived_sessions"),
			filepath.Join(filepath.Dir(a.cfg.Codex.SessionsDir), "history.jsonl"),
			filepath.Join(a.cfg.StateDir, "codex", "home", "sessions"),
			filepath.Join(a.cfg.StateDir, "codex", "home", "archived_sessions"),
			filepath.Join(a.cfg.StateDir, "codex", "home", "history.jsonl"),
			a.cfg.Agy.BrainDir, a.cfg.Agy.ConversationsDir, a.cfg.Omp.SessionsDir},
		ClaudeProjects: []string{a.cfg.Claude.ProjectsDir},
		AgyBrains:      []string{a.cfg.Agy.BrainDir},
		Managed:        a.managedTranscriptSession,
		Grants:         a.cfg.Hooks.TranscriptGrants,
	}
}

// Unlike the general hook scope, roster membership or a cwd under scope.dirs
// does not make a person's session a component-started worker.
func (a *app) managedTranscriptSession(session string) (bool, error) {
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return false, err
	}
	st, err := store.Peek()
	if err != nil {
		return false, err
	}
	parties := []state.Party{{Session: session, HostSession: os.Getenv(transcriptHostEnv)}}
	for _, h := range []struct{ env, prefix string }{{codex.EnvAgent, codex.HostPrefix}, {agy.EnvAgent, agy.HostPrefix}, {omp.EnvAgent, omp.HostPrefix}} {
		if id := os.Getenv(h.env); id != "" {
			parties = append(parties, state.Party{Session: h.prefix + id, HostSession: h.prefix + id})
		}
	}
	for _, p := range parties {
		if st.StartIndex(p) >= 0 {
			return true, nil
		}
		for _, holder := range []*state.Supervisor{st.Supervisor, st.GuideRole().Holder} {
			if holder != nil && (p.Session != "" || p.HostSession != "") && holder.Is(p) {
				return true, nil
			}
		}
	}
	return false, nil
}

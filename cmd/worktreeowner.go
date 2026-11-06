package cmd

import "github.com/giantswarm/beekeeper/internal/worktreeowner"

func (a *app) hookWorktreeOwner(session, dir string, create bool) string {
	return worktreeowner.Check(a.cfg.StateDir, session, dir, create, func(owner string) (bool, error) {
		sessions, _, err := a.sessions()
		if err != nil {
			return false, err
		}
		for _, s := range sessions {
			if (s.ID == owner || s.HostID == owner) && s.State != transcriptStatusEnded {
				return true, nil
			}
		}
		return false, nil
	})
}

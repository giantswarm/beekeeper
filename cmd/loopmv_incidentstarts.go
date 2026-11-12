package cmd

import (
	"fmt"
)

// RepositoryStart rereads immediately before dispatch, so a hold set while
// the board or GitHub was being read cannot launch another worker.
func (l *Loop) RepositoryStart(ref string) error {
	st, err := l.Host.store.Read()
	if err != nil {
		return err
	}
	if why := repositoryStartReason(st, l.Host.timeNow(), ref); why != "" {
		l.SayOnce("incident "+ref, "SUPERVISOR START WAITS %s: %s", ref, why)
		return fmt.Errorf("%s: %w", why, ErrLoopLater)
	}
	l.ClearSaid("incident " + ref)
	return nil
}

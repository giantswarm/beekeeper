package cmd

import (
	"fmt"
)

// repositoryStart rereads immediately before dispatch, so a hold set while
// the board or GitHub was being read cannot launch another worker.
func (l *supervisorLoop) repositoryStart(ref string) error {
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	if why := repositoryStartReason(st, l.a.timeNow(), ref); why != "" {
		l.sayOnce("incident "+ref, "SUPERVISOR START WAITS %s: %s", ref, why)
		return fmt.Errorf("%s: %w", why, errLoopLater)
	}
	l.clearSaid("incident " + ref)
	return nil
}

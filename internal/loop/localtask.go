package loop

import (
	"context"
	"fmt"

	"github.com/giantswarm/beekeeper/internal/state"
)

// localTask says why a start of the task refs name stays on the
// coordinator: the mark a worker or the desk gave one of them (agents move),
// "" for none.
func localTask(st *state.State, refs ...string) string {
	var names []string
	for _, raw := range refs {
		if r, ok := Cmd.ParseRef(raw); ok {
			names = append(names, Cmd.RefName(r))
		}
	}
	t, ok := st.LocalTaskFor(names...)
	if !ok {
		return ""
	}
	return fmt.Sprintf("its task %s needs the coordinator (agents move): %s", t.Ref, t.Why)
}

// localPlacement places a worker start that stays on the coordinator: here
// once its capacity reads room, else later.
func (l *Loop) localPlacement(ctx context.Context, name string) (startHost, error) {
	flags := startFlags(false, false)
	if model := l.Host.RuleModel(name, workerRole); model != "" {
		flags = append(flags, modelFlag, model)
	}
	out, code, err := BeekeeperRun(ctx, l.As(append([]string{LoopCapacity}, flags...)...)...)
	var capacity struct {
		Room int `json:"room"`
	}
	if err != nil || code != 0 || decodeRun(out, &capacity) != nil || capacity.Room <= 0 {
		return startHost{}, ErrLoopLater
	}
	return startHost{}, nil
}

// taskPlacement places a worker start of the task refs name: on the
// coordinator, with here saying why, when one of them is marked to stay
// there (agents move); else as startPlacement places any start.
func (l *Loop) taskPlacement(ctx context.Context, st *state.State, name, repo string, refs ...string) (place startHost, here string, err error) {
	if l.PlacesRemotely() {
		if here = localTask(st, refs...); here != "" {
			place, err = l.localPlacement(ctx, name)
			return place, here, err
		}
	}
	place, err = l.startPlacement(ctx, name, repo, false, false)
	return place, "", err
}

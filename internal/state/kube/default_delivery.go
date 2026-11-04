package kube

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const defaultDeliveredAnnotation = "beekeeper.giantswarm.io/default-delivered"

// PendingDefault is a retained default whose external delivery is unfinished.
type PendingDefault struct {
	Note      state.Note
	ClosureID string
}

// PendingDefaults survives Event failures, partial batches and server restarts.
// The acknowledgement belongs to one closure, so a later answer supersedes it.
func (s *Store) PendingDefaults() ([]PendingDefault, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	snap, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	var out []PendingDefault
	for _, k := range sortedKeys(snap.notes) {
		n := snap.notes[k]
		c := n.Status.Closure
		if c != nil && c.Verb == noteDefaultedEvent && n.Annotations[defaultDeliveredAnnotation] != c.ID {
			out = append(out, PendingDefault{Note: noteOf(n), ClosureID: c.ID})
		}
	}
	return out, nil
}

// DefaultDelivered acknowledges only the closure that was actually delivered.
// A concurrent answer, deletion or acknowledgement leaves nothing to write.
func (s *Store) DefaultDelivered(team string, id int, closureID string) error {
	ctx, cancel := s.ctx()
	defer cancel()
	return s.rerun(ctx, "default delivery", func() (bool, error) {
		n := &v1alpha1.Note{}
		err := s.c.Get(ctx, client.ObjectKeyFromObject(NoteObject(team, id)), n)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		c := n.Status.Closure
		if c == nil || c.Verb != noteDefaultedEvent || c.ID != closureID || n.Annotations[defaultDeliveredAnnotation] == closureID {
			return false, nil
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[defaultDeliveredAnnotation] = closureID
		err = s.c.Update(ctx, n)
		if apierrors.IsConflict(err) {
			return true, nil
		}
		return false, err
	})
}

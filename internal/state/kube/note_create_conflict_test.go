package kube

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// A concurrent request can see the new Note through spec provenance and
// withdraw it before the creator's initial status write reaches the API.
type withdrawBeforeInitialStatus struct {
	client.Client
	withdrawn bool
}

func (c *withdrawBeforeInitialStatus) Status() client.SubResourceWriter {
	return &withdrawBeforeInitialStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type withdrawBeforeInitialStatusWriter struct {
	client.SubResourceWriter
	c *withdrawBeforeInitialStatus
}

func (w *withdrawBeforeInitialStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*v1alpha1.Note); ok && !w.c.withdrawn {
		w.c.withdrawn = true
		if err := New(w.c.Client).Update(func(st *state.State) ([]state.Event, error) {
			st.Notes = nil
			return []state.Event{{By: ana, Verb: noteDoneEvent, Detail: "#1 withdrawn: no longer needed"}}, nil
		}); err != nil {
			return err
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestInitialStatusConflictDoesNotRepeatNoteAdd(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).Build()
	s := New(&withdrawBeforeInitialStatus{Client: c})
	now := time.Now().UTC().Truncate(time.Second)
	runs := 0
	err = s.Update(func(st *state.State) ([]state.Event, error) {
		runs++
		st.NextNote++
		st.Notes = append(st.Notes, state.Note{ID: st.NextNote, By: ana, At: now, Kind: state.NoteDecision, Text: asked})
		return nil, nil
	})
	if !apierrors.IsConflict(err) {
		t.Fatalf("the initial status conflict should be reported after the successful create: %v", err)
	}
	if runs != 1 {
		t.Fatalf("the note add ran %d times after creating a note", runs)
	}
	notes := &v1alpha1.NoteList{}
	if err := c.List(context.Background(), notes); err != nil {
		t.Fatal(err)
	}
	if len(notes.Items) != 1 || notes.Items[0].Spec.ID != 1 || notes.Items[0].Status.State != noteClosed {
		t.Fatalf("the add recreated a withdrawn note: %+v", notes.Items)
	}
	st, err := New(c).Read()
	if err != nil || st.NextNote != 1 || len(st.Notes) != 0 {
		t.Fatalf("the create conflict issued another note number: %+v, %v", st, err)
	}
}

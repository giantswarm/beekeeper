package kube

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func checkNoteRetention(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	store := New(c)
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		for range 4 {
			st.NextNote++
			st.Notes = append(st.Notes, state.Note{ID: st.NextNote, By: ana, At: now, Kind: state.NoteDecision, Text: asked, Due: now.Add(time.Hour), Default: unreleasedDefault})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Highest ID expires; a recent closed note, an open one and a default
	// still in its late-answer window must all survive collection.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		open := []state.Note{}
		evs := []state.Event{}
		for _, n := range st.Notes {
			switch n.ID {
			case 1:
				open = append(open, n)
			case 3:
				st.Defaulted = append(st.Defaulted, n)
				evs = append(evs, state.Event{At: now, By: ana, Verb: noteDefaultedEvent, Detail: fmt.Sprintf("#%d defaulted", n.ID)})
			default:
				at := now.Add(-noteClosureRetention + time.Hour)
				if n.ID == 4 {
					at = now.Add(-noteClosureRetention - time.Hour)
				}
				evs = append(evs, state.Event{At: at, By: ana, Verb: noteDoneEvent, Detail: fmt.Sprintf("#%d withdrawn", n.ID)})
			}
		}
		st.Notes = open
		return evs, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Exercise legacy recovery: neither the counter nor expired Events can
	// supply the high-water mark once the highest retained Note is collected.
	if err := c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: noteCounter}}); err != nil {
		t.Fatal(err)
	}
	events := &corev1.EventList{}
	if err := c.List(ctx, events); err != nil {
		t.Fatal(err)
	}
	for i := range events.Items {
		if err := c.Delete(ctx, &events.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{Target: muster, Reason: "unrelated update during retention cleanup", By: ana, At: now})
		return nil, nil
	}); err != nil {
		t.Fatalf("unrelated update must not become a multi-object refusal: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(NoteObject(team, 4)), &v1alpha1.Note{}); err != nil {
		t.Fatalf("unrelated update collected history: %v", err)
	}
	// A note-only no-op (as the periodic default tick does) collects history.
	if err := store.Update(func(*state.State) ([]state.Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(NoteObject(team, 4)), &v1alpha1.Note{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired Note still exists: %v", err)
	}
	for _, id := range []int{1, 2, 3} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(NoteObject(team, id)), &v1alpha1.Note{}); err != nil {
			t.Fatalf("collected unexpired Note %d: %v", id, err)
		}
	}
	restarted := New(c)
	st, err := restarted.Read()
	if err != nil || st.NextNote != 4 || len(st.Notes) != 1 || len(st.Defaulted) != 1 {
		t.Fatalf("state after collection: %+v %v", st, err)
	}
	if err := restarted.Update(func(st *state.State) ([]state.Event, error) {
		st.NextNote++
		st.Notes = append(st.Notes, state.Note{ID: st.NextNote, By: ana, At: now, Kind: state.NoteDecision, Text: asked})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(NoteObject(team, 5)), &v1alpha1.Note{}); err != nil {
		t.Fatalf("next note did not advance past expired highest ID: %v", err)
	}
}

func TestNoteClosureRetention(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}, &v1alpha1.Hold{}).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultNamespace}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}).Build()
	checkNoteRetention(t, c)
}

func TestEnvtestNoteClosureRetention(t *testing.T) {
	_, _, c := envtestStore(t)
	namespace(t, c, team)
	checkNoteRetention(t, c)
}

// collectedNoteClient simulates another collector winning after our read:
// the delete has happened, but our request sees NotFound and must rerun.
type collectedNoteClient struct{ client.Client }

func (c collectedNoteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	err := c.Client.Delete(ctx, obj, opts...)
	if _, note := obj.(*v1alpha1.Note); note && err == nil {
		return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("notes").GroupResource(), obj.GetName())
	}
	return err
}

func TestNoteClosureRetentionLostRace(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}, &v1alpha1.Hold{}).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultNamespace}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}).Build()
	checkNoteRetention(t, collectedNoteClient{c})
}

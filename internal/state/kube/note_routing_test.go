package kube

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func TestNoteEventsWaitForTheirNote(t *testing.T) {
	first := &v1alpha1.Note{Spec: v1alpha1.NoteSpec{ID: 1}}
	second := &v1alpha1.Note{Spec: v1alpha1.NoteSpec{ID: 2}}
	for _, e := range []state.Event{
		{Verb: noteAnsweredEvent, Detail: `acme/app#1 answered by "bo" via cli, note #2 asked by "ana": "ship?": yes`},
		{Verb: "plan.ready", Detail: "acme/plans#1: note #2 asks bo for the review"},
		{Verb: "plan.grilling", Detail: "acme/plans#1: note #2 carries its questions for bo"},
	} {
		t.Run(e.Verb, func(t *testing.T) {
			if got := on([]client.Object{first}, e, false); got != nil {
				t.Fatalf("an event whose note has not been written attached to note #1: %v", got)
			}
			if got := on([]client.Object{first, second}, e, true); got != second {
				t.Fatalf("the PR number was mistaken for the event's note: %v", got)
			}
		})
	}
}

// An answer may collect an older closed Note in the same update. The
// answer's Event must reuse the answered Note's closure ID and subject.
func TestAnswerEventRoutesPastCollectedNote(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-1"},
		Spec: v1alpha1.NoteSpec{ID: 1, Text: "old question"},
		Status: v1alpha1.NoteStatus{State: noteClosed, By: apiParty(ana), At: metav1.NewTime(now.Add(-noteClosureRetention - time.Hour)),
			Closure: &v1alpha1.NoteClosure{ID: "0000000000000000001", Verb: noteDoneEvent, Detail: "#1 withdrawn", At: metav1.NewTime(now.Add(-noteClosureRetention - time.Hour))}}}
	open := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-2"},
		Spec:   v1alpha1.NoteSpec{ID: 2, Text: asked, Kind: state.NoteDecision},
		Status: v1alpha1.NoteStatus{State: noteOpen, By: apiParty(ana), At: metav1.NewTime(now)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(old, open).Build()
	s := New(c)
	if err := s.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool { return n.ID == 2 })
		return []state.Event{{At: now, By: bo, Verb: noteAnsweredEvent, Detail: `acme/app#1 answered by "bo" via cli, note #2 asked by "ana": "ship?": yes`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	events := &corev1.EventList{}
	if err := c.List(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 || events.Items[0].InvolvedObject.Name != "note-2" {
		t.Fatalf("answer Event on the wrong Note: %+v", events.Items)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(open), open); err != nil {
		t.Fatal(err)
	}
	if open.Status.Closure == nil || eventID(&events.Items[0]) != open.Status.Closure.ID {
		t.Fatalf("answer Event and durable closure have different IDs: %+v, %+v", events.Items[0], open.Status.Closure)
	}
	feed, err := s.Feed(0)
	if err != nil || len(feed) != 1 || feed[0].Subject != "Note/note-2" {
		t.Fatalf("answer appears more than once in the feed: %+v, %v", feed, err)
	}
}

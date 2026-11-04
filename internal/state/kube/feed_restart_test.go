package kube

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// The closure remains in the feed even if its Event failed or expired. A
// restarted writer must continue above that ID, including for a dated event.
func TestFeedIDContinuesFromRetainedClosure(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	last := feed.ID(now.Add(time.Millisecond).UnixNano())
	note := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-1"},
		Spec: v1alpha1.NoteSpec{ID: 1, Text: asked},
		Status: v1alpha1.NoteStatus{State: noteClosed, By: apiParty(ana), At: metav1.NewTime(now),
			Closure: &v1alpha1.NoteClosure{ID: last, Verb: noteDoneEvent, Detail: "#1 withdrawn", At: metav1.NewTime(now), By: apiParty(ana)}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).
		WithObjects(note, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}).Build()
	s := New(c)
	// Claim events are routinely dated at the start of the current second;
	// no clock change is needed to fall below a retained closure's ID.
	if err := s.Audit(nil, team, state.Event{At: now, By: ana, Verb: "test.restart", Detail: "after the Event is gone"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Feed(0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("feed: %+v, %v", evs, err)
	}
	if newest := evs[len(evs)-1]; newest.Kind != "test.restart" || newest.ID <= last {
		t.Fatalf("a cursor at %s would miss the new event: %+v", last, evs)
	}
}

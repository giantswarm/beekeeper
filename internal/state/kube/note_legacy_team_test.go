package kube

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func TestLegacySameNumberDefaultsKeepTheirOwnClosures(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	teams := []string{ana.Team, pia.Team}
	var objects []client.Object
	for _, team := range teams {
		objects = append(objects, &v1alpha1.Note{
			ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-3"},
			Spec:       v1alpha1.NoteSpec{ID: 3, Kind: state.NoteDecision, Text: team + " question", Default: team + " default", Due: apiTime(now.Add(-time.Hour))},
			Status:     v1alpha1.NoteStatus{State: noteOpen, By: v1alpha1.Party{Name: "filer", Team: team}, At: metav1.NewTime(now.Add(-2 * time.Hour))},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(objects...).Build()
	s := New(c)
	if err := s.Update(func(st *state.State) ([]state.Event, error) {
		var events []state.Event
		for _, n := range st.Notes {
			st.Defaulted = append(st.Defaulted, n)
			events = append(events, state.Event{NoteTeam: n.By.Team, Verb: noteDefaultedEvent, At: now, Detail: fmt.Sprintf("#3 defaulted: %s", n.Default)})
		}
		st.Notes = nil
		return events, nil
	}); err != nil {
		t.Fatal(err)
	}
	events := &corev1.EventList{}
	if err := c.List(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != len(teams) {
		t.Fatalf("events: %+v", events.Items)
	}
	for _, team := range teams {
		n := &v1alpha1.Note{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(NoteObject(team, 3)), n); err != nil {
			t.Fatal(err)
		}
		want := "#3 defaulted: " + team + " default"
		if n.Status.Closure == nil || n.Status.Closure.Detail != want {
			t.Fatalf("%s got another team's closure: %+v", team, n.Status.Closure)
		}
		found := false
		for _, e := range events.Items {
			if e.InvolvedObject.Namespace == n.Namespace {
				found = true
				if eventDetail(&e) != want || eventID(&e) != n.Status.Closure.ID {
					t.Fatalf("%s got another team's Event: %+v", team, e)
				}
			}
		}
		if !found {
			t.Fatalf("no Event attached to %s", n.Namespace)
		}
	}
	feed, err := s.Feed(0)
	if err != nil || len(feed) != len(teams) {
		t.Fatalf("duplicate/missing defaults in feed: %+v, %v", feed, err)
	}
}

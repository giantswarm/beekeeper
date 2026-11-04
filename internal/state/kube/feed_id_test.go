package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

type failingFeedListClient struct {
	client.Client
	failNotes bool
	fail      bool
}

var errFeedList = errors.New("feed list unavailable")

func (c *failingFeedListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	_, notes := list.(*v1alpha1.NoteList)
	if c.fail && notes == c.failNotes {
		c.fail = false
		return errFeedList
	}
	return c.Client.List(ctx, list, opts...)
}

func TestFeedIDRetriesInitialization(t *testing.T) {
	for _, failNotes := range []bool{false, true} {
		name := "events"
		if failNotes {
			name = "notes"
		}
		t.Run(name, func(t *testing.T) {
			scheme, err := Scheme()
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			note := NoteObject(team, 1).(*v1alpha1.Note)
			note.Status.Closure = &v1alpha1.NoteClosure{ID: feed.ID(at.UnixNano() + 2)}
			event := &corev1.Event{}
			event.Name = "existing"
			event.Labels = EventLabels()
			event.Annotations = map[string]string{idAnnotation: feed.ID(at.UnixNano() + 3)}
			c := &failingFeedListClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(note, event).Build(), failNotes: failNotes, fail: true}
			s := New(c)
			if _, err := s.nextID(context.Background(), at); !errors.Is(err, errFeedList) {
				t.Fatalf("first allocation: %v, want list failure", err)
			}
			if s.idsRead {
				t.Fatal("initialization completed despite list failure")
			}
			id, err := s.nextID(context.Background(), at)
			if err != nil || id != feed.ID(at.UnixNano()+4) {
				t.Fatalf("retry allocation: %s, %v; want ID above both retained sources", id, err)
			}
		})
	}
}

func TestFeedIDContinuesFromRetainedClosure(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	closureID := feed.ID(at.Add(500 * time.Millisecond).UnixNano())
	note := NoteObject(team, 1).(*v1alpha1.Note)
	note.Status.Closure = &v1alpha1.NoteClosure{ID: closureID, Verb: noteDoneEvent, At: *apiTime(at)}
	// Only the retained closure survives the previous writer; its core Event
	// has expired. Claim events are normally dated at the second boundary.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(note).Build()
	s := New(c)
	if err := s.recordOn(context.Background(), note, state.Event{At: at, Verb: "note.add", Detail: "#2 added"}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("feed has %d events, want retained closure and new event", len(events))
	}
	if events[0].ID != closureID || events[1].Kind != "note.add" || events[1].ID <= closureID {
		t.Fatalf("new event must remain readable after closure cursor %s: %+v", closureID, events)
	}
}

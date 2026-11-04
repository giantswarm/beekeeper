package kube

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

type delayedPublication struct {
	client.Client
	closure bool
	entered chan struct{}
	release chan struct{}
	paused  atomic.Bool
}

func (c *delayedPublication) pause() {
	if c.paused.CompareAndSwap(false, true) {
		close(c.entered)
		<-c.release
	}
}

func (c *delayedPublication) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Event); ok && !c.closure {
		c.pause()
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *delayedPublication) Status() client.SubResourceWriter {
	return delayedPublicationStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type delayedPublicationStatus struct {
	client.SubResourceWriter
	c *delayedPublication
}

func (w delayedPublicationStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if n, ok := obj.(*v1alpha1.Note); ok && n.Status.Closure != nil && w.c.closure {
		w.c.pause()
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestFeedPublicationKeepsDelayedWritesBeforeLaterEvents(t *testing.T) {
	for _, closure := range []bool{false, true} {
		name := "event"
		if closure {
			name = "closure"
		}
		t.Run(name, func(t *testing.T) {
			scheme, err := Scheme()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			note := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-1"},
				Spec: v1alpha1.NoteSpec{ID: 1, Text: asked}, Status: v1alpha1.NoteStatus{State: noteOpen, By: apiParty(ana), At: metav1.NewTime(now)}}
			c := &delayedPublication{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(note).
				WithObjects(note, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}).Build(),
				closure: closure, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(c.release) }) }
			t.Cleanup(unblock)
			s := New(c)
			first := make(chan error, 1)
			go func() {
				if closure {
					first <- s.Update(func(st *state.State) ([]state.Event, error) {
						st.Notes = nil
						return []state.Event{{At: now, By: ana, Verb: noteDoneEvent, Detail: "#1 withdrawn"}}, nil
					})
				} else {
					first <- s.Audit(nil, team, state.Event{At: now, By: ana, Verb: "test.first"})
				}
			}()
			select {
			case <-c.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("first publication never reached its write")
			}
			second := make(chan error, 1)
			go func() { second <- s.Audit(nil, team, state.Event{At: now, By: ana, Verb: "test.second"}) }()
			// A cursor must not advance to the later call while the earlier
			// Event or durable closure is still waiting on the API.
			select {
			case err := <-second:
				t.Fatalf("later event overtook the pending %s: %v", name, err)
			case <-time.After(50 * time.Millisecond):
			}
			read := make(chan error, 1)
			go func() {
				_, err := s.Feed(0)
				read <- err
			}()
			select {
			case err := <-read:
				t.Fatalf("feed read passed the pending %s: %v", name, err)
			case <-time.After(50 * time.Millisecond):
			}
			unblock()
			for _, done := range []chan error{first, second, read} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("publication did not finish")
				}
			}
			feed, err := s.Feed(0)
			if err != nil || len(feed) != 2 || feed[1].Kind != "test.second" || feed[0].ID >= feed[1].ID {
				t.Fatalf("cursor order lost the first write: %+v, %v", feed, err)
			}
			retained, reason, err := s.LookupNote(1)
			if err != nil || retained == nil || retained.Text != asked {
				t.Fatalf("publication lost the note: %+v, %v", retained, err)
			}
			if closure && (reason == nil || reason.Verb != noteDoneEvent) {
				t.Fatalf("publication lost the close reason: %+v", reason)
			}
		})
	}
}

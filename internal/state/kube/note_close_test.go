package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const firstDefaulted = "#1 defaulted"

var errCloseEvent = errors.New("event unavailable")

type failCloseEvent struct{ client.Client }

func (c failCloseEvent) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Event); ok {
		return errCloseEvent
	}
	return c.Client.Create(ctx, obj, opts...)
}

func checkNoteClose(t *testing.T, c client.Client) {
	t.Helper()
	store := New(c)
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes, state.Note{ID: 1, By: ana, At: now, Kind: state.NoteDecision, Text: asked, Due: now.Add(-time.Hour), Default: "Keep the release unpublished"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// A caller cannot erase a note without an explicit close event.
	err := store.Update(func(st *state.State) ([]state.Event, error) { st.Notes = nil; return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "without a recorded reason") {
		t.Fatalf("silent close: %v", err)
	}
	if st, _ := store.Read(); len(st.Notes) != 1 {
		t.Fatal("silent close removed note")
	}
	// A default closes without an answer; even a failed Event write cannot
	// erase the reason, and a restarted reader returns the durable audit line.
	broken := New(failCloseEvent{c})
	err = broken.Update(func(st *state.State) ([]state.Event, error) {
		st.Defaulted, st.Notes = st.Notes, nil
		return []state.Event{{At: now, By: ana, Verb: noteDefaultedEvent, Detail: "#1 defaulted: Keep the release unpublished"}}, nil
	})
	if !errors.Is(err, errCloseEvent) {
		t.Fatalf("event failure: %v", err)
	}
	store = New(c)
	evs, err := store.Events(0, nil)
	if err != nil || len(evs) != 1 || evs[0].Verb != noteDefaultedEvent {
		t.Fatalf("durable log: %+v %v", evs, err)
	}
	feed, err := store.Feed(0)
	if err != nil || len(feed) != 1 || feed[0].Kind != noteDefaultedEvent {
		t.Fatalf("feed after failed Event: %+v %v", feed, err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) { st.Defaulted = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read()
	if err != nil || len(st.Notes) != 0 || len(st.Defaulted) != 0 {
		t.Fatalf("expired state: %+v %v", st, err)
	}
	closed, err := store.NoteClosure(1)
	if err != nil || closed == nil || closed.Verb != noteDefaultedEvent {
		t.Fatalf("closure after expiry: %+v %v", closed, err)
	}
	raw := &v1alpha1.Note{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(NoteObject(team, 1)), raw); err != nil || raw.Status.State != noteClosed || raw.Status.Closure == nil {
		t.Fatalf("retained note: %+v %v", raw, err)
	}
	// Unrelated updates must preserve the retained close record.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{Target: "github", Reason: "test", By: ana, At: now})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if closed, err = store.NoteClosure(1); err != nil || closed == nil {
		t.Fatalf("lost closure: %+v %v", closed, err)
	}
}

func TestNoteCloseAudit(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}, &v1alpha1.Hold{}).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultNamespace}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}).Build()
	checkNoteClose(t, c)
}
func TestEnvtestNoteCloseAudit(t *testing.T) {
	_, _, c := envtestStore(t)
	namespace(t, c, team)
	checkNoteClose(t, c)
}

func TestNoteCloseReasons(t *testing.T) {
	for _, tc := range []struct{ verb, detail string }{
		{noteDoneEvent, "#1 withdrawn: no longer needed"},
		{"note.overtaken", "#1 overtaken: settled"},
		{noteAnsweredEvent, `acme/app#999 answered by "bo" via cli, note #1 asked by "ana": "ship?": yes`},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			scheme, err := Scheme()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultNamespace}}).Build()
			s := New(c)
			if err := s.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = append(st.Notes, state.Note{ID: 1, By: ana, At: now, Text: asked, Kind: state.NoteDecision})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			// An answer may come from a different team; the issue's number in
			// the event is not the number of the note being answered.
			if err := s.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = nil
				return []state.Event{{At: now, By: pia, Verb: tc.verb, Detail: tc.detail}}, nil
			}); err != nil {
				t.Fatal(err)
			}
			evs, err := New(c).Events(0, nil)
			if err != nil || len(evs) != 1 || evs[0].Verb != tc.verb {
				t.Fatalf("close audit: %+v %v", evs, err)
			}
			before, err := s.Feed(0)
			if err != nil || len(before) != 1 {
				t.Fatalf("feed before expiry: %+v %v", before, err)
			}
			// Simulate the API server's Event TTL, then restart the reader.
			list := &corev1.EventList{}
			if err := c.List(context.Background(), list); err != nil {
				t.Fatal(err)
			}
			for i := range list.Items {
				if err := c.Delete(context.Background(), &list.Items[i]); err != nil {
					t.Fatal(err)
				}
			}
			after, err := New(c).Feed(0)
			if err != nil || len(after) != 1 || after[0].ID != before[0].ID || after[0].Line != before[0].Line {
				t.Fatalf("feed after expiry: %+v %v", after, err)
			}
			st, err := s.Read()
			if err != nil || len(st.Notes) != 0 {
				t.Fatalf("closed note listed: %+v %v", st, err)
			}
		})
	}
}

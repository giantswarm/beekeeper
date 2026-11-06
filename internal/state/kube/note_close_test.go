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

const unreleasedDefault = "Keep the release unpublished"

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
		st.Notes = append(st.Notes, state.Note{ID: 1, By: ana, At: now, Kind: state.NoteDecision, Text: asked, Due: now.Add(-time.Hour), Default: unreleasedDefault})
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
	note, closure, err := store.LookupNote(1)
	if err != nil || note == nil || note.Text != asked || closure == nil || closure.Verb != noteDefaultedEvent {
		t.Fatalf("full retained note: %+v %+v %v", note, closure, err)
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
	for _, tc := range []struct{ verb, detail, answer string }{
		{noteDoneEvent, "#1 withdrawn: no longer needed", ""},
		{"note.overtaken", "#1 overtaken: settled", ""},
		{noteAnsweredEvent, `acme/app#999 answered by "display name" via cli, note #1 asked by "ana": "ship?": yes`, "yes"},
		{noteAnsweredEvent, "acme/app#999 answered by \"display name\" via slack, note #1 asked by \"ana\" for \"team:example\": \"ship: \\\"now\\\"?\": first: yes\nthen ship", "first: yes\nthen ship"},
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
			if tc.verb == noteAnsweredEvent {
				raw := &v1alpha1.Note{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(NoteObject(team, 1)), raw); err != nil {
					t.Fatal(err)
				}
				if raw.Status.State != noteAnswered || raw.Status.Answer != tc.answer || raw.Status.AnsweredBy != pia.Person || raw.Status.AnsweredAt == nil || !raw.Status.AnsweredAt.Time.Equal(now) {
					t.Fatalf("retained answer status: %+v", raw.Status)
				}
			}
			st, err := s.Read()
			if err != nil || len(st.Notes) != 0 {
				t.Fatalf("closed note listed: %+v %v", st, err)
			}
		})
	}
}

// The filer is not the actor of a legacy default. Its defaulted state proves
// the outcome, but cannot reconstruct who applied it before close auditing.
func TestLegacyDefaultCloseReason(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	n := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: NoteObject(team, 1).GetName()}, Spec: v1alpha1.NoteSpec{ID: 1, Due: apiTime(now.Add(-48 * time.Hour)), Default: unreleasedDefault}, Status: v1alpha1.NoteStatus{State: noteDefaulted, By: apiParty(ana), At: metav1.NewTime(now)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(n).Build()
	s := New(c)
	if err := s.Update(func(st *state.State) ([]state.Event, error) { st.Defaulted = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	closed, err := s.NoteClosure(1)
	if err != nil || closed == nil || closed.Verb != noteDefaultedEvent || closed.By.Name != "" || closed.By.Person != "" {
		t.Fatalf("legacy close attribution: %+v %v", closed, err)
	}
}

func TestLegacySameNumberDefaultsKeepTheirOwnClosures(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	teams := []string{"alpha", "beta"}
	objects := make([]client.Object, 0, len(teams))
	for _, team := range teams {
		n := NoteObject(team, 3).(*v1alpha1.Note)
		n.Spec = v1alpha1.NoteSpec{ID: 3, Kind: state.NoteDecision, Due: apiTime(now.Add(-time.Hour)), Default: "wait for " + team}
		n.Status = v1alpha1.NoteStatus{State: noteOpen, By: apiParty(state.Party{Team: team}), At: metav1.NewTime(now)}
		objects = append(objects, n)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(objects...).Build()
	s := New(c)
	actor := state.Party{Name: "timer", Team: "operations"}
	err = s.Update(func(st *state.State) ([]state.Event, error) {
		var events []state.Event
		for _, n := range st.Notes {
			events = append(events, state.Event{At: now, By: actor, NoteTeam: n.By.Team, Verb: noteDefaultedEvent, Detail: "#3 defaulted: " + n.Default})
		}
		st.Defaulted, st.Notes = st.Notes, nil
		return events, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	list := &corev1.EventList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("events: %+v", list.Items)
	}
	ids := map[string]bool{}
	for _, team := range teams {
		n := &v1alpha1.Note{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(NoteObject(team, 3)), n); err != nil {
			t.Fatal(err)
		}
		closure := n.Status.Closure
		if closure == nil || closure.Detail != "#3 defaulted: wait for "+team || closure.By.Team != actor.Team || closure.By.Name != actor.Name {
			t.Fatalf("closure for %s: %+v", team, closure)
		}
		if ids[closure.ID] {
			t.Fatal("duplicate closure ID")
		}
		ids[closure.ID] = true
		matches := 0
		for _, e := range list.Items {
			if e.InvolvedObject.Namespace == n.Namespace {
				matches++
				if e.InvolvedObject.Name != n.Name || e.Message != closure.Detail || e.Annotations[idAnnotation] != closure.ID {
					t.Fatalf("event for %s: %+v", team, e)
				}
			}
		}
		if matches != 1 {
			t.Fatalf("events for %s: %d", team, matches)
		}
	}
	entries, err := New(c).Feed(0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("feed: %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if !ids[entry.ID] {
			t.Fatalf("unexpected feed ID: %s", entry.ID)
		}
		delete(ids, entry.ID)
	}
	if len(ids) != 0 {
		t.Fatal("missing closures in feed")
	}
}

func TestQualifiedNoteEventWaitsForItsTeam(t *testing.T) {
	e := state.Event{NoteTeam: "beta", Detail: "#3 defaulted"}
	written := []client.Object{NoteObject("alpha", 3)}
	for _, all := range []bool{false, true} {
		if got := on(written, e, all); got != nil {
			t.Fatalf("routed to another team: %+v", got)
		}
	}
}

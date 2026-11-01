package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

var errInitialStatus = errors.New("initial status unavailable")

type failNoteStatus struct{ client.Client }
type failNoteStatusWriter struct{ client.SubResourceWriter }

func (c failNoteStatus) Status() client.SubResourceWriter {
	return failNoteStatusWriter{c.Client.Status()}
}

func (w failNoteStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*v1alpha1.Note); ok {
		return errInitialStatus
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func checkNoteRecovery(t *testing.T, c client.Client) {
	t.Helper()
	broken := New(failNoteStatus{c})
	now := time.Now().Truncate(time.Second)
	n := state.Note{ID: 1, By: ana, At: now, Kind: state.NoteDecision, For: "bo", Text: asked, Due: now.Add(-time.Minute), Default: "Keep the release unpublished"}
	err := broken.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes, n)
		return nil, nil
	})
	if !errors.Is(err, errInitialStatus) {
		t.Fatalf("create: %v", err)
	}
	raw := &v1alpha1.Note{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(NoteObject(team, 1)), raw); err != nil {
		t.Fatal(err)
	}
	if raw.Status.State != "" || raw.Spec.Filer == nil || raw.Spec.FiledAt == nil {
		t.Fatalf("partial create: %+v", raw)
	}
	store := New(c)
	st, err := store.Read()
	if err != nil || len(st.Notes) != 1 || !sameRecoveredNote(st.Notes[0], n) {
		t.Fatalf("recovered %+v: %v", st, err)
	}
	// An unrelated object's update must not try to repair the pending Note
	// as well, or contend with its creator's status write.
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{Target: "github", Reason: "test", By: ana, At: now})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Defaulted = append(st.Defaulted, st.Notes...)
		st.Notes = nil
		return []state.Event{{By: ana, Verb: noteDefaultedEvent, Detail: firstDefaulted}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read()
	if err != nil || len(st.Notes) != 0 || len(st.Defaulted) != 1 || !sameRecoveredNote(st.Defaulted[0], n) {
		t.Fatalf("defaulted %+v: %v", st, err)
	}
}

// metav1 decodes timestamps in the process timezone; compare their instants.
func sameRecoveredNote(got, want state.Note) bool {
	got.At, want.At = got.At.UTC(), want.At.UTC()
	got.Due, want.Due = got.Due.UTC(), want.Due.UTC()
	return equal(got, want)
}

func TestNoteFailedInitialStatus(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	filer := apiParty(ana)
	n := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-1"}, Spec: v1alpha1.NoteSpec{ID: 1, Text: asked, Filer: &filer, FiledAt: apiTime(now)}}
	snap := &snapshot{notes: map[string]*v1alpha1.Note{key(n): n}}
	st := snap.state()
	if len(st.Notes) != 1 || !st.Notes[0].By.Is(ana) || !st.Notes[0].At.Equal(now) {
		t.Fatalf("partial create recovery: %+v", st.Notes)
	}
	n.Status = v1alpha1.NoteStatus{State: noteOpen, By: apiParty(bo), At: metav1.NewTime(now.Add(time.Second))}
	got := noteOf(n)
	if !got.By.Is(bo) || !got.At.Equal(n.Status.At.Time) {
		t.Fatalf("initialized status must win: %+v", got)
	}
}

func TestEnvtestNoteFailedInitialStatus(t *testing.T) {
	_, _, c := envtestStore(t)
	namespace(t, c, team)
	checkNoteRecovery(t, c)
}

func TestLegacyNoteWithoutStatus(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	n := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-1", CreationTimestamp: metav1.NewTime(now)}, Spec: v1alpha1.NoteSpec{ID: 1, Text: asked}}
	snap := &snapshot{notes: map[string]*v1alpha1.Note{key(n): n}}
	st := snap.state()
	if len(st.Notes) != 1 || st.Notes[0].By.Team != team || st.Notes[0].By.Person != "" || !st.Notes[0].At.Equal(now) {
		t.Fatalf("legacy recovery: %+v", st.Notes)
	}
}

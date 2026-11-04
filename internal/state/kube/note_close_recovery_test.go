package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// A note whose create succeeded without its status can be answered or
// withdrawn directly, retaining the filer recovered while it was open.
func TestNoteCloseRecoversFiler(t *testing.T) {
	for _, tc := range []struct {
		name, verb, detail string
		legacy             bool
	}{
		{name: "withdrawn", verb: noteDoneEvent, detail: "#1 withdrawn: no longer needed"},
		{name: "answered", verb: noteAnsweredEvent, detail: `acme/app#999 answered by "bo" via cli, note #1 asked by "ana": "ship?": yes`},
		{name: "legacy", verb: noteDoneEvent, detail: "#1 withdrawn: no longer needed", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme, err := Scheme()
			if err != nil {
				t.Fatal(err)
			}
			filedAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
			filer := apiParty(ana)
			raw := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: "note-1", CreationTimestamp: metav1.NewTime(filedAt)},
				Spec: v1alpha1.NoteSpec{ID: 1, Text: asked, Kind: state.NoteDecision, Filer: &filer, FiledAt: apiTime(filedAt)}}
			if tc.legacy {
				raw.Spec.Filer, raw.Spec.FiledAt = nil, nil
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(raw).Build()
			s := New(c)
			before, err := s.Read()
			if err != nil || len(before.Notes) != 1 {
				t.Fatalf("unfinished create: %+v, %v", before, err)
			}
			if err := s.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = nil
				return []state.Event{{At: filedAt.Add(time.Minute), By: bo, Verb: tc.verb, Detail: tc.detail}}, nil
			}); err != nil {
				t.Fatal(err)
			}
			n, closure, err := New(c).LookupNote(1)
			if err != nil || n == nil || !sameRecoveredNote(*n, before.Notes[0]) {
				t.Fatalf("retained note lost its recovered filer or filing time: %+v, %v", n, err)
			}
			if closure == nil || closure.Verb != tc.verb || !closure.By.Is(bo) {
				t.Fatalf("close actor and filer must both survive: %+v", closure)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(raw), raw); err != nil {
				t.Fatal(err)
			}
			if raw.Status.By.Team != team || !raw.Status.At.Time.Equal(filedAt) {
				t.Fatalf("recovered provenance was not persisted: %+v", raw.Status)
			}
		})
	}
}

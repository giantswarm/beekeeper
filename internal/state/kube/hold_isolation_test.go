package kube

import (
	"context"
	"fmt"
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

// Expiry must not write an unchanged Hold beside a note, nor stop the
// one-at-a-time cleanup used by serve's hold tools.
func TestExpiredHoldsDoNotBlockOtherUpdates(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			scheme, err := Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&v1alpha1.Hold{}, &v1alpha1.Note{}).
				WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: DefaultNamespace}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: teamNamespace(team)}}).Build()
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			var holds []*v1alpha1.Hold
			for i := range count {
				target := fmt.Sprintf("acme/repo-%d", i)
				h := &v1alpha1.Hold{ObjectMeta: metav1.ObjectMeta{Name: holdName(target)},
					Spec: v1alpha1.HoldSpec{Target: target, Reason: "expired", Until: apiTime(now.Add(-time.Hour))}}
				if err := c.Create(ctx, h); err != nil {
					t.Fatal(err)
				}
				// The status last written while the hold was still active.
				h.Status = v1alpha1.HoldStatus{By: apiParty(ana), At: metav1.NewTime(now.Add(-2 * time.Hour)), Active: true}
				if err := c.Status().Update(ctx, h); err != nil {
					t.Fatal(err)
				}
				holds = append(holds, h.DeepCopy())
			}
			s := New(c)
			if err := s.Update(func(st *state.State) ([]state.Event, error) {
				st.NextNote++
				st.Notes = append(st.Notes, state.Note{ID: st.NextNote, By: ana, At: now, Text: asked, Kind: state.NoteDecision})
				return nil, nil
			}); err != nil {
				t.Fatalf("filing a note beside expired holds: %v", err)
			}
			for _, before := range holds {
				after := &v1alpha1.Hold{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(before), after); err != nil {
					t.Fatal(err)
				}
				if after.ResourceVersion != before.ResourceVersion || !equal(after.Status, before.Status) {
					t.Fatalf("the note update changed %s: %+v", before.Name, after.Status)
				}
			}
			for _, h := range holds {
				if err := s.Update(func(st *state.State) ([]state.Event, error) {
					st.Holds = slices.DeleteFunc(st.Holds, func(x state.Hold) bool { return x.Target == h.Spec.Target && x.Expired(now) })
					return nil, nil
				}); err != nil {
					t.Fatalf("pruning %s beside other expired holds: %v", h.Name, err)
				}
			}
			st, err := s.Read()
			if err != nil || len(st.Holds) != 0 || len(st.Notes) != 1 {
				t.Fatalf("after pruning: %+v, %v", st, err)
			}
		})
	}
}

package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func TestDefaultDeliveryAcknowledgesOnlyItsClosure(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	n := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: NoteObject(team, 1).GetName()},
		Spec: v1alpha1.NoteSpec{ID: 1, Text: asked}, Status: v1alpha1.NoteStatus{State: noteDefaulted, By: apiParty(ana), At: metav1.NewTime(now),
			Closure: &v1alpha1.NoteClosure{ID: "1", Verb: noteDefaultedEvent, Detail: firstDefaulted, At: metav1.NewTime(now)}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(n).WithObjects(n).Build()
	pending, err := New(c).PendingDefaults(now)
	if err != nil || len(pending) != 1 || pending[0].ClosureID != "1" || !pending[0].Note.By.Is(ana) {
		t.Fatalf("pending: %+v, %v", pending, err)
	}
	if err := New(c).DefaultDelivered(team, 1, "other-closure"); err != nil {
		t.Fatal(err)
	}
	pending, err = New(c).PendingDefaults(now)
	if err != nil || len(pending) != 1 {
		t.Fatalf("a stale acknowledgement erased delivery: %+v, %v", pending, err)
	}
	if err := New(c).DefaultDelivered(team, 1, "1"); err != nil {
		t.Fatal(err)
	}
	pending, err = New(c).PendingDefaults(now)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledgement did not survive restart: %+v, %v", pending, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}
	if n.Status.Closure == nil || n.Status.Closure.ID != "1" || n.Status.State != noteDefaulted {
		t.Fatalf("acknowledgement damaged the status: %+v", n.Status)
	}
	// A later answer supersedes the default, even if its delivery was unacked.
	n.Status.State, n.Status.Closure.ID, n.Status.Closure.Verb = noteAnswered, "2", noteAnsweredEvent
	if err := c.Status().Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	pending, err = New(c).PendingDefaults(now)
	if err != nil || len(pending) != 0 {
		t.Fatalf("answered note returned as a pending default: %+v, %v", pending, err)
	}
}

// A rollout must not resend the defaults that closed long before it.
func TestDefaultDeliverySkipsClosuresPastItsWindow(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	n := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Namespace: teamNamespace(team), Name: NoteObject(team, 1).GetName()},
		Spec: v1alpha1.NoteSpec{ID: 1, Text: asked}, Status: v1alpha1.NoteStatus{State: noteDefaulted, By: apiParty(ana), At: metav1.NewTime(now.Add(-48 * time.Hour)),
			Closure: &v1alpha1.NoteClosure{ID: "1", Verb: noteDefaultedEvent, Detail: firstDefaulted, At: metav1.NewTime(now.Add(-2 * defaultDeliveryWindow))}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(n).WithObjects(n).Build()
	pending, err := New(c).PendingDefaults(now)
	if err != nil || len(pending) != 0 {
		t.Fatalf("an old default is pending: %+v, %v", pending, err)
	}
}

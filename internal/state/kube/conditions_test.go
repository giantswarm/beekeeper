package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const conditionFixture = "fixture"

const conditionTest = "test"

func conditionsOf(t *testing.T, obj client.Object) []metav1.Condition {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status struct {
			Conditions []metav1.Condition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got.Status.Conditions
}

func TestWriterReadyConditions(t *testing.T) {
	for _, obj := range []client.Object{
		&v1alpha1.Environment{}, &v1alpha1.MergeLane{}, &v1alpha1.Hold{},
		&v1alpha1.Note{}, &v1alpha1.RosterEntry{},
	} {
		t.Run(fmt.Sprintf("%T", obj), func(t *testing.T) {
			scheme, err := Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).Build()
			s := New(c)
			obj.SetName(conditionFixture)
			obj.SetGeneration(1)
			ctx := context.Background()
			if _, err := s.write(ctx, change{obj: obj, create: true}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			first := meta.FindStatusCondition(conditionsOf(t, obj), "Ready")
			if first == nil || first.Status != metav1.ConditionTrue || first.ObservedGeneration != 1 || first.Reason == "" || first.Message == "" || first.LastTransitionTime.IsZero() {
				t.Fatalf("Ready after create: %+v", first)
			}
			// A later generation must be observed without resetting the transition
			// time of an already true condition.
			previous := first.LastTransitionTime
			obj.SetGeneration(2)
			if _, err := s.write(ctx, change{obj: obj, spec: true}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			ready := meta.FindStatusCondition(conditionsOf(t, obj), "Ready")
			if ready == nil || ready.ObservedGeneration != 2 || !ready.LastTransitionTime.Equal(&previous) {
				t.Fatalf("Ready after update: %+v", ready)
			}
		})
	}
}

func TestHolderReadyCondition(t *testing.T) {
	scheme, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	env := &v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: conditionFixture, Generation: 3}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(env).WithObjects(env).Build()
	s := New(c)
	holder := v1alpha1.Holder{Party: v1alpha1.Party{Name: conditionFixture}, Since: metav1.NewTime(time.Now())}
	if err := s.Claim(env.Name, holder); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(env), env); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(env.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != 3 {
		t.Fatalf("Ready after claim: %+v", ready)
	}
	if err := s.Release(env.Name, state.Party{Name: conditionFixture}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(env), env); err != nil {
		t.Fatal(err)
	}
	if env.Status.Holder != nil || !meta.IsStatusConditionTrue(env.Status.Conditions, "Ready") {
		t.Fatalf("release status: %+v", env.Status)
	}
}

func TestEnvtestReadyCondition(t *testing.T) {
	s, _, c := envtestStore(t)
	environment(t, c, conditionFixture)
	if err := s.Claim(conditionFixture, v1alpha1.Holder{
		Party: v1alpha1.Party{Name: conditionFixture}, Purpose: conditionTest, Since: metav1.NewTime(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	env := &v1alpha1.Environment{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: conditionFixture}, env); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(env.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != env.Generation || env.Generation == 0 {
		t.Fatalf("persisted Ready condition: %+v, generation %d", ready, env.Generation)
	}
}

func TestReadyPreservesOtherConditions(t *testing.T) {
	env := &v1alpha1.Environment{}
	other := metav1.Condition{Type: "Other", Status: metav1.ConditionFalse, Reason: "Pending", Message: "Pending work", LastTransitionTime: metav1.NewTime(time.Now().Truncate(time.Second))}
	env.Status.Conditions = []metav1.Condition{other}
	setObserved(env)
	got := meta.FindStatusCondition(env.Status.Conditions, "Other")
	if got == nil || *got != other {
		t.Fatalf("other condition changed: %+v", got)
	}
}

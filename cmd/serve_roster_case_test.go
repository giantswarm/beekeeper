package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func TestAgentsRegisterKeepsObjectAcrossNameCase(t *testing.T) {
	const storedName = "Worker"
	const task = "finish the fix"
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	me := state.Party{Name: storedName, Person: "owner@example.com", Team: ourTeam, Host: lab}
	now := time.Now().UTC().Truncate(time.Second)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.RosterEntry{}).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kube.TeamNamespace(ourTeam)}}).Build()
	store := kube.New(c)
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: me, Registered: now, Task: task}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(kube.RosterObject(me))
	for _, name := range []string{"worker", "WORKER"} {
		me.Name = name
		call := &call{me: me, app: &app{store: store, now: now.Add(time.Second)}, out: &bytes.Buffer{}}
		var req mcp.CallToolRequest
		req.Params.Arguments = map[string]any{paramAgent: name, paramHost: lab}
		result, err := toolAgentsRegister(call, req)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		registered := result.(map[string]any)["agent"].(state.Party)
		if registered.Name != storedName || client.ObjectKeyFromObject(call.concern) != key {
			t.Fatalf("registration or audit changed the address: %+v, %+v", registered, call.concern)
		}
		if err := store.Audit(call.concern, ourTeam, state.Event{By: me, Verb: "serve.agents_register"}); err != nil {
			t.Fatal(err)
		}
		entries := &v1alpha1.RosterEntryList{}
		if err := c.List(context.Background(), entries); err != nil {
			t.Fatal(err)
		}
		if len(entries.Items) != 1 || client.ObjectKeyFromObject(&entries.Items[0]) != key || entries.Items[0].Spec.Party.Name != storedName || entries.Items[0].Status.Task != task {
			t.Fatalf("registration lost or duplicated its task/address: %+v", entries.Items)
		}
		events := &corev1.EventList{}
		if err := c.List(context.Background(), events); err != nil {
			t.Fatal(err)
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Kind != "RosterEntry" || event.InvolvedObject.Name != key.Name {
				t.Fatalf("audit fell back to the namespace: %+v", event.InvolvedObject)
			}
		}
	}
	// Case folding still enforces the owner check.
	me.Person = "other@example.com"
	call := &call{me: me, app: &app{store: store, now: now}, out: &bytes.Buffer{}}
	var req mcp.CallToolRequest
	req.Params.Arguments = map[string]any{paramAgent: me.Name, paramHost: lab}
	if _, err := toolAgentsRegister(call, req); err == nil {
		t.Fatal("another person took the case-folded address")
	}
}

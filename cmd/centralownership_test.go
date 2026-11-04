package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// Switch the owner after authorization reads, immediately before the write.
type ownershipSwitchStore struct {
	state.Store
	before func()
}

func (s *ownershipSwitchStore) Update(fn func(*state.State) ([]state.Event, error)) error {
	if s.before != nil {
		f := s.before
		s.before = nil
		f()
	}
	return s.Store.Update(fn)
}

func ownershipCall(t *testing.T, race ...*func()) *call {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	lane := &v1alpha1.MergeLane{ObjectMeta: metav1.ObjectMeta{Name: "test-lane"}, Spec: v1alpha1.MergeLaneSpec{Repositories: []string{"test/repo"}}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Hold{}, &v1alpha1.MergeLane{}, &v1alpha1.RosterEntry{}).WithObjects(lane).Build()
	fire := func() error {
		if len(race) == 0 || *race[0] == nil {
			return nil
		}
		f := *race[0]
		*race[0] = nil
		f()
		return apierrors.NewConflict(schema.GroupResource{Group: "beekeeper.giantswarm.io", Resource: "fixture"}, "fixture", errors.New("concurrent replacement"))
	}
	store := kube.New(interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := fire(); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := fire(); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := fire(); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}))
	s := &server{cfg: &config.Config{}, store: store, ids: &identity.Verifier{}, now: time.Now}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramAgent: "test-agent", paramHost: "test-machine"}
	return s.newCall(context.Background(), identity.Caller{Email: "first@example.com", Team: "test-team"}, req)
}

func ownershipUpdate(t *testing.T, c *call, fn func(*state.State)) {
	t.Helper()
	if err := c.s.store.Update(func(st *state.State) ([]state.Event, error) { fn(st); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}
func ownershipRead(t *testing.T, c *call) *state.State {
	t.Helper()
	st, err := c.s.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}
func replacementOwner() state.Party {
	return state.Party{Person: "second@example.com", Team: "test-team", Name: "test-agent", Host: "test-machine"}
}

func TestCentralHoldRechecksOwnerAtWrite(t *testing.T) {
	for _, verb := range []string{"set", "lift"} {
		t.Run(verb, func(t *testing.T) {
			c := ownershipCall(t)
			ownershipUpdate(t, c, func(st *state.State) { st.Holds = []state.Hold{{Target: "test/repo", By: c.me, Reason: "original"}} })
			var replacement []state.Hold
			c.app.store = &ownershipSwitchStore{Store: c.store, before: func() {
				ownershipUpdate(t, c, func(st *state.State) { st.Holds[0].By = replacementOwner(); st.Holds[0].Reason = "replacement" })
				replacement = ownershipRead(t, c).Holds
			}}
			req := mcp.CallToolRequest{}
			req.Params.Arguments = map[string]any{paramTarget: "test/repo", paramReason: "changed"}
			run := toolHoldSet
			if verb == "lift" {
				run = toolHoldLift
			}
			_, err := run(c, req)
			if Code(err) != ExitRefused {
				t.Fatalf("want refusal, got %v", err)
			}
			if got := ownershipRead(t, c).Holds; !reflect.DeepEqual(got, replacement) {
				t.Fatalf("replacement changed: %+v", got)
			}
		})
	}
}

func TestCentralLaneRechecksOwnerAtWrite(t *testing.T) {
	c := ownershipCall(t)
	ownershipUpdate(t, c, func(st *state.State) {
		st.Merges = []state.Merge{{Repo: "test/repo", PR: 1, Lane: "test-lane", By: c.me, Phase: state.Waiting}}
	})
	var replacement []state.Merge
	c.app.store = &ownershipSwitchStore{Store: c.store, before: func() {
		ownershipUpdate(t, c, func(st *state.State) { st.Merges[0].By = replacementOwner() })
		replacement = ownershipRead(t, c).Merges
	}}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramRepo: "test/repo", "pr": 1}
	_, err := toolLaneLeave(c, req)
	if Code(err) != ExitRefused {
		t.Fatalf("want refusal, got %v", err)
	}
	if got := ownershipRead(t, c).Merges; !reflect.DeepEqual(got, replacement) {
		t.Fatalf("replacement changed: %+v", got)
	}
}

func TestCentralConversationRechecksOwnerAtWrite(t *testing.T) {
	c := ownershipCall(t)
	ownershipUpdate(t, c, func(st *state.State) { st.Agents = []state.Agent{{Party: c.me}} })
	var replacement []state.Agent
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ownershipUpdate(t, c, func(st *state.State) { st.Agents[0].Person = replacementOwner().Person })
		replacement = ownershipRead(t, c).Agents
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test-thread"}`))
	}))
	defer gateway.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	c.s.gw = newGateway(config.Gateway{URL: gateway.URL, TokenFile: token})
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramText: "hello"}
	_, err := toolConverse(c, req)
	if Code(err) != ExitRefused {
		t.Fatalf("want refusal, got %v", err)
	}
	if got := ownershipRead(t, c).Agents; !reflect.DeepEqual(got, replacement) {
		t.Fatalf("replacement changed: %+v", got)
	}
}

// Lose the first write after its callback authorized the original owner.
// The store must invoke authorization again on the winning snapshot.
func TestCentralOwnerRecheckedOnConflict(t *testing.T) {
	for _, tool := range []string{"hold_set", "hold_lift", "lane_leave", "converse"} {
		t.Run(tool, func(t *testing.T) {
			var race func()
			c := ownershipCall(t, &race)
			req := mcp.CallToolRequest{}
			var run func(*call, mcp.CallToolRequest) (any, error)
			var replace func(*state.State)
			switch tool {
			case "hold_set", "hold_lift":
				ownershipUpdate(t, c, func(st *state.State) { st.Holds = []state.Hold{{Target: "test/repo", By: c.me, Reason: "original"}} })
				req.Params.Arguments = map[string]any{paramTarget: "test/repo", paramReason: "changed"}
				run = toolHoldSet
				if tool == "hold_lift" {
					run = toolHoldLift
				}
				replace = func(st *state.State) { st.Holds[0].By = replacementOwner() }
			case "lane_leave":
				ownershipUpdate(t, c, func(st *state.State) {
					st.Merges = []state.Merge{{Repo: "test/repo", PR: 1, Lane: "test-lane", By: c.me, Phase: state.Waiting}}
				})
				req.Params.Arguments = map[string]any{paramRepo: "test/repo", "pr": 1}
				run = toolLaneLeave
				replace = func(st *state.State) { st.Merges[0].By = replacementOwner() }
			case "converse":
				ownershipUpdate(t, c, func(st *state.State) { st.Agents = []state.Agent{{Party: c.me}} })
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"test-thread"}`)) }))
				defer gateway.Close()
				token := filepath.Join(t.TempDir(), "token")
				if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				c.s.gw = newGateway(config.Gateway{URL: gateway.URL, TokenFile: token})
				req.Params.Arguments = map[string]any{paramText: "hello"}
				run = toolConverse
				replace = func(st *state.State) { st.Agents[0].Person = replacementOwner().Person }
			}
			var replacement *state.State
			race = func() { ownershipUpdate(t, c, replace); replacement = ownershipRead(t, c) }
			_, err := run(c, req)
			if race != nil {
				t.Fatal("write conflict was not exercised")
			}
			if Code(err) != ExitRefused {
				t.Fatalf("want refusal after conflict, got %v", err)
			}
			got := ownershipRead(t, c)
			if !reflect.DeepEqual(got.Holds, replacement.Holds) || !reflect.DeepEqual(got.Merges, replacement.Merges) || !reflect.DeepEqual(got.Agents, replacement.Agents) {
				t.Fatal("replacement changed after conflict")
			}
		})
	}
}

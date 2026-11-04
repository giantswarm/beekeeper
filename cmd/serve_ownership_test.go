package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const (
	ownershipFirst  = "first@example.com"
	ownershipSecond = "second@example.com"
	ownershipTeam   = "test-team"
	ownershipLift   = "lift"
)

// holdOwnershipClient returns the authorization read, then lets a second
// person replace the hold before the command enters its write callback.
type holdOwnershipClient struct {
	client.Client
	reads    int
	owner    string
	changeAt int
}

type laneOwnershipClient struct {
	client.Client
	reads int
	owner string
}

func (c *laneOwnershipClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if lanes, ok := list.(*v1alpha1.MergeLaneList); ok {
		c.reads++
		if c.reads == 2 {
			lane := lanes.Items[0].DeepCopy()
			lane.Status.Queue[0].By.Person = c.owner
			return c.Client.Status().Update(ctx, lane)
		}
	}
	return nil
}

func TestCentralLaneRechecksOwnerAtWrite(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	lane := kube.LaneObject("test-lane").(*v1alpha1.MergeLane)
	lane.Spec.Repositories = []string{scratchRepo}
	lane.Status.Queue = []v1alpha1.LaneEntry{{Repo: scratchRepo, PR: 1, Phase: state.Waiting, By: v1alpha1.Party{Person: ownershipFirst, Name: workerRole, Team: ownershipTeam}}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.MergeLane{}).WithObjects(lane).Build()
	store := kube.New(&laneOwnershipClient{Client: base, owner: ownershipSecond})
	s := &server{cfg: &config.Config{}, store: store, ids: &identity.Verifier{}, now: time.Now}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramRepo: scratchRepo, "pr": 1}
	c := s.newCall(t.Context(), identity.Caller{Email: ownershipFirst, Team: ownershipTeam}, req)
	if _, err := toolLaneLeave(c, req); err == nil || Code(err) != ExitRefused {
		t.Errorf("former owner removed the new person's lane entry: %v", err)
	}
	current := &v1alpha1.MergeLane{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(lane), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Status.Queue) != 1 || current.Status.Queue[0].By.Person != ownershipSecond {
		t.Error("the new owner's lane entry disappeared")
	}
}

func TestCentralConversationRechecksOwnerAtWrite(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	party := state.Party{Name: workerRole, Host: "test-host", Person: ownershipFirst, Team: ownershipTeam}
	roster := kube.RosterObject(party).(*v1alpha1.RosterEntry)
	store := kube.New(fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.RosterEntry{}).Build())
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: party}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The original entry left and another person registered the same
		// address while the gateway was opening the original person's thread.
		if err := store.Update(func(st *state.State) ([]state.Event, error) {
			st.Agents[0].Person = ownershipSecond
			return nil, nil
		}); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":"fixture-thread"}`))
	}))
	t.Cleanup(gateway.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Serve: config.Serve{Gateway: config.Gateway{URL: gateway.URL, TokenFile: token}}}
	s := &server{cfg: cfg, store: store, gw: newGateway(cfg.Serve.Gateway), now: time.Now}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramAgent: party.Name, paramHost: party.Host, paramText: "fixture message"}
	c := s.newCall(t.Context(), identity.Caller{Email: ownershipFirst, Team: party.Team}, req)
	if _, err := toolConverse(c, req); err == nil || Code(err) != ExitRefused {
		t.Errorf("former owner attached a thread to the new person's agent: %v", err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Person != ownershipSecond || st.Agents[0].Conversation != "" {
		t.Errorf("conversation was attached to another person's agent at %s", roster.Name)
	}
}

func (c *holdOwnershipClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if holds, ok := list.(*v1alpha1.HoldList); ok {
		c.reads++
		if c.reads == c.changeAt {
			hold := holds.Items[0].DeepCopy()
			hold.Status.By.Person = c.owner
			return c.Client.Status().Update(ctx, hold)
		}
	}
	return nil
}

func TestCentralHoldRechecksOwnerAtWrite(t *testing.T) {
	for _, tc := range []struct {
		name, operation string
		changeAt        int
	}{{"replace existing hold", setOp, 2}, {"lift existing hold", ownershipLift, 2}, {"conflict retry", setOp, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			hold := kube.HoldObject(scratchRepo).(*v1alpha1.Hold)
			hold.Spec = v1alpha1.HoldSpec{Target: scratchRepo, Reason: "repair in progress"}
			hold.Status.By = v1alpha1.Party{Person: ownershipFirst, Name: workerRole, Team: ownershipTeam}
			hold.Status.At = metav1.NewTime(time.Now().UTC().Truncate(time.Second))
			base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Hold{}).WithObjects(hold).Build()
			store := kube.New(&holdOwnershipClient{Client: base, owner: ownershipSecond, changeAt: tc.changeAt})
			s := &server{cfg: &config.Config{}, store: store, ids: &identity.Verifier{}, now: time.Now}
			req := mcp.CallToolRequest{}
			req.Params.Arguments = map[string]any{paramTarget: hold.Spec.Target, paramReason: "replace hold"}
			c := s.newCall(t.Context(), identity.Caller{Email: ownershipFirst, Team: ownershipTeam}, req)
			call := toolHoldSet
			if tc.operation == ownershipLift {
				call = toolHoldLift
			}
			if _, err := call(c, req); err == nil || Code(err) != ExitRefused {
				t.Errorf("former owner changed the new person's hold: %v", err)
			}
			current := &v1alpha1.Hold{}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(hold), current); err != nil {
				t.Fatalf("the new owner's hold disappeared: %v", err)
			}
			if current.Status.By.Person != ownershipSecond || current.Spec.Reason != hold.Spec.Reason {
				t.Error("the new owner's hold was replaced")
			}
		})
	}
}

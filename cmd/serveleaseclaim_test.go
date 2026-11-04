package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func TestMCPLeaseClaimOutcome(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 2, 0, 0, 0, time.Local)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Environment{}).WithObjects(&v1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: "installation"}}).Build()
	store := kube.New(client)
	s := &server{store: store, cfg: &config.Config{}, now: func() time.Time { return now }}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{paramEnvironment: "installation", paramPurpose: "proof"}
	invoke := func(person string) (string, error) {
		c := s.newCall(context.Background(), identity.Caller{Email: person}, req)
		_, err := toolLeaseClaim(c, req)
		if err != nil {
			return err.Error(), err
		}
		return c.out.String(), nil
	}
	if out, err := invoke("agent-one"); err != nil || out != "held by you since 02:00: claimed installation\n" {
		t.Fatalf("claim: %q, %v", out, err)
	}
	if out, err := invoke("agent-one"); err != nil || out != "held by you since 02:00: installation was yours already\n" {
		t.Fatalf("retry: %q, %v", out, err)
	}
	if out, err := invoke("agent-two"); Code(err) != ExitRefused || !strings.HasPrefix(out, "refused: installation is held") {
		t.Fatalf("refusal: %q, %v", out, err)
	}
	env := &v1alpha1.Environment{}
	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "installation"}, env); err != nil {
		t.Fatal(err)
	}
	env.Status.Queue = []v1alpha1.Grant{{To: kube.APIParty(state.Party{Name: "agent-two", Person: "agent-two"}), At: metav1.NewTime(now)}}
	if err := client.Status().Update(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke("agent-two"); Code(err) != ExitRefused || !strings.HasPrefix(out, "queued: number 1 behind") {
		t.Fatalf("queued: %q, %v", out, err)
	}
	env.Status.Queue = nil
	if err := client.Status().Update(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	was := claimPause
	t.Cleanup(func() { claimPause = was })
	var pauses []time.Duration
	claimPause = func(_ context.Context, d time.Duration) error { pauses = append(pauses, d); return nil }
	req.Params.Arguments.(map[string]any)["wait"] = "25s"
	if out, err := invoke("agent-two"); Code(err) != ExitRefused || !strings.HasSuffix(out, "(waited "+dur(25*time.Second)+")") || len(pauses) != 3 {
		t.Fatalf("wait: %q, %v, %v", out, err, pauses)
	}
	claimPause = func(_ context.Context, d time.Duration) error {
		return store.Release("installation", state.Party{Name: "agent-one", Person: "agent-one"}, now)
	}
	if out, err := invoke("agent-two"); err != nil || !strings.HasPrefix(out, "held by you since") {
		t.Fatalf("wait until held: %q, %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	claimPause = was
	c := s.newCall(ctx, identity.Caller{Email: "agent-three"}, req)
	if _, err := toolLeaseClaim(c, req); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}

	req.Params.Arguments.(map[string]any)["wait"] = "10m"
	if _, err := invoke("agent-two"); Code(err) != ExitUsage {
		t.Fatalf("unbounded wait: %v", err)
	}
}

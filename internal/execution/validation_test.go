package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHostValidationRefusesIncompleteAndUnsafeEndpoints(t *testing.T) {
	f := testFixture(t)
	tests := []struct {
		name   string
		change func(*Host)
	}{
		{"missing identity", func(h *Host) { h.Identity = "" }},
		{"ssh option", func(h *Host) { h.SSH = "-oProxyCommand=fixture" }},
		{"ssh shell", func(h *Host) { h.SSH = "host-a.example;fixture" }},
		{"relative executable", func(h *Host) { h.Executable = "beekeeper" }},
		{"unclean workspace", func(h *Host) { h.WorkspaceRoot = "/srv/../workspaces" }},
		{"unsupported harness", func(h *Host) { h.Harnesses = []string{"other"} }},
		{"missing digest", func(h *Host) { h.PolicyDigest = "" }},
		{"unbounded timeout", func(h *Host) { h.OperationTimeout = "1h" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := f.host
			test.change(&h)
			if err := h.Validate(); err == nil {
				t.Fatal("unsafe host admitted")
			}
		})
	}
}

func TestActivationNeedsAuthenticatedLiveCoordinatorAnswer(t *testing.T) {
	tests := []struct {
		name     string
		decision any
		drop     bool
		starts   int
	}{
		{name: "fresh allow", decision: ActivationDecision{Allowed: true}, starts: 1},
		{name: "revoked", decision: ActivationDecision{}},
		{name: "malformed authority", decision: true},
		{name: "coordinator unreachable", drop: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			f.s.Authorize = nil
			calls := 0
			f.s.Callback = func(_ context.Context, _ []string, raw []byte) ([]byte, error) {
				calls++
				var req CallbackRequest
				if Decode(raw, &req) != nil || req.Operation != "activate" || !VerifyCallback(req, f.r.ControlKey) {
					return nil, errors.New("invalid authority request")
				}
				if test.drop {
					return nil, errors.New("fixture coordinator interruption")
				}
				return json.Marshal(WireResponse{Protocol: Protocol, Ref: req.Ref, RequestID: req.RequestID, Payload: mustJSON(test.decision)})
			}
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("registration-free preparation contacted an activation authority")
			}
			f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
			_, err := f.s.Start(ctx, f.r)
			if (err == nil) != (test.starts == 1) || len(f.launcher.starts) != test.starts || calls != 1 {
				t.Fatalf("wrong authority result: starts %d, calls %d, error %v", len(f.launcher.starts), calls, err)
			}
		})
	}
}

func TestHostChecksFailBeforeWorkspaceOrModelStart(t *testing.T) {
	tests := []struct {
		name   string
		change func(fixture)
	}{
		{"version mismatch", func(f fixture) {
			original := f.s.Command
			f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
				if bin == f.s.Config.Harnesses[HarnessClaude].Executable {
					return []byte("2.1.289"), nil
				}
				return original(ctx, dir, env, bin, args...)
			}
		}},
		{"missing login", func(f fixture) {
			original := f.s.Command
			f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
				if len(args) > 1 && args[0] == authCommand {
					return []byte(`{"loggedIn":false}`), nil
				}
				return original(ctx, dir, env, bin, args...)
			}
		}},
		{"wrong login location", func(f fixture) {
			original := f.s.Command
			f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
				if len(args) > 1 && args[0] == authCommand {
					return []byte(`{"loggedIn":true,"configDirectory":"/srv/other-auth","authMethod":"claude.ai"}`), nil
				}
				return original(ctx, dir, env, bin, args...)
			}
		}},
		{"guard unavailable", func(f fixture) { f.s.GuardProbe = func() error { return errors.New("fixture guard failure") } }},
		{"stale resources", func(f fixture) {
			f.s.Resources = func(context.Context) (Resources, error) {
				return Resources{ObservedAt: f.s.now().Add(-2 * time.Minute)}, nil
			}
		}},
		{"host resource block", func(f fixture) {
			f.s.Resources = func(context.Context) (Resources, error) {
				return Resources{Blocks: []string{"fixture resource block"}, ObservedAt: f.s.now()}, nil
			}
		}},
		{"policy drift", func(f fixture) { f.s.Rules = []byte("different rules") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := testFixture(t)
			test.change(f)
			if _, err := f.s.Start(context.Background(), f.r); err == nil {
				t.Fatal("preflight failure admitted")
			}
			if len(f.launcher.starts) != 0 {
				t.Fatal("model process started")
			}
			if _, err := os.Stat(filepath.Join(f.s.Config.WorkspaceRoot, string(f.r.Ref.Execution))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("workspace created before validation")
			}
		})
	}
}

func TestHostConcurrentReservationsShareOneLimit(t *testing.T) {
	f := testFixture(t)
	f.s.Config.MaxBusy = 1
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest
	requests := []StartRequest{f.r, f.r}
	requests[1].Ref.Execution = ExecutionID(uuid.NewString())
	requests[1].RequestID = uuid.NewString()
	requests[1].ConversationID = uuid.NewString()
	var wg sync.WaitGroup
	results := make(chan error, len(requests))
	for _, request := range requests {
		wg.Add(1)
		go func(r StartRequest) { defer wg.Done(); _, err := f.s.Start(context.Background(), r); results <- err }(request)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 || len(f.launcher.starts) != 0 {
		t.Fatalf("expected one preparation and no launch; accepted %d", accepted)
	}
}

func TestUnknownSupervisorStateRetainsReservation(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Phase = Failed; return nil }); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[UnitName(f.r.Ref)] = ""
	o, err := f.s.Inspect(ctx, f.r.Ref)
	if err != nil || o.ProcessState != Unknown {
		t.Fatal("unreadable supervisor inferred termination", err)
	}
}

func TestWorkspaceRefusesForeignOriginAndPreservesPartialClone(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.git(ctx, f.source, "remote", "set-url", "origin", "https://github.com/example/other.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Start(ctx, f.r); err == nil {
		t.Fatal("foreign source repository accepted")
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("model started with wrong origin")
	}
	g := testFixture(t)
	workspace := filepath.Join(g.s.Config.WorkspaceRoot, string(g.r.Ref.Execution), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Start(ctx, g.r); err == nil {
		t.Fatal("partial clone was overwritten")
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "preserve" { //nolint:gosec // marker in this test's own partial workspace
		t.Fatal("partial workspace lost")
	}
}

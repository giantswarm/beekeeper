package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/state"
)

const workerProofTokenField = "token"

// Uses only pre-feature APIs so the same test runs unmodified on the base.
func TestExecutionGitHubWorkerReadsWithScopedCredential(t *testing.T) {
	a, r := executionTestApp(t)
	a.cfg.Identity.Host = r.Coordinator
	if err := yaml.Unmarshal([]byte("allowedOrgs: [giantswarm]\nworkerApp:\n  clientID: Iv23liFixtureClientID\n  refreshToken: op://Shared/worker-app/refresh-token\n"), &a.cfg.GitHub); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"github":true}`), &r); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.RequestDigest = r.Digest()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.cfg.StateDir, "execution-github")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := map[string]any{r.Repository + "/read": map[string]any{workerProofTokenField: "fixture-scoped-worker-credential", "expires_at": time.Now().Add(time.Hour)}} //nolint:gosec // fake callback credential in a local test fixture, never a GitHub token
	b, _ := json.Marshal(ledger)
	if err := os.WriteFile(filepath.Join(dir, string(r.Ref.Host)+"-"+string(r.Ref.Execution)+"-1.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Issuance verifies the coordinator's current token rather than trusting
	// a possibly stale per-execution ledger after refresh.
	current, _ := json.Marshal(map[string]any{"current": ledger[r.Repository+"/read"]})
	if err := os.WriteFile(filepath.Join(dir, "oauth.json"), current, 0o600); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if err := coordinatorTestCall(t, a, r, "github-token", map[string]any{probeRepositoryCommand: r.Repository, "write": false}, &result, false); err != nil {
		t.Fatalf("worker cannot obtain its scoped GitHub credential: %v", err)
	}
	if result.Token != "fixture-scoped-worker-credential" {
		t.Fatal("worker did not receive its credential")
	}
}

package claim_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
)

// Damaged cache data must never hide a live claim behind a retained ETag
// or make a successful GitHub read panic while repairing the cache.
func TestClaimReadDiscardsBrokenCache(t *testing.T) {
	for _, kind := range []string{"null", "invalid-comments", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			gh := claimtest.New()
			id := gh.Add(issue, claim.Marker{Desk: deskTwo, Session: "synthetic-session", At: t0}.Body())
			path := filepath.Join(t.TempDir(), "cache.json")
			cl := &claim.Client{GH: gh.GH, Cache: path}
			ctx := context.Background()
			if _, err := cl.Read(ctx, issue); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path) //nolint:gosec // fixed cache filename inside t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "null":
				raw = []byte("null")
			case "invalid-comments":
				var doc map[string]map[string]any
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatal(err)
				}
				doc[issue]["comments"] = "damaged"
				raw, err = json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
			case "truncated":
				raw = raw[:len(raw)-1]
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil { //nolint:gosec // fixed cache filename inside t.TempDir; only synthetic data
				t.Fatal(err)
			}
			cs, err := cl.Read(ctx, issue)
			if err != nil || len(cs) != 1 || cs[0].ID != id {
				t.Fatalf("Read with broken cache = %+v, %v; want live claim %d", cs, err, id)
			}
			if gh.Calls["list"] != 2 || gh.Calls["notModified"] != 0 {
				t.Fatalf("calls = %v; want a fresh listing after the broken cache", gh.Calls)
			}

			// A new client can use the repaired, complete cached answer.
			fresh := &claim.Client{GH: gh.GH, Cache: path}
			cs, err = fresh.Read(ctx, issue)
			if err != nil || len(cs) != 1 || cs[0].ID != id || gh.Calls["notModified"] != 1 {
				t.Fatalf("Read with repaired cache = %+v, %v; calls %v", cs, err, gh.Calls)
			}
		})
	}
}

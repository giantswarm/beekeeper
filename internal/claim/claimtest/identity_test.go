package claimtest_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
)

// Repository spelling does not change the GitHub thread a desk sees.
func TestGitHubRepositoryIdentity(t *testing.T) {
	ctx := context.Background()
	m := claim.Marker{Desk: "other-desk", Session: "synthetic", At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	t.Run("fixture helpers share a thread", func(t *testing.T) {
		g := claimtest.New()
		first := g.Add("Owner/Repo#7", "first")
		second := g.Add("owner/repo#7", "second")
		cs := g.Comments("OWNER/REPO#7")
		if len(cs) != 2 || cs[0].ID != first || cs[1].ID != second {
			t.Fatalf("comments = %+v, want both comments in insertion order", cs)
		}
		g.Delete(first)
		if cs := g.Comments("Owner/Repo#7"); len(cs) != 1 || cs[0].ID != second {
			t.Fatalf("after deletion = %+v, want the second comment", cs)
		}
	})
	t.Run("reads share a cached thread", func(t *testing.T) {
		g := claimtest.New()
		id := g.Add("Owner/Repo#7", m.Body())
		cl := &claim.Client{GH: g.GH, Cache: filepath.Join(t.TempDir(), "claims.json")}
		for _, issue := range []string{"owner/repo#7", "OWNER/Repo#7", "Owner/Repo#7"} {
			cs, err := cl.Read(ctx, issue)
			if err != nil || len(cs) != 1 || cs[0].ID != id || cs[0].Marker != m {
				t.Fatalf("Read(%s) = %+v, %v; want the other desk's claim", issue, cs, err)
			}
		}
		if g.Calls["list"] != 1 || g.Calls["notModified"] != 2 {
			t.Fatalf("calls = %v, want one read and two cache hits", g.Calls)
		}
	})
	t.Run("post reaches the same thread", func(t *testing.T) {
		g := claimtest.New()
		cl := &claim.Client{GH: g.GH}
		id, err := cl.Post(ctx, "Owner/Repo#7", m)
		if err != nil {
			t.Fatal(err)
		}
		if cs := g.Comments("owner/repo#7"); len(cs) != 1 || cs[0].ID != id || cs[0].Body != m.Body() {
			t.Fatalf("posted comments = %+v", cs)
		}
	})
	t.Run("edit keeps repository isolation", func(t *testing.T) {
		g := claimtest.New()
		id := g.Add("Owner/Repo#7", "original")
		cl := &claim.Client{GH: g.GH}
		if err := cl.Edit(ctx, "other/repo#7", id, m); !errors.Is(err, claim.ErrGone) {
			t.Fatalf("edit in another repository = %v, want ErrGone", err)
		}
		if err := cl.Edit(ctx, "owner/REPO#7", id, m); err != nil {
			t.Fatal(err)
		}
		if cs := g.Comments("OWNER/repo#7"); len(cs) != 1 || cs[0].Body != m.Body() {
			t.Fatalf("edited comments = %+v", cs)
		}
	})
	t.Run("delete keeps repository isolation", func(t *testing.T) {
		g := claimtest.New()
		id := g.Add("Owner/Repo#7", m.Body())
		cl := &claim.Client{GH: g.GH}
		if err := cl.Delete(ctx, "owner/other#7", id); !errors.Is(err, claim.ErrGone) {
			t.Fatalf("delete in another repository = %v, want ErrGone", err)
		}
		if err := cl.Delete(ctx, "owner/REPO#7", id); err != nil {
			t.Fatal(err)
		}
		if cs := g.Comments("Owner/Repo#7"); len(cs) != 0 {
			t.Fatalf("deleted comments = %+v, want none", cs)
		}
	})
	t.Run("pull lookup ignores repository casing", func(t *testing.T) {
		g := claimtest.New()
		g.Pulls["Owner/Repo#7"] = true
		cl := &claim.Client{GH: g.GH}
		for _, issue := range []string{"owner/repo#7", "OWNER/Repo#7", "Owner/Repo#7"} {
			if pull, err := cl.Pull(ctx, issue); err != nil || !pull {
				t.Fatalf("Pull(%s) = %v, %v; want true", issue, pull, err)
			}
		}
		if pull, err := cl.Pull(ctx, "other/repo#7"); err != nil || pull {
			t.Fatalf("Pull(other/repo#7) = %v, %v; want false", pull, err)
		}
	})
}

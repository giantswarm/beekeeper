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

func TestGitHubRepositoryIdentity(t *testing.T) {
	const mixed = "Owner/Repo#7"
	const lower = "owner/repo#7"
	const other = "owner/other#7"
	marker := claim.Marker{Desk: "desk", Session: "session", At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	t.Run("fixture helpers", func(t *testing.T) {
		g := claimtest.New()
		id := g.Add(mixed, marker.Body())
		g.Add(lower, "second")
		for _, ref := range []string{mixed, lower, "OWNER/REPO#7"} {
			got := g.Comments(ref)
			if len(got) != 2 || got[0].ID != id {
				t.Fatalf("Comments(%q) = %+v", ref, got)
			}
		}
		if got := g.Comments(other); len(got) != 0 {
			t.Fatalf("other repository: %+v", got)
		}
		g.Delete(id)
		if got := g.Comments(lower); len(got) != 1 || got[0].Body != "second" {
			t.Fatalf("after Delete: %+v", got)
		}
	})
	t.Run("cached client reads", func(t *testing.T) {
		g := claimtest.New()
		id := g.Add(mixed, marker.Body())
		c := claim.Client{GH: g.GH, Cache: filepath.Join(t.TempDir(), "cache.json")}
		for _, ref := range []string{mixed, lower, "OWNER/REPO#7"} {
			got, err := c.Read(ctx, ref)
			if err != nil || len(got) != 1 || got[0].ID != id || got[0].Marker != marker {
				t.Fatalf("Read(%q) = %+v, %v", ref, got, err)
			}
		}
		if g.Calls["notModified"] != 2 {
			t.Fatalf("cache hits = %d", g.Calls["notModified"])
		}
		got, err := c.Read(ctx, other)
		if err != nil || len(got) != 0 {
			t.Fatalf("other repository: %+v, %v", got, err)
		}
	})
	t.Run("post", func(t *testing.T) {
		g := claimtest.New()
		seeded := g.Add(mixed, marker.Body())
		c := claim.Client{GH: g.GH}
		id, err := c.Post(ctx, lower, marker)
		if err != nil {
			t.Fatal(err)
		}
		got := g.Comments(mixed)
		if len(got) != 2 || got[0].ID != seeded || got[1].ID != id {
			t.Fatalf("comments after Post: %+v", got)
		}
		if len(g.Comments(other)) != 0 {
			t.Fatal("post crossed repository boundary")
		}
	})
	for _, operation := range []string{"edit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			g := claimtest.New()
			id := g.Add(mixed, marker.Body())
			c := claim.Client{GH: g.GH}
			updated := marker
			updated.Session = "updated"
			mutate := func(ref string) error {
				if operation == "edit" {
					return c.Edit(ctx, ref, id, updated)
				}
				return c.Delete(ctx, ref, id)
			}
			for _, ref := range []string{other, "other/repo#7", "owner/repository#7"} {
				if err := mutate(ref); !errors.Is(err, claim.ErrGone) {
					t.Fatalf("%s(%q) = %v; want ErrGone", operation, ref, err)
				}
			}
			if got := g.Comments(mixed); len(got) != 1 || got[0].Body != marker.Body() {
				t.Fatalf("cross-repository mutation: %+v", got)
			}
			if err := mutate(lower); err != nil {
				t.Fatal(err)
			}
			got := g.Comments(mixed)
			if operation == "delete" {
				if len(got) != 0 {
					t.Fatalf("after Delete: %+v", got)
				}
			} else if len(got) != 1 || got[0].Body != updated.Body() {
				t.Fatalf("after Edit: %+v", got)
			}
		})
	}
	t.Run("pull lookup", func(t *testing.T) {
		g := claimtest.New()
		g.Pulls[mixed] = true
		c := claim.Client{GH: g.GH}
		for _, ref := range []string{mixed, lower, "OWNER/REPO#7", other, "owner/repo#8"} {
			got, err := c.Pull(ctx, ref)
			want := ref != other && ref != "owner/repo#8"
			if err != nil || got != want {
				t.Fatalf("Pull(%q) = %v, %v; want %v", ref, got, err, want)
			}
		}
	})
}

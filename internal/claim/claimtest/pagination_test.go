package claimtest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
)

func TestGitHubCommentPages(t *testing.T) {
	g := claimtest.New()
	var ids []int64
	for i := range 101 {
		ids = append(ids, g.Add("o/r#7", fmt.Sprintf("comment %d", i)))
	}
	for _, tc := range []struct {
		name, query string
		start, size int
		next        bool
	}{
		{name: "first page", query: "per_page=100&page=1", size: 100, next: true},
		{name: "last page", query: "per_page=100&page=2", start: 100, size: 1},
		{name: "past the end", query: "per_page=100&page=3"},
		{name: "large page number", query: "per_page=100&page=2147483647"},
		{name: "default size", size: 30, next: true},
		{name: "smaller page", query: "per_page=2&page=2", start: 2, size: 2, next: true},
		{name: "size capped at 100", query: "per_page=1000&page=1", size: 100, next: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.GH(context.Background(), "api", "-i", "repos/o/r/issues/7/comments?"+tc.query)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var cs []claimtest.Comment
			if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
				t.Fatal(err)
			}
			if cs == nil || len(cs) != tc.size {
				t.Fatalf("got %d comments (nil=%v), want a JSON array of %d", len(cs), cs == nil, tc.size)
			}
			for i, c := range cs {
				if c.ID != ids[tc.start+i] || c.Body != fmt.Sprintf("comment %d", tc.start+i) {
					t.Fatalf("comment %d = %+v, want fixture comment %d", i, c, tc.start+i)
				}
			}
			if link := resp.Header.Get("Link"); strings.Contains(link, `rel="next"`) != tc.next {
				t.Fatalf("Link = %q, want next=%v", link, tc.next)
			}
			if resp.Header.Get("Etag") == "" {
				t.Fatal("page has no ETag")
			}
		})
	}
}

// A claim beyond page one must remain visible as the thread grows, is
// refreshed, and shrinks back to one cacheable page.
func TestGitHubPaginatedClaimRead(t *testing.T) {
	g := claimtest.New()
	for range 100 {
		g.Add("o/r#7", "ordinary discussion")
	}
	cl := &claim.Client{GH: g.GH, Cache: filepath.Join(t.TempDir(), "claims.json")}
	ctx := context.Background()
	check := func(want []claim.Comment, lists, hits int) {
		t.Helper()
		got, err := cl.Read(ctx, "o/r#7")
		if err != nil || len(got) != len(want) {
			t.Fatalf("Read = %+v, %v; want %+v", got, err, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Read = %+v, want %+v", got, want)
			}
		}
		if g.Calls["list"] != lists || g.Calls["notModified"] != hits {
			t.Fatalf("calls = %v, want %d page reads and %d cache hits", g.Calls, lists, hits)
		}
	}
	check(nil, 1, 0)
	m := claim.Marker{Desk: "other-desk", Session: "first", At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	id := g.Add("o/r#7", m.Body())
	check([]claim.Comment{{ID: id, Marker: m}}, 3, 0)
	check([]claim.Comment{{ID: id, Marker: m}}, 5, 0)
	m.Session = "refreshed"
	if err := cl.Edit(ctx, "o/r#7", id, m); err != nil {
		t.Fatal(err)
	}
	check([]claim.Comment{{ID: id, Marker: m}}, 7, 0)
	if err := cl.Delete(ctx, "o/r#7", id); err != nil {
		t.Fatal(err)
	}
	check(nil, 8, 0)
	check(nil, 8, 1)
}

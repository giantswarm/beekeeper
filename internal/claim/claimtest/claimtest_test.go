package claimtest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
)

func TestGitHubCommentPages(t *testing.T) {
	g := claimtest.New()
	for i := 0; i < 101; i++ {
		g.Add("o/r#7", fmt.Sprintf("discussion %d", i))
	}
	for _, tc := range []struct {
		name, query              string
		start, count, next, size int
	}{
		{"first", "per_page=100&page=1", 0, 100, 2, 100},
		{"last", "per_page=100&page=2", 100, 1, 0, 100},
		{"past end", "per_page=100&page=3", 101, 0, 0, 100},
		{"default size", "", 0, 30, 2, 30},
		{"smaller page", "per_page=10&page=3", 20, 10, 4, 10},
		{"maximum size", "per_page=200&page=1", 0, 100, 2, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.GH(context.Background(), "api", "-i", "repos/o/r/issues/7/comments?"+tc.query)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(string(out))), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var got []claimtest.Comment
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("response must be a JSON array")
			}
			want := g.Comments("o/r#7")[tc.start : tc.start+tc.count]
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("comments = %v, want %v", got, want)
			}
			link := resp.Header.Get("Link")
			if tc.next == 0 {
				if link != "" {
					t.Fatalf("unexpected Link: %s", link)
				}
			} else {
				target, relation, ok := strings.Cut(link, ">;")
				if !ok || strings.TrimSpace(relation) != `rel="next"` {
					t.Fatalf("invalid next link: %s", link)
				}
				u, err := url.Parse(strings.TrimPrefix(target, "<"))
				if err != nil {
					t.Fatal(err)
				}
				if u.Path != "repos/o/r/issues/7/comments" || u.Query().Get("page") != fmt.Sprint(tc.next) || u.Query().Get("per_page") != fmt.Sprint(tc.size) {
					t.Fatalf("next URL = %s", u)
				}
			}
			tag := resp.Header.Get("Etag")
			if tag == "" {
				t.Fatal("missing ETag")
			}
			out, err = g.GH(context.Background(), "api", "-i", "repos/o/r/issues/7/comments?"+tc.query, "-H", "If-None-Match: "+tag)
			if err == nil || !strings.Contains(string(out), "304 Not Modified") {
				t.Fatalf("conditional read = %s, %v", out, err)
			}
		})
	}
}

func TestGitHubPaginatedClaimRead(t *testing.T) {
	ctx := context.Background()
	g := claimtest.New()
	for i := 0; i < 100; i++ {
		g.Add("o/r#7", "discussion")
	}
	c := &claim.Client{GH: g.GH, Cache: filepath.Join(t.TempDir(), "claims.json")}
	read := func(want []claim.Comment, lists, cached int) {
		t.Helper()
		beforeList, beforeCached := g.Calls["list"], g.Calls["notModified"]
		got, err := c.Read(ctx, "o/r#7")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("claims = %#v, want %#v", got, want)
		}
		if g.Calls["list"]-beforeList != lists || g.Calls["notModified"]-beforeCached != cached {
			t.Fatalf("read calls: list=%d cached=%d, want %d/%d", g.Calls["list"]-beforeList, g.Calls["notModified"]-beforeCached, lists, cached)
		}
	}
	read(nil, 1, 0)
	read(nil, 0, 1)
	marker := claim.Marker{Desk: "desk", Session: "session", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	id := g.Add("o/r#7", marker.Body())
	read([]claim.Comment{{ID: id, Marker: marker}}, 2, 0)
	read([]claim.Comment{{ID: id, Marker: marker}}, 2, 0)
	marker.Seen = marker.At.Add(time.Hour)
	if err := c.Edit(ctx, "o/r#7", id, marker); err != nil {
		t.Fatal(err)
	}
	read([]claim.Comment{{ID: id, Marker: marker}}, 2, 0)
	if err := c.Delete(ctx, "o/r#7", id); err != nil {
		t.Fatal(err)
	}
	read(nil, 1, 0)
	read(nil, 0, 1)
}

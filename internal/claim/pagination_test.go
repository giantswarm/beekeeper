package claim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
	"github.com/giantswarm/beekeeper/internal/github"
)

type readContextKey struct{}

// pagedDiscussion is deliberately separate from the shared, single-page fake.
// Its first page and ETag stay unchanged when a later claim is released.
type pagedDiscussion struct {
	t        *testing.T
	caller   context.Context
	comments []claimtest.Comment
	pages    []int
	fail     func(context.Context) ([]byte, error)
}

const deskThree = "three"

const malformedJSONMode = "malformed JSON"

func newPagedDiscussion(t *testing.T) *pagedDiscussion {
	t.Helper()
	d := &pagedDiscussion{t: t}
	for id := int64(1); id <= 205; id++ {
		d.comments = append(d.comments, claimtest.Comment{ID: id, Body: "Ordinary discussion."})
	}
	for id, desk := range map[int]string{2: deskOne, 101: deskTwo, 205: deskThree} {
		d.comments[id-1].Body = claim.Marker{Desk: desk, Session: "s", At: t0, Seen: t0}.Body()
	}
	return d
}

func (d *pagedDiscussion) GH(ctx context.Context, args ...string) ([]byte, error) {
	d.t.Helper()
	if github.Repo(ctx) != "o/r" || ctx.Value(readContextKey{}) != d.caller.Value(readContextKey{}) || ctx.Done() != d.caller.Done() {
		d.t.Fatalf("request lost caller context or repository identity")
	}
	gotDeadline, gotOK := ctx.Deadline()
	wantDeadline, wantOK := d.caller.Deadline()
	if gotOK != wantOK || !gotDeadline.Equal(wantDeadline) {
		d.t.Fatal("request lost caller deadline")
	}
	if len(args) < 3 || args[0] != "api" || args[1] != "-i" {
		d.t.Fatalf("unexpected request: %v", args)
	}
	u, err := url.Parse(args[2])
	if err != nil {
		d.t.Fatal(err)
	}
	page, err := strconv.Atoi(u.Query().Get("page"))
	if err != nil || page < 1 || page > 3 || u.Path != "repos/o/r/issues/7/comments" || u.Query().Get("per_page") != "100" {
		d.t.Fatalf("unexpected comments URL: %s", args[2])
	}
	d.pages = append(d.pages, page)
	// A cached first-page ETag would hide edits on the later pages.
	if len(args) != 3 {
		d.t.Errorf("multi-page read sent conditional headers: %v", args)
		return []byte("HTTP/2.0 304 Not Modified\r\n\r\n"), errors.New("HTTP 304")
	}
	if page == 2 && d.fail != nil {
		return d.fail(ctx)
	}
	start, end := (page-1)*100, min(page*100, len(d.comments))
	raw, err := json.Marshal(d.comments[start:end])
	if err != nil {
		d.t.Fatal(err)
	}
	head := "HTTP/2.0 200 OK\r\nEtag: \"unchanged-page\"\r\n"
	if page < 3 {
		// Include a last relation so the reader must select rel=next.
		head += fmt.Sprintf("Link: </repos/o/r/issues/7/comments?per_page=100&page=3>; rel=\"last\", </repos/o/r/issues/7/comments?per_page=100&page=%d>; rel=\"next\"\r\n", page+1)
	}
	return append([]byte(head+"\r\n"), raw...), nil
}

func (d *pagedDiscussion) read(ctx context.Context, cl *claim.Client) ([]claim.Comment, error) {
	d.t.Helper()
	d.caller = ctx
	d.pages = nil
	cs, err := cl.Read(ctx, issue)
	want := []int{1, 2, 3}
	if err != nil {
		want = []int{1, 2}
	}
	if !reflect.DeepEqual(d.pages, want) {
		d.t.Fatalf("pages = %v, want %v", d.pages, want)
	}
	return cs, err
}

func assertDiscussionClaims(t *testing.T, cs []claim.Comment, released bool) {
	t.Helper()
	want := []claim.Comment{
		{ID: 2, Marker: claim.Marker{Desk: deskOne, Session: "s", At: t0, Seen: t0}},
		{ID: 101, Marker: claim.Marker{Desk: deskTwo, Session: "s", At: t0, Seen: t0}},
		{ID: 205, Marker: claim.Marker{Desk: deskThree, Session: "s", At: t0, Seen: t0}},
	}
	if released {
		want[1].Released = t0.Add(time.Hour)
	}
	if !reflect.DeepEqual(cs, want) {
		t.Fatalf("claims = %+v, want %+v", cs, want)
	}
	live, stale := claim.Others(cs, deskOne, t0.Add(2*time.Hour), 24*time.Hour)
	wantLive := want[1:]
	if released {
		wantLive = want[2:]
	}
	if !reflect.DeepEqual(live, wantLive) || len(stale) != 0 {
		t.Fatalf("Others = %+v, %+v; want %+v, no stale claims", live, stale, wantLive)
	}
}

func TestReadAllCommentPagesAndLaterRelease(t *testing.T) {
	d := newPagedDiscussion(t)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), readContextKey{}, "caller"), time.Minute)
	defer cancel()
	cl := &claim.Client{GH: d.GH, Cache: filepath.Join(t.TempDir(), "cache.json")}
	cs, err := d.read(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	assertDiscussionClaims(t, cs, false)
	d.comments[100].Body = claim.Marker{Desk: deskTwo, Session: "s", At: t0, Seen: t0, Released: t0.Add(time.Hour)}.Body()
	cs, err = d.read(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	assertDiscussionClaims(t, cs, true)
}

func TestReadLaterPageFailurePreservesCompleteCache(t *testing.T) {
	failure := errors.New("continuation failed")
	for _, mode := range []string{"failure", malformedJSONMode, "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			d := newPagedDiscussion(t)
			cache := filepath.Join(t.TempDir(), "cache.json")
			cl := &claim.Client{GH: d.GH, Cache: cache}
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), readContextKey{}, "caller"), time.Minute)
			defer cancel()
			cs, err := d.read(ctx, cl)
			if err != nil {
				t.Fatal(err)
			}
			assertDiscussionClaims(t, cs, false)
			before, err := os.ReadFile(cache) //nolint:gosec // a path of this test
			if err != nil {
				t.Fatal(err)
			}
			// Change the prefix as well, making any premature cache write observable.
			d.comments[1].Body = "Now an ordinary comment."
			wantErr := failure
			switch mode {
			case "failure":
				d.fail = func(context.Context) ([]byte, error) { return nil, failure }
			case malformedJSONMode:
				wantErr = nil
				d.fail = func(context.Context) ([]byte, error) { return []byte("HTTP/2.0 200 OK\r\n\r\n[{"), nil }
			case "cancellation":
				wantErr = context.Canceled
				d.fail = func(request context.Context) ([]byte, error) { cancel(); return nil, request.Err() }
			}
			cs, err = d.read(ctx, cl)
			if err == nil || cs != nil {
				t.Fatalf("failed read = %+v, %v; want no prefix and an error", cs, err)
			}
			if wantErr != nil && !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
			if mode == malformedJSONMode {
				var syntax *json.SyntaxError
				if !errors.As(err, &syntax) {
					t.Fatalf("error = %v, want a JSON syntax error", err)
				}
			}
			if !strings.Contains(err.Error(), issue) {
				t.Fatalf("error lacks issue identity: %v", err)
			}
			after, err := os.ReadFile(cache) //nolint:gosec // a path of this test
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed read changed the complete cache")
			}
			d.fail = nil
			d.comments[1].Body = claim.Marker{Desk: deskOne, Session: "s", At: t0, Seen: t0}.Body()
			d.comments[100].Body = claim.Marker{Desk: deskTwo, Session: "s", At: t0, Seen: t0, Released: t0.Add(time.Hour)}.Body()
			fresh := &claim.Client{GH: d.GH, Cache: cache}
			recovery, stop := context.WithCancel(context.WithValue(context.Background(), readContextKey{}, "recovery"))
			defer stop()
			cs, err = d.read(recovery, fresh)
			if err != nil {
				t.Fatal(err)
			}
			assertDiscussionClaims(t, cs, true)
			recovered, err := os.ReadFile(cache) //nolint:gosec // a path of this test
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, recovered) {
				t.Fatal("successful recovery did not update cache")
			}
		})
	}
}

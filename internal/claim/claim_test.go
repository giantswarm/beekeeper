package claim_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claim"
	"github.com/giantswarm/beekeeper/internal/claim/claimtest"
)

const (
	issue   = "o/r#7"
	deskOne = "one"
	deskTwo = "two"
)

var t0 = time.Date(2026, 11, 1, 0, 12, 0, 0, time.UTC)

func TestMarkerRoundTrips(t *testing.T) {
	for name, m := range map[string]claim.Marker{
		"open":     {Desk: "platform-desk", Session: "0b5c1d2e-aaaa", At: t0, Seen: t0.Add(time.Hour)},
		"released": {Desk: "desk two", Session: "id with \"quotes\"", At: t0, Seen: t0, Released: t0.Add(2 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			body := m.Body()
			got, ok := claim.Parse(body)
			if !ok || got != m {
				t.Fatalf("Parse(%q) = %+v, %v; want %+v", body, got, ok, m)
			}
			first, rest, _ := strings.Cut(body, "\n")
			if !strings.HasPrefix(first, "<!-- beekeeper:claim ") || !strings.HasSuffix(first, " -->") || strings.Contains(rest, "<!--") {
				t.Errorf("body %q: the marker is the first line alone", body)
			}
			if got.Open() != m.Released.IsZero() {
				t.Errorf("Open() = %v", got.Open())
			}
		})
	}
}

// A value can neither break the HTML comment nor the marker's line.
func TestMarkerKeepsItsValuesInside(t *testing.T) {
	m := claim.Marker{Desk: "a-->b", Session: "x\n<!-- y -->", At: t0}
	body := m.Body()
	if strings.Count(body, "-->") != 1 || strings.Count(body, "\n") != 1 {
		t.Fatalf("body %q", body)
	}
	got, ok := claim.Parse(body)
	if !ok || got.Desk != "a->b" || got.Session != "x <!- y ->" {
		t.Errorf("Parse = %+v, %v", got, ok)
	}
}

func TestParseRefusesOtherComments(t *testing.T) {
	for _, body := range []string{
		"Working on this.",
		"<!-- beekeeper:claim session=x at=2026-11-01T00:00:00Z -->",          // no desk
		"<!-- beekeeper:claim desk=d -->",                                     // no time
		"<!-- beekeeper:claim desk=d at=yesterday -->",                        // a time that is none
		"<!-- beekeeper:claim desk=\"d at=2026-11-01T00:00:00Z -->",           // an open quote
		"<!-- beekeeper:claim desk=d at=2026-11-01T00:00:00Z",                 // never closed
		"quoting it: <!-- beekeeper:claim desk=d at=2026-11-01T00:00:00Z -->", // not opening the comment
	} {
		if m, ok := claim.Parse(body); ok {
			t.Errorf("Parse(%q) = %+v, want none", body, m)
		}
	}
}

func TestOthersSplitsLiveAndStaleOfOtherDesks(t *testing.T) {
	now := t0.Add(30 * time.Hour)
	cs := []claim.Comment{
		{ID: 5, Marker: claim.Marker{Desk: deskTwo, At: t0, Seen: now.Add(-time.Hour)}},
		{ID: 3, Marker: claim.Marker{Desk: "three", At: t0, Seen: now.Add(-time.Hour)}},
		{ID: 4, Marker: claim.Marker{Desk: "four", At: t0}}, // 30h unrefreshed
		{ID: 6, Marker: claim.Marker{Desk: "five", At: t0, Seen: now, Released: now}},
		{ID: 2, Marker: claim.Marker{Desk: "ONE", At: t0, Seen: now}},
	}
	live, stale := claim.Others(cs, deskOne, now, 24*time.Hour)
	if len(live) != 2 || live[0].ID != 3 || live[1].ID != 5 {
		t.Errorf("live = %+v, want 3 and 5, oldest first", live)
	}
	if len(stale) != 1 || stale[0].ID != 4 {
		t.Errorf("stale = %+v, want 4", stale)
	}
	if d := claim.Desk(cs, deskOne); d == nil || d.ID != 2 {
		t.Errorf("Desk = %+v", d)
	}
	if !strings.HasPrefix(stale[0].Says(now, 24*time.Hour), "stale, desk four") {
		t.Errorf("Says = %q", stale[0].Says(now, 24*time.Hour))
	}
}

// A thread read again unchanged is answered from the cache through its
// ETag; a changed one is read again.
func TestReadSendsTheLastETag(t *testing.T) {
	gh := claimtest.New()
	gh.Add(issue, "Working on this.")
	id := gh.Add(issue, claim.Marker{Desk: deskTwo, Session: "s", At: t0}.Body())
	cl := &claim.Client{GH: gh.GH, Cache: filepath.Join(t.TempDir(), "cache.json")}
	ctx := context.Background()
	for range 2 {
		cs, err := cl.Read(ctx, issue)
		if err != nil || len(cs) != 1 || cs[0].ID != id || cs[0].Desk != deskTwo {
			t.Fatalf("Read = %+v, %v", cs, err)
		}
	}
	if gh.Calls["list"] != 1 || gh.Calls["notModified"] != 1 {
		t.Errorf("calls = %v, want one listing and one 304", gh.Calls)
	}
	if _, err := cl.Post(ctx, issue, claim.Marker{Desk: deskOne, Session: "s1", At: t0}); err != nil {
		t.Fatal(err)
	}
	if cs, err := cl.Read(ctx, issue); err != nil || len(cs) != 2 {
		t.Errorf("Read after a post = %+v, %v", cs, err)
	}
}

func TestEditOfAGoneCommentIsErrGone(t *testing.T) {
	gh := claimtest.New()
	id := gh.Add(issue, "x")
	cl := &claim.Client{GH: gh.GH}
	ctx := context.Background()
	m := claim.Marker{Desk: deskOne, Session: "s", At: t0}
	if err := cl.Edit(ctx, issue, id, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := claim.Parse(gh.Comments(issue)[0].Body); got.Desk != deskOne {
		t.Errorf("edited comment = %q", gh.Comments(issue)[0].Body)
	}
	gh.Delete(id)
	if err := cl.Edit(ctx, issue, id, m); !errors.Is(err, claim.ErrGone) {
		t.Errorf("Edit of a deleted comment = %v, want ErrGone", err)
	}
	if _, err := cl.Read(ctx, "not-a-ref"); err == nil {
		t.Error("Read of no issue succeeded")
	}
}

package board

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

const (
	addIssueCall  = "add ISSUE to P"
	backlogStatus = "backlog"
	unknownField  = "unknown"
)

func TestAddAndShowIssueScoped(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			f := &fakeGH{t: t, item: onBoard, added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I1","project":{"id":"P"}}}}}`}
			if !existing {
				f.item = `{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[]}}}}}`
			}
			c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "Y"}}
			out, err := c.Add(context.Background(), issue5, "", "epic")
			if err != nil {
				t.Fatal(err)
			}
			if !out.OnBoard || out.Status != "Backlog 📦" || out.Team != "Y" || out.Kind != "Epic 🎯" {
				t.Fatalf("fields: %+v", out)
			}
			want := []string{"I1=s1", "I1=t2", "I1=k1"}
			if !existing {
				want = append([]string{addIssueCall}, want...)
			}
			if !slices.Equal(f.mutations, want) {
				t.Fatalf("mutations: %v, want %v", f.mutations, want)
			}
			f.item = onBoard
			before := len(f.mutations)
			shown, err := c.Show(context.Background(), issue5)
			if err != nil {
				t.Fatal(err)
			}
			if shown.Item != "I1" || shown.Status != "Backlog 📦" || shown.Team != "X 🐝" {
				t.Fatalf("show: %+v", shown)
			}
			if len(f.mutations) != before {
				t.Fatal("show mutated board")
			}
			for _, q := range f.queries {
				if strings.Contains(q, "items(") || strings.Contains(q, "fieldValues(") {
					t.Fatalf("board-wide query: %s", q)
				}
				if strings.HasPrefix(q, "query=query") && !strings.Contains(q, "fields(first:50)") && !strings.Contains(q, "issue(number:$n){id projectItems(") {
					t.Fatalf("unexpected query: %s", q)
				}
			}
		})
	}
}

func TestAddValidatesBeforeWriting(t *testing.T) {
	for _, tc := range []struct{ status, kind, team string }{{unknownField, "", "X"}, {backlogStatus, unknownField, "X"}, {backlogStatus, "epic", unknownField}} {
		f := &fakeGH{t: t, item: onBoard}
		c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: tc.team}}
		if _, err := c.Add(context.Background(), issue5, tc.status, tc.kind); err == nil {
			t.Fatal("expected invalid field error")
		}
		if len(f.mutations) > 0 {
			t.Fatalf("wrote before validation: %v", f.mutations)
		}
	}
}

func TestShowAbsentDoesNotAdd(t *testing.T) {
	f := &fakeGH{t: t, item: `{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[]}}}}}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7}}
	out, err := c.Show(context.Background(), issue5)
	if err != nil {
		t.Fatal(err)
	}
	if out.OnBoard || len(f.mutations) > 0 {
		t.Fatalf("unexpected board item: %+v", out)
	}
}

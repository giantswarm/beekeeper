package board

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestReadSearchFindsWorkBeyondFiftyAssignedIssues(t *testing.T) {
	for _, mode := range []string{"complete", "partial", "failure", "cancel", "stalled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			var first []string
			for n := 1; n <= 50; n++ {
				first = append(first, issue(n, `,"assignees":{"nodes":[{"login":"pat"}]}`))
			}
			f := &fakeGH{t: t, pages: []string{`{"data":{"repositoryOwner":{"projectV2":{"items":{"nodes":[]}}}}}`}}
			sentinel := errors.New("search failed")
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				q := args[3]
				if strings.Contains(q, boardItemsQuery) {
					return []byte(f.pages[0]), nil
				}
				if !strings.Contains(q, "s0:search(") {
					return f.run(ctx, args...)
				}
				calls++
				if !strings.Contains(q, "first:50") || !strings.Contains(q, "pageInfo{hasNextPage endCursor}") {
					t.Fatalf("unbounded or unpaged query: %s", q)
				}
				if calls == 1 {
					if mode == "cancel" {
						cancel()
					}
					return []byte(`{"data":{"s0":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[` + strings.Join(first, ",") + `]}}}`), nil
				}
				if calls > 2 {
					t.Fatal("pagination did not stop")
				}
				if !strings.Contains(q, "after:$c") || !strings.Contains(strings.Join(args, " "), "c=next") {
					t.Fatalf("missing continuation: %v", args)
				}
				if mode == "failure" {
					return nil, sentinel
				}
				info := `"hasNextPage":false`
				if mode == "stalled" {
					info = `"hasNextPage":true,"endCursor":"next"`
				}
				errs := ""
				if mode == "partial" {
					errs = `,"errors":[{"path":["s0","nodes",0,"title"],"message":"title unavailable"}]`
				}
				return []byte(`{"data":{"s0":{"pageInfo":{` + info + `},"nodes":[` + issue(51, "") + `]}}` + errs + `}`), nil
			}
			b := config.Board{Owner: "o", Project: 7, People: []string{"worker"}, Order: []config.BoardStep{{Name: "queue", Search: "repo:o/r"}}}
			snap, err := (&Client{GH: gh, Board: b}).Read(ctx, now)
			if mode == "failure" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("error = %v, calls = %d", err, calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ranked := Rank(snap, b, now)
			if len(ranked) != 51 || calls != 2 {
				t.Fatalf("candidates = %d, calls = %d", len(ranked), calls)
			}
			for i, c := range ranked {
				if c.Ref != fmt.Sprintf("o/r#%d", i+1) || (i < 50 && c.Skip == "") || (i == 50 && c.Skip != "") {
					t.Fatalf("candidate %d = %+v", i, c)
				}
			}
			if (mode == "partial" || mode == "stalled") && len(snap.Missing) == 0 {
				t.Fatal("missing partial-answer evidence")
			}
		})
	}
}

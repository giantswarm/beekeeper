package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSquashDeletesLiteralHeadBranch(t *testing.T) {
	const otherBranch = "fix"
	for _, tc := range []struct {
		name  string
		head  string
		other string
		fork  bool
	}{
		{"case 0: fragment character", "fix#topic", otherBranch, false},
		{"case 1: percent-encoded slash", "fix%2Ftopic", "fix/topic", false},
		{"case 2: ordinary slash", "fix/topic", otherBranch, false},
		{"case 3: fork branch", "fix#topic", otherBranch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p map[string]any
			if err := json.Unmarshal([]byte(openPull), &p); err != nil {
				t.Fatal(err)
			}
			p["headRefName"] = tc.head
			p["isCrossRepository"] = tc.fork
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeGH{pull: string(raw), checks: []string{`[]`}}
			refs := map[string]bool{tc.head: true, tc.other: true}
			deleteCalls := 0
			run := func(ctx context.Context, args ...string) ([]byte, error) {
				if len(args) < 3 || args[0] != "api" || args[1] != "-X" || args[2] != http.MethodDelete {
					return f.run(ctx, args...)
				}
				deleteCalls++
				// gh api builds an HTTP request by joining its REST prefix and
				// this endpoint. Model that URL parsing without sending anything.
				req, err := http.NewRequest(http.MethodDelete, "https://api.example.invalid/"+args[3], nil)
				if err != nil {
					return nil, err
				}
				branch := strings.TrimPrefix(req.URL.Path, "/repos/teemow/lab/git/refs/heads/")
				if !refs[branch] {
					return nil, fmt.Errorf("HTTP 422: unknown synthetic ref %q", branch)
				}
				delete(refs, branch)
				return nil, nil
			}
			s := Squash{GH: run, Repo: personalRepo, Number: 7, Now: time.Now}
			doc := s.Run(t.Context())
			wantDeletes := 1
			if tc.fork {
				wantDeletes = 0
			}
			if deleteCalls != wantDeletes {
				t.Fatalf("DELETE calls %d, want %d", deleteCalls, wantDeletes)
			}
			if doc.ExitCode != SquashMerged || !refs[tc.other] || refs[tc.head] != tc.fork {
				t.Fatalf("exit %d, head remains %v, other branch remains %v", doc.ExitCode, refs[tc.head], refs[tc.other])
			}
		})
	}
}

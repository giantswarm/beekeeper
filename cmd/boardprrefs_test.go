package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestBoardNextOpenSessionPRReferences(t *testing.T) {
	for _, tc := range []struct {
		name, prState string
		running       bool
		wantSkip      bool
	}{
		{"open pr", github.Open, true, true},
		{"merged", "MERGED", true, false},
		{overrideClosed, "CLOSED", true, false},
		{"session ended", github.Open, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := &claude.Session{ID: "neighbour", Name: litWorker, Repo: strings.TrimSuffix(refOne, "#1"), Branch: "fix-neighbour"}
			st := &state.State{Records: []state.Record{{Session: worker.Party(), Issue: "o/r#99"}}}
			var sessions []*claude.Session
			if tc.running {
				sessions = append(sessions, worker)
			}
			calls := 0
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if !strings.Contains(strings.Join(args, " "), "--head fix-neighbour --state open") {
					t.Fatalf("args: %v", args)
				}
				return []byte(`[{"number":100,"state":"` + tc.prState + `","title":"Fix #1","body":"Also handles #2 and other/x#7.","closingIssuesReferences":[{"number":3,"repository":{"nameWithOwner":"o/r"}}]}]`), nil
			}
			refs, err := sessionPRReferences(context.Background(), gh, st, sessions, state.Party{})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.running && calls != 0 {
				t.Fatal("queried PRs without a running session")
			}
			sc := pickScope{alive: func(state.Party) bool { return tc.running }, listed: time.Now(), prReferences: refs}
			res := nextFree(st, boardCandidates(4), sc)
			if tc.wantSkip {
				if res.Pick == nil || res.Pick.Ref != "o/r#4" || len(res.Skipped) != 3 {
					t.Fatalf("result: %+v", res)
				}
				want := `referenced by open PR o/r#100 of "Worker"`
				for _, c := range res.Skipped {
					if c.Skip != want {
						t.Fatalf("skip: %q", c.Skip)
					}
				}
				if refs["other/x#7"] != want {
					t.Fatalf("cross repo refs: %v", refs)
				}
				doc, err := nextJSON(res, nextView{})
				if err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(doc)
				if err != nil || !strings.Contains(string(b), "referenced by open PR o/r#100") {
					t.Fatalf("json: %s, %v", b, err)
				}
			} else if res.Pick == nil || res.Pick.Ref != refOne {
				t.Fatalf("pick: %+v", res.Pick)
			}
		})
	}
}

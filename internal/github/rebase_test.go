package github

import (
	"context"
	"errors"
	"testing"
)

func TestReadRebase(t *testing.T) {
	for _, c := range []struct {
		name      string
		answer    string
		err       error
		want      Rebase
		conflicts bool
		wantErr   bool
	}{
		{"rebaseable", `{"rebaseable":true,"mergeable_state":"clean"}`, nil, Rebase{Known: true, Rebaseable: true, State: "clean"}, false, false},
		{"not rebaseable", `{"rebaseable":false,"mergeable_state":"clean"}`, nil, Rebase{Known: true, State: "clean"}, true, false},
		{"a base merge hides it, the state does not", `{"rebaseable":true,"mergeable_state":"dirty"}`, nil, Rebase{Known: true, Rebaseable: true, State: DirtyState}, true, false},
		{"not computed yet", `{"rebaseable":null,"mergeable_state":"unknown"}`, nil, Rebase{State: "unknown"}, false, false},
		{"GitHub unanswered", "", errors.New("gh api: exit status 1: connection reset"), Rebase{}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			run := func(_ context.Context, args ...string) ([]byte, error) {
				asked = append(asked, args...)
				return []byte(c.answer), c.err
			}
			got, err := ReadRebase(context.Background(), run, "o/r", 7)
			if (err != nil) != c.wantErr || got != c.want || got.Conflicts() != c.conflicts {
				t.Fatalf("got %+v (conflicts %v), %v; want %+v (conflicts %v)", got, got.Conflicts(), err, c.want, c.conflicts)
			}
			if len(asked) != 2 || asked[1] != "repos/o/r/pulls/7" {
				t.Errorf("asked %q", asked)
			}
		})
	}
}

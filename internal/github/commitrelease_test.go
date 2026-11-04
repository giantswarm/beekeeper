package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCommitRelease(t *testing.T) {
	for _, tc := range []struct {
		name, tags, want string
		fail             bool
	}{
		{name: "no tag yet"},
		{name: "stable", tags: "v1.2.3\n", want: "v1.2.3"},
		{name: "candidate", tags: "v1.2.3-rc.1\n", want: "v1.2.3-rc.1"},
		{name: "ignore nonrelease tags", tags: "plugin/v1.2.3\nv1.2.4\n", want: "v1.2.4"},
		{name: "read failed", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if got := strings.Join(args, " "); got != `api --paginate repos/o/r/tags?per_page=100 --jq .[] | select(.commit.sha == "merge-sha") | .name` {
					t.Fatalf("unexpected query: %s", got)
				}
				if tc.fail {
					return nil, errors.New("read failed")
				}
				return []byte(tc.tags), nil
			}
			got, err := CommitRelease(context.Background(), run, "o/r", "merge-sha")
			if got != tc.want || (err != nil) != tc.fail {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

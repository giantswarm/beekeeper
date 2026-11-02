package github

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// LatestComments asks for the tail of the conversation (last), and tells
// a member's comment, an outsider's and an automation's apart.
func TestLatestCommentsReadsTheTail(t *testing.T) {
	var asked []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		asked = args
		return []byte(`{"data":{"repository":{"issueOrPullRequest":{"comments":{"nodes":[
			{"author":{"login":"teammate","__typename":"User","databaseId":1},"authorAssociation":"MEMBER","createdAt":"2026-11-01T10:00:00Z","url":"u1"},
			{"author":{"login":"customer","__typename":"User","databaseId":2},"authorAssociation":"NONE","createdAt":"2026-11-01T11:00:00Z","url":"u2"},
			{"author":{"login":"renovate","__typename":"Bot"},"authorAssociation":"NONE","createdAt":"2026-11-01T12:00:00Z","url":"u3"},
			{"author":null,"authorAssociation":"NONE","createdAt":"2026-11-01T13:00:00Z","url":"u4"}]}}}}}`), nil
	}
	cs, err := LatestComments(context.Background(), gh, "o/r#7", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(asked, "last=5") || !slices.Contains(asked, "number=7") || !strings.Contains(strings.Join(asked, " "), "comments(last:$last)") {
		t.Errorf("asked %q", asked)
	}
	if len(cs) != 4 || !cs[0].Member || cs[0].Automation || cs[1].Member || cs[1].Automation || !cs[2].Automation || !cs[3].Automation || cs[1].URL != "u2" {
		t.Errorf("comments %+v", cs)
	}
	if _, err := LatestComments(context.Background(), gh, "o/r", 5); err == nil {
		t.Error("a ref without a number is no error")
	}
}

package github

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// LatestComments asks for the tail of the conversation (last), and tells
// a member's comment, an outsider's and an automation's apart, with each
// comment's text.
func TestLatestCommentsReadsTheTail(t *testing.T) {
	var asked []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		asked = args
		return []byte(`{"data":{"repository":{"issueOrPullRequest":{"comments":{"nodes":[
			{"author":{"login":"teammate","__typename":"User","databaseId":1},"authorAssociation":"MEMBER","createdAt":"2026-11-01T10:00:00Z","url":"u1","body":"waits on o/r#9"},
			{"author":{"login":"customer","__typename":"User","databaseId":2},"authorAssociation":"NONE","createdAt":"2026-11-01T11:00:00Z","url":"u2"},
			{"author":{"login":"renovate","__typename":"Bot"},"authorAssociation":"NONE","createdAt":"2026-11-01T12:00:00Z","url":"u3"},
			{"author":{"login":"friend","__typename":"User","databaseId":3},"authorAssociation":"COLLABORATOR","createdAt":"2026-11-01T12:30:00Z","url":"u5"},
			{"author":null,"authorAssociation":"NONE","createdAt":"2026-11-01T13:00:00Z","url":"u4"}]}}}}}`), nil
	}
	cs, err := LatestComments(context.Background(), gh, "o/r#7", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(asked, "last=5") || !slices.Contains(asked, "number=7") || !strings.Contains(strings.Join(asked, " "), "comments(last:$last)") {
		t.Errorf("asked %q", asked)
	}
	if len(cs) != 5 || !cs[0].Member || cs[0].Automation || cs[1].Member || cs[1].Automation || cs[1].Association != associationNone ||
		!cs[2].Automation || !cs[3].Member || cs[3].Automation || !cs[4].Automation || cs[1].URL != "u2" || cs[0].Body != "waits on o/r#9" {
		t.Errorf("comments %+v", cs)
	}
	if _, err := LatestComments(context.Background(), gh, "o/r", 5); err == nil {
		t.Error("a ref without a number is no error")
	}
}

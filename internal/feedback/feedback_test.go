package feedback_test

import (
	"encoding/json"
	"testing"

	"github.com/giantswarm/beekeeper/internal/feedback"
)

// replyTS is a reply's ts in the tests.
const replyTS = "1791126240.000100"

func TestParsePost(t *testing.T) {
	p, err := feedback.ParsePost(`{"message_link":"https:\/\/x.slack.com\/archives\/D1\/p1791126073108809","message_context":{"message_ts":"1791126073.108809","channel_id":"D1"}}`)
	if err != nil || p != (feedback.Post{Channel: "D1", TS: "1791126073.108809"}) {
		t.Errorf("ParsePost = %+v, %v", p, err)
	}
	for _, bad := range []string{"sent", `{"message_link":"x"}`} {
		if _, err := feedback.ParsePost(bad); err == nil {
			t.Errorf("ParsePost(%q) took it", bad)
		}
	}
}

// threadResult is slack_read_thread's result for messages.
func threadResult(t *testing.T, messages string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"messages": messages, "pagination_info": "There are no more messages in this thread.\n"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseThread(t *testing.T) {
	th, err := feedback.ParseThread(threadResult(t, `=== THREAD PARENT MESSAGE ===
From: Ada (U1)
Time: 2026-10-04 15:00:12 CEST
Message TS: 1791126073.108809
Status 15:00 CEST

=== THREAD REPLIES (2 total) ===

--- Reply 1 of 2 ---
From: Ada (U1)
Time: 2026-10-04 15:04:00 CEST
Message TS: 1791126240.000100
Board pull 9: look at the CI first

and then merge.
*Sent using* <@U9|Claude>
Reactions: eyes (1)

--- Reply 2 of 2 ---
From: Bob Smith (U2)
Time: 2026-10-04 15:05:00 CEST
Message TS: 1791126300.000200
hi
`))
	if err != nil {
		t.Fatal(err)
	}
	if th.Parent != (feedback.Message{User: "U1", TS: "1791126073.108809", Text: "Status 15:00 CEST"}) {
		t.Errorf("parent %+v", th.Parent)
	}
	want := []feedback.Message{
		{User: "U1", TS: replyTS, Text: "Board pull 9: look at the CI first\n\nand then merge."},
		{User: "U2", TS: "1791126300.000200", Text: "hi"},
	}
	if len(th.Replies) != len(want) || th.Replies[0] != want[0] || th.Replies[1] != want[1] {
		t.Errorf("replies %+v", th.Replies)
	}
	for _, bad := range []string{"not json", threadResult(t, "nothing"), threadResult(t, "=== THREAD PARENT MESSAGE ===\nFrom: Ada (U1)\nno ts\n")} {
		if _, err := feedback.ParseThread(bad); err == nil {
			t.Errorf("ParseThread(%q) took it", bad)
		}
	}
}

func TestLater(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{replyTS, "", true},
		{replyTS, replyTS, false},
		{"1791126240.000200", replyTS, true},
		{"1791126241.000000", "1791126240.999999", true},
		{"999999999.000000", "1000000000.000000", false},
	} {
		if got := feedback.Later(c.a, c.b); got != c.want {
			t.Errorf("Later(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestRoute(t *testing.T) {
	names := []string{"Board pull 9", "Board pull 91", "Supervisor run 69"}
	for _, c := range []struct{ text, to, msg string }{
		{"board pull 9: look at the CI", "Board pull 9", "look at the CI"},
		{"Board pull 91:   merge it", "Board pull 91", "merge it"},
		{"slow down please", "", "slow down please"},
		{"Board pull 9 looks stuck", "", "Board pull 9 looks stuck"},
		{"Board pull 9:", "", "Board pull 9:"},
	} {
		if to, msg := feedback.Route(c.text, names); to != c.to || msg != c.msg {
			t.Errorf("Route(%q) = %q, %q; want %q, %q", c.text, to, msg, c.to, c.msg)
		}
	}
}

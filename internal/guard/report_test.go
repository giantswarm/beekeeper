package guard

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/post"
)

func TestReportCheck(t *testing.T) {
	slack := func(msg string) []byte {
		return []byte(`{"tool_name":"mcp__claude_ai_Slack__slack_send_message","tool_input":{"channel_id":"U1","message":` + msg + `}}`)
	}
	if out := ReportCheck(slack(`"[beekeeper#109](https://github.com/giantswarm/beekeeper/pull/109) merged"`), post.Check); out != nil {
		t.Errorf("a valid post: %s", out)
	}
	out := string(ReportCheck(slack(`"merged #109"`), post.Check))
	if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "#109 is not a link") {
		t.Errorf("a bare reference: %s", out)
	}
	if out := string(ReportCheck([]byte(`{"tool_name":"mcp__claude_ai_Slack__slack_send_message","tool_input":{"channel_id":"U1"}}`), post.Check)); !strings.Contains(out, "empty") {
		t.Errorf("no message: %s", out)
	}
	for _, in := range []string{`{"tool_name":"Bash","tool_input":{"command":"ls"}}`, `not json`} {
		if out := ReportCheck([]byte(in), post.Check); out != nil {
			t.Errorf("%s: %s", in, out)
		}
	}
}

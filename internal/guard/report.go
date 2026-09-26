package guard

import (
	"encoding/json"
	"strings"
)

// PostTool is the suffix of the tool that posts a report: the Slack
// connector's slack_send_message.
const PostTool = "slack_send_message"

// ReportCheck decides one PreToolUse event of a reporter session: a post
// whose message check finds wrong (post.Report) is denied, with what to
// fix as the reason. Any other tool passes, nil; a post without a message
// is denied.
func ReportCheck(input []byte, check func(string) []string) []byte {
	var ev struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
	}
	if json.Unmarshal(input, &ev) != nil || !strings.HasSuffix(ev.ToolName, PostTool) {
		return nil
	}
	msg, _ := ev.ToolInput["message"].(string)
	problems := check(msg)
	if len(problems) == 0 {
		return nil
	}
	return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Not posted: the report fails beekeeper's check. Fix every point and post again:\n- " +
		strings.Join(problems, "\n- ")})
}

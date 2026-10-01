package guard

import "fmt"

// AskTool is Claude Code's tool that puts a question to the person and
// blocks the session until the person answers.
const AskTool = "AskUserQuestion"

// ask decides an AskUserQuestion call: only the guide puts questions to the
// person, each with what the person needs to answer it; every other session
// files a note for the guide and carries on.
func (h Hook) ask(session string, input map[string]any) []byte {
	guide, person := false, ""
	if h.Guide != nil {
		guide, person = h.Guide(session)
	}
	if guide {
		if r := h.questionRefusal(input, person); r != "" {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
		}
		return nil
	}
	if person == "" {
		person = "<person>"
	}
	return answer(hookOutput{PermissionDecision: decisionDeny, Reason: fmt.Sprintf(
		"Refused: only the guide (`beekeeper guide`) puts questions to %[1]s; a question here blocks this session until %[1]s answers. "+
			"File it for the guide and carry on with the default or with other work:\n"+
			"  beekeeper note add --for %[1]s \"<the question, every issue or PR as its full URL>\" --status-quo \"<what is true now>\" --why \"<why it needs %[1]s>\" --option \"<choice>: <consequence>\" --default \"<the action if nobody answers>\"\n"+
			"The same goes for a permission, browser or configuration approval you would wait on: a note, never a wait.", person)})
}

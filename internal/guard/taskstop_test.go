package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// A TaskStop passes, and the gate its task runs is ended first: the hook
// hands StopTask the session and the task (the deprecated shell_id too) and
// tells the session what it ended.
func TestTaskStopEndsTheTasksGate(t *testing.T) {
	var got []string
	h := Hook{StopTask: func(session, task string) string {
		got = append(got, session+" "+task)
		if task == "bgate" {
			return "beekeeper gate: TaskStop bgate stops its gate"
		}
		return ""
	}}
	for _, c := range []struct{ input, want string }{
		{`{"tool_name":"TaskStop","session_id":"s1","tool_input":{"task_id":"bgate"}}`, "TaskStop bgate stops its gate"},
		{`{"tool_name":"TaskStop","session_id":"s1","tool_input":{"shell_id":"bother"}}`, ""},
	} {
		out := h.Decide([]byte(c.input))
		if c.want == "" {
			if out != nil {
				t.Errorf("%s: answered %s, want nothing", c.input, out)
			}
			continue
		}
		var o map[string]hookOutput
		if err := json.Unmarshal(out, &o); err != nil {
			t.Fatalf("%s: %v (%s)", c.input, err, out)
		}
		d := o["hookSpecificOutput"]
		if d.PermissionDecision != "" || !strings.Contains(d.AdditionalContext, c.want) {
			t.Errorf("%s: answered %+v", c.input, d)
		}
	}
	if strings.Join(got, ",") != "s1 bgate,s1 bother" {
		t.Errorf("StopTask got %v", got)
	}
}

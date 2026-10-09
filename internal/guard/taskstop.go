package guard

// TaskStopTool is the tool a session stops one of its background tasks
// with.
const TaskStopTool = "TaskStop"

// taskStop decides a TaskStop call: it always passes, and the gate the task
// runs, if any, goes with it (StopTask), whose answer the stopping session
// reads as additional context.
func (h Hook) taskStop(session string, input map[string]any) []byte {
	if h.StopTask == nil {
		return nil
	}
	task, _ := input["task_id"].(string)
	if task == "" {
		task, _ = input["shell_id"].(string) // the deprecated name
	}
	said := h.StopTask(session, task)
	if said == "" {
		return nil
	}
	return answer(hookOutput{AdditionalContext: said})
}

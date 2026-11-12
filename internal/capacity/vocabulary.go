package capacity

// The command's names capacity writes and reads. They are the command's;
// its TestCapacityVocabulary keeps each equal to the command's own.
const (
	claudeComm = "claude"
	noProblems = "none"
	workerRole = "worker"
)

// Vocabulary is the constants above by name, for the command's test.
func Vocabulary() map[string]any {
	return map[string]any{
		"claudeComm": claudeComm,
		"noProblems": noProblems,
		"workerRole": workerRole,
	}
}

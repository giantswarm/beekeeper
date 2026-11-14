package gate

// The command's names the gate writes and reads. They are the command's;
// its TestGateVocabulary keeps each equal to the command's own.
const (
	ExitCentral    = 69
	claudeComm     = "claude"
	holdParkFlag   = "hold:"
	paramReason    = "reason"
	paramRepo      = "repo"
	squashMergeCmd = "squash-merge"
)

// Vocabulary is the constants above by name, for the command's test.
func Vocabulary() map[string]any {
	return map[string]any{
		"ExitCentral":    ExitCentral,
		"claudeComm":     claudeComm,
		"holdParkFlag":   holdParkFlag,
		"paramReason":    paramReason,
		"paramRepo":      paramRepo,
		"squashMergeCmd": squashMergeCmd,
	}
}

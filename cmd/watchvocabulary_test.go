package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/watch"
)

// TestWatchVocabulary keeps the copy the watch has of the command's names equal to
// the command's own: it cannot import them.
//
// control: none: it guards the copies of the command's constants in internal/watch,
// which the merge base does not have, so it cannot build there.
func TestWatchVocabulary(t *testing.T) {
	own := map[string]any{
		"ExitRelieved":          ExitRelieved,
		"answerRecorded":        answerRecorded,
		vocabClaudeComm:         claudeComm,
		"closedOvertaken":       closedOvertaken,
		"configFlag":            configFlag,
		"envTimerCommand":       envTimerCommand,
		"envTimerDue":           envTimerDue,
		"envTimerID":            envTimerID,
		"envTimerLate":          envTimerLate,
		"episodeGap":            episodeGap,
		"freeFile":              freeFile,
		"ghNone":                ghNone,
		"goneActivityPrefix":    goneActivityPrefix,
		"guideRoleAlias":        guideRoleAlias,
		"hookTypeCommand":       hookTypeCommand,
		"hooksKey":              hooksKey,
		"loopUnit":              loopUnit,
		"mailboxKey":            mailboxKey,
		"maxFailedSuccessors":   maxFailedSuccessors,
		"nameAgentsLeave":       nameAgentsLeave,
		"nameAgentsRegister":    nameAgentsRegister,
		"nameReadFeed":          nameReadFeed,
		"noteDone":              noteDone,
		"noteLogin":             noteLogin,
		"noteTodo":              noteTodo,
		"overtakenSep":          overtakenSep,
		"owedRetry":             owedRetry,
		"ownerWait":             ownerWait,
		"paramAfter":            paramAfter,
		"paramDone":             paramDone,
		"paramTask":             paramTask,
		"parkRefRetry":          parkRefRetry,
		"probeInProgressPrefix": probeInProgressPrefix,
		"probeTimeout":          probeTimeout,
		"resourceFeed":          resourceFeed,
		"resourceMailbox":       resourceMailbox,
		"restartedEnv":          restartedEnv,
		"secLoad":               secLoad,
		"secMemory":             secMemory,
		"secPressure":           secPressure,
		"secScope":              secScope,
		"secSessions":           secSessions,
		"stoppedNothing":        stoppedNothing,
		"successorBackoff":      successorBackoff,
		"swapFile":              swapFile,
		"timerPRChecks":         timerPRChecks,
		"timerTick":             timerTick,
		"unitActive":            unitActive,
		"unroutedResume":        unroutedResume,
		"usageFile":             usageFile,
		"usageHighFile":         usageHighFile,
		"verbHookRefused":       verbHookRefused,
		"verbLaneSettled":       verbLaneSettled,
	}
	got := watch.Vocabulary()
	if len(got) != len(own) {
		t.Fatalf("watch names %d constants, the test %d", len(got), len(own))
	}
	for name, want := range own {
		if got[name] != want {
			t.Errorf("watch %s = %v, the command's is %v", name, got[name], want)
		}
	}
}

package cmd

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/loop"
)

// TestLoopVocabulary keeps the loop's copy of the command's names equal to
// the command's own: the loop cannot import them.
func TestLoopVocabulary(t *testing.T) {
	own := map[string]any{
		"ExitError":                        ExitError,
		"ExitGateDuplicate":                ExitGateDuplicate,
		"ExitGateQueued":                   ExitGateQueued,
		"ExitGateRefused":                  ExitGateRefused,
		"ExitGateUnread":                   ExitGateUnread,
		"ExitOutdated":                     ExitOutdated,
		"ExitRefused":                      ExitRefused,
		"agentStartName":                   agentStartName,
		"agentWakeEvent":                   agentWakeEvent,
		"agentWakeName":                    agentWakeName,
		"agentsName":                       agentsName,
		"answerRecorded":                   answerRecorded,
		"authorModelFlag":                  authorModelFlag,
		"bgStopCmd":                        bgStopCmd,
		"boardEscalateName":                boardEscalateName,
		"configFlag":                       configFlag,
		"continuationQueuedEvent":          continuationQueuedEvent,
		"deliveryBlocked":                  deliveryBlocked,
		"deliveryExemptOnlyFlag":           deliveryExemptOnlyFlag,
		"deliveryFlowing":                  deliveryFlowing,
		"devctlBinary":                     devctlBinary,
		"diskHoldingFile":                  diskHoldingFile,
		"effortFlag":                       effortFlag,
		"epicOwnerTask":                    epicOwnerTask,
		"epicSnapshotFile":                 epicSnapshotFile,
		"excludeFlag":                      excludeFlag,
		"gateCmdName":                      gateCmdName,
		"harnessAuto":                      harnessAuto,
		"hostUpdateCmdName":                hostUpdateCmdName,
		"hostUpdateDeferred":               hostUpdateDeferred,
		"inventoryGraphQLCommand":          inventoryGraphQLCommand,
		"kindUnknown":                      kindUnknown,
		"modelFlag":                        modelFlag,
		"noteDecision":                     noteDecision,
		"noteLogin":                        noteLogin,
		"noteStatusOpen":                   noteStatusOpen,
		"parkEvent":                        parkEvent,
		"reasonFlagName":                   reasonFlagName,
		"reasonMergeValidationUnavailable": reasonMergeValidationUnavailable,
		"resumedEvent":                     resumedEvent,
		"reviewFlag":                       reviewFlag,
		"reviewOfSelfHostedFlag":           reviewOfSelfHostedFlag,
		"reviewedMergeRoute":               reviewedMergeRoute,
		"runMerge":                         runMerge,
		"selfUpdateCmdName":                selfUpdateCmdName,
		"taskKindArg":                      taskKindArg,
		"taskKindReview":                   taskKindReview,
		"throughputCadence":                throughputCadence,
		"throughputObservationFile":        throughputObservationFile,
		"verbStart":                        verbStart,
		"verdictAvailable":                 verdictAvailable,
		"workerGitHubAPIGroup":             workerGitHubAPIGroup,
		"workerRole":                       workerRole,
	}
	got := loop.Vocabulary()
	if len(got) != len(own) {
		t.Fatalf("the loop names %d constants, the test %d", len(got), len(own))
	}
	for name, want := range own {
		if got[name] != want {
			t.Errorf("loop %s = %v, the command's is %v", name, got[name], want)
		}
	}
}

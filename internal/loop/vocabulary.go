package loop

import (
	"time"
)

// The command's names the loop writes and reads: its commands and flags,
// the verbs and files it shares, its exit codes. They are the command's;
// its TestLoopVocabulary keeps each equal to the command's own.
const (
	ExitError                        = 1
	ExitGateDuplicate                = 3
	ExitGateQueued                   = 76
	ExitGateRefused                  = 77
	ExitGateUnread                   = 74
	ExitOutdated                     = 125
	ExitRefused                      = 3
	agentStartName                   = "start"
	agentWakeEvent                   = "agents.wake"
	agentWakeName                    = "wake"
	agentsName                       = "agents"
	answerRecorded                   = "answer.recorded"
	authorModelFlag                  = "author-model"
	bgStopCmd                        = "stop"
	boardEscalateName                = "escalate"
	configFlag                       = "config"
	continuationQueuedEvent          = "agents.continuation-queued"
	defaultEffortFlag                = "--default-effort"
	deliveryBlocked                  = "blocked"
	deliveryExemptOnlyFlag           = "--wip-exempt-only"
	deliveryFlowing                  = "flowing"
	devctlBinary                     = "devctl"
	diskHoldingFile                  = "disk-holding.json"
	effortFlag                       = "--effort"
	epicOwnerTask                    = "Epic owner: "
	epicSnapshotFile                 = "epic-delivery.json"
	excludeFlag                      = "exclude"
	executionReleasedEvent           = "execution.released"
	gateCmdName                      = "gate"
	harnessAuto                      = "auto"
	hostUpdateCmdName                = "update"
	hostUpdateDeferred               = "host update deferred"
	inventoryGraphQLCommand          = "graphql"
	kindUnknown                      = "unknown"
	modelFlag                        = "--model"
	noteDecision                     = "decision"
	noteLogin                        = "login"
	noteStatusOpen                   = "open"
	parkEvent                        = "agents.park"
	reasonFlagName                   = "reason"
	reasonMergeValidationUnavailable = "merge-result validation unavailable"
	resumedEvent                     = "agents.resumed"
	reviewFlag                       = "review"
	reviewOfSelfHostedFlag           = "review-of-self-hosted"
	reviewedMergeRoute               = "reviewed merge"
	runMerge                         = "merge"
	selfUpdateCmdName                = "self-update"
	taskKindReview                   = "review"
	throughputCadence                = time.Minute
	throughputObservationFile        = "throughput-observation.json"
	verbStart                        = "agents.start"
	verdictAvailable                 = "available"
	workerGitHubAPIGroup             = "api"
	workerRole                       = "worker"
)

// Vocabulary is the constants above by name, for the command's test.
func Vocabulary() map[string]any {
	return map[string]any{
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
		"defaultEffortFlag":                defaultEffortFlag,
		"deliveryBlocked":                  deliveryBlocked,
		"deliveryExemptOnlyFlag":           deliveryExemptOnlyFlag,
		"deliveryFlowing":                  deliveryFlowing,
		"devctlBinary":                     devctlBinary,
		"diskHoldingFile":                  diskHoldingFile,
		"effortFlag":                       effortFlag,
		"epicOwnerTask":                    epicOwnerTask,
		"epicSnapshotFile":                 epicSnapshotFile,
		"excludeFlag":                      excludeFlag,
		"executionReleasedEvent":           executionReleasedEvent,
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
		"taskKindReview":                   taskKindReview,
		"throughputCadence":                throughputCadence,
		"throughputObservationFile":        throughputObservationFile,
		"verbStart":                        verbStart,
		"verdictAvailable":                 verdictAvailable,
		"workerGitHubAPIGroup":             workerGitHubAPIGroup,
		"workerRole":                       workerRole,
	}
}

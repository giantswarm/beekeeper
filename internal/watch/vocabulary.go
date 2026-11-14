package watch

import (
	"time"
)

// The command's names the watch writes and reads. They are the command's;
// its TestWatchVocabulary keeps each equal to the command's own.
const (
	ExitRelieved          = 4
	answerRecorded        = "answer.recorded"
	claudeComm            = "claude"
	closedOvertaken       = "closed as overtaken"
	configFlag            = "config"
	envTimerCommand       = "BEEKEEPER_TIMER_COMMAND"
	envTimerDue           = "BEEKEEPER_TIMER_DUE"
	envTimerID            = "BEEKEEPER_TIMER_ID"
	envTimerLate          = "BEEKEEPER_TIMER_LATE"
	episodeGap            = 15 * time.Minute
	freeFile              = "free.json"
	ghNone                = "none"
	goneActivityPrefix    = "gone, last activity "
	guideRoleAlias        = "the guide"
	hookTypeCommand       = "command"
	hooksKey              = "hooks"
	loopUnit              = "beekeeper-supervisor-loop.service"
	mailboxKey            = "mailbox"
	maxFailedSuccessors   = 3
	nameAgentsLeave       = "agents_leave"
	nameAgentsRegister    = "agents_register"
	nameReadFeed          = "read_feed"
	noteDone              = "note.done"
	noteLogin             = "login"
	noteTodo              = "todo"
	overtakenSep          = "; the note: "
	owedRetry             = time.Minute
	ownerWait             = 5 * time.Minute
	paramAfter            = "after"
	paramDone             = "done"
	paramTask             = "task"
	parkRefRetry          = 5 * time.Minute
	probeInProgressPrefix = "probe already in progress: "
	probeTimeout          = 10 * time.Second
	resourceFeed          = "beekeeper://feed"
	resourceMailbox       = "beekeeper://mailbox/"
	restartedEnv          = "BEEKEEPER_RESTARTED"
	secLoad               = "load"
	secMemory             = "memory"
	secPressure           = "pressure"
	secScope              = "desktop scope"
	secSessions           = "sessions"
	stoppedNothing        = "beekeeper is stopped: nothing revives, starts or reopens until beekeeper resume"
	successorBackoff      = 5 * time.Minute
	swapFile              = "swap.json"
	timerPRChecks         = "pr-checks"
	timerTick             = 30 * time.Second
	unitActive            = "active"
	unroutedResume        = "its harness has no message route, so nobody wakes it: it ends its own park with `beekeeper agents resume`, no agent named, which prints what settled the wait"
	usageFile             = "usage.json"
	usageHighFile         = "usage-high.json"
	verbHookRefused       = "hook.refused"
	verbLaneSettled       = "lane.settled"
)

// Vocabulary is the constants above by name, for the command's test.
func Vocabulary() map[string]any {
	return map[string]any{
		"ExitRelieved":          ExitRelieved,
		"answerRecorded":        answerRecorded,
		"claudeComm":            claudeComm,
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
}

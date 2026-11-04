package cmd

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The channels an answer came through, which answer.recorded names after
// "via": beekeeper sets them from where it runs, never from a flag.
const (
	// channelTerminal is the person's terminal on the desk: no agent
	// session in the environment or among the process's ancestors, a
	// terminal on its stdin.
	channelTerminal = "terminal"
	// channelSession is an answer recorded from inside an agent session:
	// relayed, never the person's own.
	channelSession = "session"
	// channelScript is an answer recorded outside any session and without
	// a terminal: a script's, never the person's own.
	channelScript = "script"
	// channelCentral is the central instance's note_answer called by the
	// person, as their verified email.
	channelCentral = "central"
	// channelSlack is the central instance's Slack decision answered by
	// the person, as their verified email.
	channelSlack = "slack"
)

// answerFresh bounds the age of a completed guide dialog result.
const answerFresh = 24 * time.Hour

// agentComms are the processes of the agent harnesses: one among a
// command's ancestors runs it inside an agent session.
var agentComms = []string{claudeComm, omp.Comm, agy.Comm, codex.Comm}

// answerSeat is where the running command sits: the agent session it runs
// inside ("" for none) and whether a terminal is on its stdin. A process
// table it cannot read counts as a session: nothing shows there is none.
var answerSeat = func(a *app) (session string, terminal bool) {
	terminal = term.IsTerminal(int(os.Stdin.Fd()))
	if id := a.managedExecutionID(); id != "" {
		return "execution " + id, terminal
	}
	if p := envParty(os.Getenv); p.Session != "" || p.HostSession != "" {
		return cmp.Or(p.Name, p.Session, p.HostSession), terminal
	}
	t, err := plat.Machine.Processes()
	if err != nil {
		return "unknown (no process table: " + err.Error() + ")", terminal
	}
	for _, p := range t.Ancestors(os.Getpid()) {
		if slices.Contains(agentComms, p.Comm) {
			return fmt.Sprintf("%s (pid %d)", p.Comm, p.PID), terminal
		}
	}
	return "", terminal
}

// answerChannel is the channel an answer me records came through and, for
// one not the person's own, who relayed it. On the central instance me is
// the verified caller: an agent that called as its person names itself.
func (a *app) answerChannel(me state.Party, via string) (channel, relay string) {
	if a.central {
		switch {
		case me.Person == "" || !strings.EqualFold(me.Name, me.Person):
			return channelSession, cmp.Or(me.Name, "an unnamed caller")
		case via == viaSlack:
			return channelSlack, ""
		}
		return channelCentral, ""
	}
	session, terminal := answerSeat(a)
	switch {
	case session != "":
		return channelSession, cmp.Or(me.Name, session)
	case !terminal:
		return channelScript, cmp.Or(me.Name, "a script")
	}
	return channelTerminal, ""
}

// attribution is who gave rec's answer, as the asking session is told.
func (r *recordedAnswer) attribution() string {
	if r.RelayedBy == "" {
		return r.By
	}
	return r.By + " (relayed by " + r.RelayedBy + ")"
}

// attribution is who gave an's answer, and who relayed one not the
// person's own.
func (an answered) attribution() string {
	if an.Via != channelSession && an.Via != channelScript {
		return an.For
	}
	return an.For + " (relayed by " + partyName(an.Recorder) + ")"
}

// shaWord identifies a commit named in a guide dialog.
var shaWord = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

package cmd

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
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

// answerFresh is how old an answer verify still takes as the person's
// word for what it concerns.
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

// answerQuery is the answer verify looks for: a note's, else the newest on
// a thread; Head is the commit the answer must approve, "" for any.
type answerQuery struct {
	Note  int
	About string
	Head  string
}

// newestAnswer is the newest answer of evs q asks for.
func newestAnswer(evs []state.Event, q answerQuery) (answered, bool) {
	for _, e := range slices.Backward(evs) {
		an, ok := parseAnswered(e)
		switch {
		case !ok:
		case q.Note != 0 && an.ID == q.Note, q.Note == 0 && strings.EqualFold(an.About, q.About):
			return an, true
		}
	}
	return answered{}, false
}

// shaWord is a commit named in words: 7 to 40 hex digits on their own.
var shaWord = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// unverified is why an is not the person's own answer to q through an
// authenticated channel on a store central or not, fresh at now; "" when it
// is.
func (an answered) unverified(q answerQuery, central bool, now time.Time) string {
	rec := an.Recorder
	switch an.Via {
	case channelTerminal:
		if central {
			return "it names the terminal on the central instance, which records none: a forged attribution"
		}
		if rec.Session != "" || rec.HostSession != "" {
			return fmt.Sprintf("it names the terminal, but %s recorded it from a session: a forged attribution", partyName(rec))
		}
	case channelCentral, channelSlack:
		if !central || rec.Person == "" || !strings.EqualFold(rec.Person, an.For) || !strings.EqualFold(rec.Name, rec.Person) {
			return fmt.Sprintf("it names %s, but %s on this log is not %s's verified call: a forged attribution", an.Via, partyName(rec), an.For)
		}
	case channelSession:
		return fmt.Sprintf("%s relayed it from an agent session: not %s's own answer", partyName(rec), an.For)
	case channelScript:
		return fmt.Sprintf("%s recorded it without a terminal: not %s's own answer", partyName(rec), an.For)
	default:
		return fmt.Sprintf("it came via %s, recorded before beekeeper recorded the channel: unverifiable", an.Via)
	}
	if q.About != "" && !strings.EqualFold(an.About, q.About) {
		return fmt.Sprintf("it concerns %s, not %s", an.About, q.About)
	}
	if age := now.Sub(an.At); age > answerFresh {
		return fmt.Sprintf("it is %s old, older than %s", dur(age), shortDur(answerFresh))
	}
	if q.Head == "" {
		return ""
	}
	named := shaWord.FindAllString(an.Question+"\n"+an.Answer, -1)
	if len(named) == 0 {
		return fmt.Sprintf("neither the question nor the answer names a commit: it approves no head, %s asked", q.Head)
	}
	for _, s := range named {
		if strings.HasPrefix(strings.ToLower(q.Head), strings.ToLower(s)) {
			return ""
		}
	}
	return fmt.Sprintf("it names %s, not the head %s", strings.Join(named, ", "), q.Head)
}

// attribution is who gave an's answer, and who relayed one not the
// person's own.
func (an answered) attribution() string {
	if an.Via != channelSession && an.Via != channelScript {
		return an.For
	}
	return an.For + " (relayed by " + partyName(an.Recorder) + ")"
}

// strength is what makes an's channel the person's: what a reader of
// verify's output weighs.
func (an answered) strength() string {
	switch an.Via {
	case channelCentral, channelSlack:
		return "the central instance's verified identity " + an.Recorder.Person
	case channelTerminal:
		return "the desk's own login: a terminal outside every agent session"
	}
	return ghNone
}

// verifiedAnswer is answer verify's account of one answer.
type verifiedAnswer struct {
	About    string    `json:"about"`
	Note     int       `json:"note,omitempty"`
	Question string    `json:"question,omitempty"`
	By       string    `json:"by"`
	Channel  string    `json:"channel"`
	Recorder string    `json:"recordedBy"`
	At       time.Time `json:"at"`
	Answer   string    `json:"answer"`
	Verified bool      `json:"verified"`
	Why      string    `json:"why,omitempty"`
	Strength string    `json:"authenticatedBy,omitempty"`
}

func (a *app) answerVerifyCmd() *cobra.Command {
	var q answerQuery
	var ref string
	c := &cobra.Command{
		Use:   "verify --note <id> | --ref <owner/repo#n> [--head <sha>]",
		Short: "Show the person's recorded answer and whether it is their own, for the session it concerns",
		Long: `Print the recorded answer word for word: the note's (--note), else the
newest about the thread (--ref), who gave it, the channel it came through,
who recorded it and when. Exit 0 only when it is the person's own answer
through an authenticated channel, about --ref when given, at most ` + shortDur(answerFresh) + `
old and, with --head, naming that commit (7 to 40 hex digits of it) in its
note's question or its words; else exit 3 with the reason.

beekeeper sets the channel from where answer runs, never from a flag:
central and slack are the central instance's note_answer as the caller's
verified email, through the person's Slack decision or their own call;
terminal is the desk's own login, a terminal on stdin with no agent
session in the environment or among the process's ancestors; session is
an answer recorded from inside an agent session (the guide's relay, for
one) and script one recorded without a terminal: neither is ever the
person's own. A terminal is the desk's login, not an identity: a process
of the same user that hides its session can forge it, as it can write the
log; central and slack carry the identity the central instance checked.

A session that needs the person's word for a step (a merge) runs it and
quotes its output there instead of asking the person to type in its
conversation.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if ref != "" {
				r, ok := parseRef(ref)
				if !ok {
					return usageErr("--ref %q: an issue or pull request is owner/repo#n or its URL", ref)
				}
				q.About = refName(r)
			}
			if q.Note == 0 && q.About == "" {
				return usageErr("which answer: --note <id> or --ref <owner/repo#n>")
			}
			return a.verifyAnswer(q)
		},
	}
	c.Flags().IntVar(&q.Note, "note", 0, "the note the answer answered")
	c.Flags().StringVar(&ref, "ref", "", "the issue or pull request the answer concerns")
	c.Flags().StringVar(&q.Head, "head", "", "the commit the answer must approve")
	return c
}

// verifyAnswer prints the answer q asks for and refuses one not verified.
func (a *app) verifyAnswer(q answerQuery) error {
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded })
	if err != nil {
		return err
	}
	an, ok := newestAnswer(evs, q)
	if !ok {
		if q.Note != 0 {
			return refused("no answer to note #%d is recorded (beekeeper log --verb %s)", q.Note, answerRecorded)
		}
		return refused("no answer about %s is recorded (beekeeper log --verb %s)", q.About, answerRecorded)
	}
	why := an.unverified(q, a.central, a.now)
	v := verifiedAnswer{About: an.About, Note: an.ID, Question: an.Question, By: an.For, Channel: an.Via,
		Recorder: partyName(an.Recorder), At: an.At, Answer: an.Answer, Verified: why == "", Why: why}
	if v.Verified {
		v.Strength = an.strength()
	}
	if a.json {
		if err := a.printJSON(v); err != nil {
			return err
		}
	} else if err := a.printVerified(v); err != nil {
		return err
	}
	if !v.Verified {
		return refused("not the person's own answer: %s", why)
	}
	return nil
}

func (a *app) printVerified(v verifiedAnswer) error {
	var b strings.Builder
	if v.Note != 0 {
		fmt.Fprintf(&b, "note #%d on %s: %q\n", v.Note, v.About, v.Question)
	}
	fmt.Fprintf(&b, "answer on %s by %s via %s, recorded by %s at %s (%s ago):\n",
		v.About, v.By, v.Channel, v.Recorder, v.At.UTC().Format(time.RFC3339), ago(a.now, v.At))
	for line := range strings.Lines(v.Answer) {
		b.WriteString("> " + strings.TrimRight(line, "\n") + "\n")
	}
	if v.Verified {
		fmt.Fprintf(&b, "verified: %s's own answer, authenticated by %s\n", v.By, v.Strength)
	} else {
		fmt.Fprintf(&b, "not verified: %s\n", v.Why)
	}
	_, err := fmt.Fprint(a.out, b.String())
	return err
}

// answerAuthority is what a merge of ref names of the person's word: the
// newest verified answer about it, "" for none.
func (a *app) answerAuthority(ref string, now time.Time) string {
	q := answerQuery{About: ref}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == answerRecorded })
	if err != nil {
		return ""
	}
	an, ok := newestAnswer(evs, q)
	if !ok || an.unverified(q, a.central, now) != "" {
		return ""
	}
	if an.ID != 0 {
		return fmt.Sprintf(", on %s's answer to note #%d via %s", an.For, an.ID, an.Via)
	}
	return fmt.Sprintf(", on %s's answer via %s at %s", an.For, an.Via, an.At.UTC().Format(time.RFC3339))
}

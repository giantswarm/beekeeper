package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	verifyHead  = "abc1234def5678abc1234def5678abc1234def56"
	verifyEmail = "pat@example.com"
	verifyGuide = "Guide run 40"
	flagHead    = "--head"
	flagNote    = "--note"
)

// verifyNote files note 1 for the person on privateThread, its question
// naming the head it approves.
func verifyNote(t *testing.T, a *app) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes, state.Note{ID: 1, For: notePerson, Kind: noteDecision, By: state.Party{Session: "w1", Name: noteFiler},
			Text: "Merge o/private#7 at abc1234?", Refs: []string{privateThread}})
		st.NextNote = 1
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// stubSeat puts the command in session (none: "") with or without a
// terminal on stdin for the test.
func stubSeat(t *testing.T, session string, terminal bool) {
	t.Helper()
	was := answerSeat
	answerSeat = func(*app) (string, bool) { return session, terminal }
	t.Cleanup(func() { answerSeat = was })
}

// The person's answer typed at their terminal verifies: for its note, its
// thread and the head its question names; not for another thread or head.
func TestTheTerminalsAnswerVerifies(t *testing.T) {
	a, out := answerApp(t)
	stubWakes(t)
	verifyNote(t, a)
	if err := runAnswer(a, privateThread, flagNote, "1", "merge it"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runAnswer(a, "verify", flagNote, "1"); err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	for _, want := range []string{`note #1 on o/private#7: "Merge o/private#7 at abc1234?"`, "answer on o/private#7 by Pat via terminal, recorded by " + agentOne, "> merge it\n",
		"verified: Pat's own answer, authenticated by the desk's own login"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, args := range [][]string{{flagNote, "1", flagHead, verifyHead}, {flagRef, "https://github.com/o/private/pull/7", flagHead, "ABC1234"}} {
		if err := runAnswer(a, append([]string{"verify"}, args...)...); err != nil {
			t.Errorf("verify %q: %v", args, err)
		}
	}
	for want, args := range map[string][]string{
		"it names abc1234, not the head 0123456":            {flagNote, "1", flagHead, "0123456789"},
		"it concerns o/private#7, not " + unknownThread:     {flagNote, "1", flagRef, unknownThread},
		"no answer about " + unknownThread + " is recorded": {flagRef, unknownThread},
		"no answer to note #2 is recorded":                  {flagNote, "2"},
	} {
		if err := runAnswer(a, append([]string{"verify"}, args...)...); Code(err) != ExitRefused || !strings.Contains(err.Error(), want) {
			t.Errorf("verify %q: %v, want %q", args, err, want)
		}
	}
	if err := runAnswer(a, "verify"); Code(err) != ExitUsage {
		t.Errorf("verify without a query: %v", err)
	}
	// An answer whose question and words name no commit approves no head.
	if err := runAnswer(a, "o/r#9", "go ahead"); err != nil {
		t.Fatal(err)
	}
	if err := runAnswer(a, "verify", flagRef, "o/r#9", flagHead, verifyHead); Code(err) != ExitRefused || !strings.Contains(err.Error(), "approves no head") {
		t.Errorf("no head named: %v", err)
	}
	if err := runAnswer(a, "verify", flagRef, "o/r#9"); err != nil {
		t.Errorf("without --head: %v", err)
	}
}

// An answer recorded inside an agent session, the guide's relay, is told
// and verified as relayed, never as the person's; so is one without a
// terminal.
func TestARelayedAnswerIsNeverThePersons(t *testing.T) {
	for name, c := range map[string]struct {
		session  string
		terminal bool
		why      string
	}{
		"an agent session": {verifyGuide, true, "Agent one relayed it from an agent session: not Pat's own answer"},
		"a script":         {"", false, "Agent one recorded it without a terminal: not Pat's own answer"},
	} {
		t.Run(name, func(t *testing.T) {
			a, out := answerApp(t)
			woke := stubWakes(t)
			stubSeat(t, c.session, c.terminal)
			verifyNote(t, a)
			if err := runAnswer(a, privateThread, flagNote, "1", "merge it"); err != nil {
				t.Fatal(err)
			}
			if len(*woke) != 1 || !strings.HasSuffix((*woke)[0], "NOTE ANSWERED #1 by Pat (relayed by Agent one): merge it (on o/private#7)") {
				t.Errorf("woke %q", *woke)
			}
			out.Reset()
			if err := runAnswer(a, "verify", flagNote, "1"); Code(err) != ExitRefused || !strings.Contains(err.Error(), c.why) {
				t.Fatalf("verify: %v", err)
			}
			if !strings.Contains(out.String(), "> merge it\nnot verified: "+c.why) {
				t.Errorf("output %q", out)
			}
			if s := a.answerAuthority(privateThread, time.Now()); s != "" {
				t.Errorf("a merge names %q", s)
			}
		})
	}
}

// centralApp is answerApp as the central instance calls a verb: as the
// caller's verified email, or as an agent naming itself.
func centralApp(t *testing.T, name string) *app {
	t.Helper()
	a, _ := answerApp(t)
	a.central, a.as = true, name
	a.cfg.Identity.Person = verifyEmail
	return a
}

// The central instance's answers verify as the verified email: its own
// call and its Slack decision; an agent calling it as the person is a
// relay.
func TestCentralAnswersVerifyAsTheVerifiedEmail(t *testing.T) {
	for name, c := range map[string]struct {
		as, via, channel, why string
	}{
		"its own call":   {verifyEmail, viaCLI, channelCentral, ""},
		"Slack":          {verifyEmail, viaSlack, channelSlack, ""},
		"an agent as it": {agentOne, viaCLI, channelSession, "pat@example.com/Agent one relayed it from an agent session"},
	} {
		t.Run(name, func(t *testing.T) {
			a := centralApp(t, c.as)
			stubWakes(t)
			stubSeat(t, verifyGuide, false) // the central instance's own seat counts for nothing
			if err := runAnswer(a, privateThread, "--via", c.via, "merge abc1234"); err != nil {
				t.Fatal(err)
			}
			got, _ := a.answeredSince(time.Time{})
			if len(got) != 1 || got[0].Via != c.channel || got[0].For != verifyEmail {
				t.Fatalf("answers %+v", got)
			}
			err := runAnswer(a, "verify", flagRef, privateThread, flagHead, verifyHead)
			if c.why == "" && err != nil || c.why != "" && (Code(err) != ExitRefused || !strings.Contains(err.Error(), c.why)) {
				t.Errorf("verify: %v, want %q", err, c.why)
			}
		})
	}
}

// An attribution the log's recorder contradicts is forged, a stale answer
// and one from before channels are unverifiable.
func TestForgedStaleAndOldAnswersDoNotVerify(t *testing.T) {
	for name, c := range map[string]struct {
		central bool
		by      state.Party
		via     string
		ago     time.Duration
		why     string
	}{
		"a session naming the terminal": {false, state.Party{Session: "s1", Name: verifyGuide}, channelTerminal, 0, "it names the terminal, but Guide run 40 recorded it from a session: a forged attribution"},
		"the terminal on central":       {true, state.Party{Name: verifyEmail, Person: verifyEmail}, channelTerminal, 0, "a forged attribution"},
		"central on a desk":             {false, state.Party{Name: verifyGuide}, channelCentral, 0, "it names central, but Guide run 40 on this log is not Pat's verified call"},
		"Slack by another email":        {true, state.Party{Name: "mallory@example.com", Person: "mallory@example.com"}, channelSlack, 0, "a forged attribution"},
		"from before channels":          {false, state.Party{Name: verifyGuide}, viaCLI, 0, "it came via cli, recorded before beekeeper recorded the channel"},
		"a stale one":                   {false, state.Party{Name: notePerson}, channelTerminal, 25 * time.Hour, "older than 24h"},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := answerApp(t)
			a.central = c.central
			a.now = time.Now().Add(c.ago)
			if err := a.store.Log(event(c.by, answerRecorded, "%s answered by %q via %s: merge it", privateThread, notePerson, c.via)); err != nil {
				t.Fatal(err)
			}
			if err := runAnswer(a, "verify", flagRef, privateThread); Code(err) != ExitRefused || !strings.Contains(err.Error(), c.why) {
				t.Errorf("verify: %v, want %q", err, c.why)
			}
		})
	}
}

// A merge through the gate names the person's verified answer about its
// pull request.
func TestAMergeNamesTheVerifiedAnswer(t *testing.T) {
	a, _ := answerApp(t)
	stubWakes(t)
	verifyNote(t, a)
	if s := a.answerAuthority(privateThread, time.Now()); s != "" {
		t.Errorf("no answer yet, a merge names %q", s)
	}
	if err := runAnswer(a, privateThread, flagNote, "1", "merge it"); err != nil {
		t.Fatal(err)
	}
	if s, want := a.answerAuthority(privateThread, time.Now()), ", on Pat's answer to note #1 via terminal"; s != want {
		t.Errorf("a merge names %q, want %q", s, want)
	}
	if s := a.answerAuthority(privateThread, time.Now().Add(25*time.Hour)); s != "" {
		t.Errorf("a stale answer, a merge names %q", s)
	}
}

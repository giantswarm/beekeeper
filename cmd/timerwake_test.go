package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	parkedCentralNote = "central#3"
	parkedHoldWait    = "hold fixture-hold"
	parkedAlertWait   = "alert:ParkFixtureAlert"
)

// Replay eight hours of watch ticks without a model: every attempted turn
// crosses wakeOwner, so zero deliveries means zero model calls or cost.
// Include an old repeating reminder, a successful unrelated probe, a failed
// probe reaching its deadline, and idle/usage notices. Only the note's
// answer may resume the agent.
func TestPersonParkEightHoursWithoutTurns(t *testing.T) {
	w := parkingWatch(t, nil)
	w.cfg.Agents.AutoResume = true
	w.cfg.Guide.Person = personTimo
	woke := stubWakes(t)
	fakeRead(t, func(cmd string) ([]byte, error) {
		if cmd == probeHolds {
			return nil, nil
		}
		return nil, fmt.Errorf("probe still waiting")
	})
	if err := park(t, w, "--on", "#3", "Alex's answer"); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Session = "parked-eight-hours"
		st.Owed = []state.Owed{{Owner: st.Agents[0].Party, Line: "completed unrelated command", Since: relayNow}}
		st.Timers = []state.Timer{
			{ID: 1, Due: relayNow, Wake: agentOne, What: "old reminder", Repeat: state.RepeatDaily},
			{ID: 2, Due: relayNow, Wake: agentOne, What: "unrelated probe", Probe: probeHolds},
			{ID: 3, Due: relayNow, Wake: agentOne, What: "probe timeout", Probe: probeFails, Until: relayNow.Add(time.Hour)},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed <= 8*time.Hour; elapsed += 5 * time.Minute {
		w.now = relayNow.Add(elapsed)
		w.pending(t.Context(), nil)
		w.timerActs.Wait()
		st, err := w.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		w.stoppedAgents(st, nil)
		if len(st.Owed) != 1 {
			t.Fatalf("command result was lost: %+v", st.Owed)
		}
		w.payOwed(t.Context(), st.Owed[0])
		if err := w.quietWake(t.Context(), watchParty, agentOne, "an unrelated note defaulted"); err != nil {
			t.Fatal(err)
		}
		w.tellBusyWorkersUsageHigh(t.Context(), st, []*claude.Session{{Name: agentOne}}, &usageView{High: true}, "park-usage.json", nil)
		if len(*woke) != 0 || st.Agents[0].Park == nil || !st.Agents[0].Park.Resumable.IsZero() {
			t.Fatalf("after %s: turns=%v, park=%+v", elapsed, *woke, st.Agents[0].Park)
		}
	}
	answer(t, w)
	w.pending(context.Background(), nil)
	w.pending(context.Background(), nil)
	if len(*woke) != 1 || !strings.Contains((*woke)[0], parkAnswer) {
		t.Fatalf("answer should start exactly one turn: %v", *woke)
	}
	t.Log("eight simulated hours: zero model turns before the answer; exactly one after")
}

func TestTimerRechecksTheCurrentParkAtDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, on, waits, when string
		offRoster             bool
		guide                 bool
		wantWake              bool
	}{
		{name: "guide note", on: "#3", guide: true},
		{name: "guide external wait", guide: true, wantWake: true},
		{name: "plain external wait", waits: "deployment ready", wantWake: true},
		{name: "person without note", waits: "Alex's answer"},
		{name: "local note", on: "#3"},
		{name: "central note", on: parkedCentralNote},
		{name: "off roster note by id", on: "#3", offRoster: true},
		{name: "matching pull", on: claimIssue, when: prMerged7, wantWake: true},
		{name: "matching checks", on: claimIssue, when: "pr-checks o/r#7", wantWake: true},
		{name: "another pull", on: claimIssue2, when: prMerged7},
		{name: "lease wait", on: "lease agentlab-1"},
		{name: verbHold, on: parkedHoldWait},
		{name: "alert", on: parkedAlertWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _ := notifyingWatch(t, t.TempDir(), false)
			w.cfg.Guide.Person = personTimo
			woke := stubWakes(t)
			q := agentOne
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				p := state.Park{By: state.Party{Name: agentOne, Session: "park-fixture-session"}, At: relayNow, On: tc.on, Waits: tc.waits}
				if tc.offRoster {
					st.Parks = []state.Park{p}
					q = p.By.Session
				} else {
					st.Agents = []state.Agent{{Party: p.By, Task: parkTask, Park: &p}}
				}
				if tc.guide {
					guideRole.set(st, state.Role{Holder: &state.Supervisor{Party: p.By}})
					q = theGuide
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w.wakeTimer(t.Context(), timerFire{t: state.Timer{ID: 1, Wake: q, When: tc.when}, reason: "holds"})
			if (len(*woke) == 1) != tc.wantWake {
				t.Fatalf("wake=%v, want delivery=%v", *woke, tc.wantWake)
			}
		})
	}
}

func TestSupervisorLeavesPersonParkAsleep(t *testing.T) {
	r := newLoopRig(t)
	start := time.Now()
	r.park(t, "#3", start)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = []state.Note{{ID: 3, For: loopPerson, By: state.Party{Name: loopWorker}, Text: askedQ}}
		st.Agents[0].Park.ReviewHead = ""
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed <= 8*time.Hour; elapsed += time.Hour {
		r.a.now = start.Add(elapsed)
		r.pass(t)
		if wake := r.called(agentsName, agentWakeName); wake != nil {
			t.Fatalf("after %s supervisor woke a person wait: %v", elapsed, wake)
		}
	}
}

func TestPersonMessageStillReachesParkedSession(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if err := park(t, w, "--on", "#3", "Alex's answer"); err != nil {
		t.Fatal(err)
	}
	if err := w.quietWake(t.Context(), state.Party{Name: personTimo}, agentOne, parkAnswer); err != nil {
		t.Fatal(err)
	}
	if len(*woke) != 1 || !strings.Contains((*woke)[0], parkAnswer) {
		t.Fatalf("person's explicit message was suppressed: %v", *woke)
	}
}

func TestMailboxDefaultLeavesPersonParkAsleep(t *testing.T) {
	const target = "parked-mail-worker"
	address := "local:" + mailHost + "/" + target
	mb := &mailboxStandIn{queue: []mailbox.Delivery{
		noteAnswerMail(1, "note-4-default", address, "NOTE DEFAULTED #4: keep the default"),
		slackReply(t, 2, address, parkAnswer),
	}}
	w, sent, _ := mailboxWatch(t, mb)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: target}, Task: parkTask, Park: &state.Park{On: parkedCentralNote, At: relayNow}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.drainMailbox(t.Context())
	w.drainMailbox(t.Context())
	if len(*sent) != 1 || !strings.Contains((*sent)[0], parkAnswer) || len(mb.acked) != 2 {
		t.Fatalf("default should stay asleep, person's message delivered once: sent=%v acked=%v", *sent, mb.acked)
	}
}

func TestCommandOutcomeWaitsForPersonPark(t *testing.T) {
	for _, on := range []string{"#3", parkedCentralNote} {
		t.Run(on, func(t *testing.T) {
			w, _, _ := notifyingWatch(t, t.TempDir(), false)
			woke := stubWakes(t)
			owner := state.Party{Name: agentOne, Session: "owed-park-fixture"}
			o := state.Owed{Owner: owner, Line: "old command completed", Since: relayNow}
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: owner, Task: parkTask, Park: &state.Park{On: on, At: relayNow}}}
				st.Owed = []state.Owed{o}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w.payOwed(t.Context(), o)
			st, err := w.store.Read()
			if err != nil || len(*woke) != 0 || len(st.Owed) != 1 {
				t.Fatalf("pending park: deliveries=%v state=%+v error=%v", *woke, st, err)
			}
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park = nil
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w.payOwed(t.Context(), o)
			st, err = w.store.Read()
			if err != nil || len(*woke) != 1 || len(st.Owed) != 0 {
				t.Fatalf("ended park: deliveries=%v state=%+v error=%v", *woke, st, err)
			}
		})
	}
}

// A gated wait's own result is a wake condition, including red CI. A plain
// PR park must still hear it; waiting for a person's review is different.
func TestCommandOutcomeCanCompletePullRequestWait(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	woke := stubWakes(t)
	owner := state.Party{Name: agentOne, Session: "ci-wait-fixture"}
	o := state.Owed{Owner: owner, Line: "devctl pr wait o/r 7 exit 1: red", Since: relayNow}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: owner, Task: parkTask, Park: &state.Park{On: claimIssue, At: relayNow, Waits: "CI completion"}}}
		st.Owed = []state.Owed{o}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.payOwed(t.Context(), o)
	if len(*woke) != 1 || !strings.Contains((*woke)[0], o.Line) {
		t.Fatalf("the CI result did not reach its waiting caller: %v", *woke)
	}
}

func TestTimerGuideRoleFollowsRelay(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	woke := stubWakes(t)
	timer, err := w.timerFrom(timerSpec{wake: theGuide}, relayNow)
	if err != nil || timer.Wake != theGuide {
		t.Fatalf("role timer: %+v, %v", timer, err)
	}
	for _, name := range []string{"previous guide", "successor guide"} {
		if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
			holder := state.Party{Name: name, Session: name + "-session"}
			st.Agents = append(st.Agents, state.Agent{Party: holder})
			guideRole.set(st, state.Role{Holder: &state.Supervisor{Party: holder, Since: relayNow}})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		w.wakeTimer(t.Context(), timerFire{t: timer, reason: "due"})
		if len(*woke) == 0 || !strings.Contains((*woke)[len(*woke)-1], name+"-session") {
			t.Fatalf("timer did not reach current holder: %v", *woke)
		}
	}
	if len(*woke) != 2 {
		t.Fatalf("deliveries: %v", *woke)
	}
}

func TestTimerSupervisorExplainsServiceRole(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	if _, err := w.timerFrom(timerSpec{wake: resumer}, relayNow); err != errSupervisorMessage {
		t.Fatalf("want supervisor service explanation, got %v", err)
	}
}

func TestTimerGuideWithoutHolderDoesNotWake(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	woke := stubWakes(t)
	w.wakeTimer(t.Context(), timerFire{t: state.Timer{Wake: theGuide}, reason: "due"})
	if len(*woke) != 0 {
		t.Fatalf("woke without a holder: %v", *woke)
	}
}

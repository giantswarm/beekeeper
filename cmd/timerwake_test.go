package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
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
	watchApp(w.Host).cfg.Agents.AutoResume = true
	watchApp(w.Host).cfg.Guide.Person = personTimo
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
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
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
		watchApp(w.Host).now = relayNow.Add(elapsed)
		w.Pending(t.Context(), nil)
		w.TimerActs.Wait()
		st, err := watchApp(w.Host).store.Read()
		if err != nil {
			t.Fatal(err)
		}
		w.StoppedAgents(st, nil)
		if len(st.Owed) != 1 {
			t.Fatalf("command result was lost: %+v", st.Owed)
		}
		watchApp(w.Host).payOwed(t.Context(), st.Owed[0])
		if err := watchApp(w.Host).quietWake(t.Context(), watch.WatchParty, agentOne, "an unrelated note defaulted"); err != nil {
			t.Fatal(err)
		}
		w.TellBusyWorkersUsageHigh(t.Context(), st, []*claude.Session{{Name: agentOne}}, &capacity.UsageView{High: true}, "park-usage.json", nil)
		if len(*woke) != 0 || st.Agents[0].Park == nil || !st.Agents[0].Park.Resumable.IsZero() {
			t.Fatalf("after %s: turns=%v, park=%+v", elapsed, *woke, st.Agents[0].Park)
		}
	}
	answer(t, w)
	w.Pending(context.Background(), nil)
	w.Pending(context.Background(), nil)
	assertFreshQueue(t, w, *woke, parkAnswer)
	t.Log("eight simulated hours: no transcript resumes; answer retained for a fresh worker")

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
		{name: "own timer probe review", waits: "Timer 1: proof review running", wantWake: true},
		{name: "own timer hash", waits: "timer #1: running", wantWake: true},
		{name: "other timer", waits: "Timer 10: running"},
		{name: "timer cannot override note", on: "#3", waits: "Timer 1: running"},
		{name: "timer cannot override pull", on: claimIssue, waits: "Timer 1: running"},
		{name: "off roster own timer", waits: "Timer 1: review proof running", offRoster: true, wantWake: true},
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
			watchApp(w.Host).cfg.Guide.Person = personTimo
			woke := stubWakes(t)
			q := agentOne
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				p := state.Park{By: state.Party{Name: agentOne, Session: "park-fixture-session"}, At: relayNow, On: tc.on, Waits: tc.waits}
				if tc.offRoster {
					st.Parks = []state.Park{p}
					q = p.By.Session
				} else {
					st.Agents = []state.Agent{{Party: p.By, Task: parkTask, Park: &p}}
				}
				if tc.guide {
					guideRole.Set(st, state.Role{Holder: &state.Supervisor{Party: p.By}})
					q = theGuide
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			fakeRead(t, func(string) ([]byte, error) { return ciAnswer("OPEN", ciLintRed), nil })
			w.WakeTimer(t.Context(), timerFire{T: state.Timer{ID: 1, Wake: q, When: tc.when}, Reason: "holds"})
			st, _ := watchApp(w.Host).store.Read()
			delivered := len(*woke) == 1
			for _, ag := range st.Agents {
				delivered = delivered || ag.Park != nil && len(ag.Park.Continuation) == 1
			}
			if delivered != tc.wantWake {
				t.Fatalf("wake=%v, want delivery=%v", *woke, tc.wantWake)
			}
			if !tc.guide && !tc.offRoster && len(*woke) > 0 {
				t.Fatal("parked worker resumed")
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
	if err := watchApp(w.Host).quietWake(t.Context(), state.Party{Name: personTimo}, agentOne, parkAnswer); err != nil {
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
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: target}, Task: parkTask, Park: &state.Park{On: parkedCentralNote, At: relayNow}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.DrainMailbox(t.Context())
	w.DrainMailbox(t.Context())
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
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: owner, Task: parkTask, Park: &state.Park{On: on, At: relayNow}}}
				st.Owed = []state.Owed{o}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			watchApp(w.Host).payOwed(t.Context(), o)
			st, err := watchApp(w.Host).store.Read()
			if err != nil || len(*woke) != 0 || len(st.Owed) != 1 {
				t.Fatalf("pending park: deliveries=%v state=%+v error=%v", *woke, st, err)
			}
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park = nil
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			watchApp(w.Host).payOwed(t.Context(), o)
			st, err = watchApp(w.Host).store.Read()
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
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: owner, Task: parkTask, Park: &state.Park{On: claimIssue, At: relayNow, Waits: "CI completion"}}}
		st.Owed = []state.Owed{o}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	watchApp(w.Host).payOwed(t.Context(), o)
	if len(*woke) != 1 || !strings.Contains((*woke)[0], o.Line) {
		t.Fatalf("the CI result did not reach its waiting caller: %v", *woke)
	}
}

func TestTimerGuideRoleFollowsRelay(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	woke := stubWakes(t)
	timer, err := watchApp(w.Host).timerFrom(timerSpec{wake: theGuide}, relayNow)
	if err != nil || timer.Wake != theGuide {
		t.Fatalf("role timer: %+v, %v", timer, err)
	}
	for _, name := range []string{"previous guide", "successor guide"} {
		if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
			holder := state.Party{Name: name, Session: name + "-session"}
			st.Agents = append(st.Agents, state.Agent{Party: holder})
			guideRole.Set(st, state.Role{Holder: &state.Supervisor{Party: holder, Since: relayNow}})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		w.WakeTimer(t.Context(), timerFire{T: timer, Reason: paramDue})
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
	if _, err := watchApp(w.Host).timerFrom(timerSpec{wake: resumer}, relayNow); err != errSupervisorMessage {
		t.Fatalf("want supervisor service explanation, got %v", err)
	}
}

func TestTimerGuideWithoutHolderDoesNotWake(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	woke := stubWakes(t)
	w.WakeTimer(t.Context(), timerFire{T: state.Timer{Wake: theGuide}, Reason: paramDue})
	if len(*woke) != 0 {
		t.Fatalf("woke without a holder: %v", *woke)
	}
}

// A skip or harness failure must leave another attempt, rather than a park
// with its only timer gone. Run settlement as well as delivery to cover the
// claim removing the timer before the park is checked again.
func TestTimerUndeliveredWakeRetries(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("harness failure=", fail), func(t *testing.T) {
			for _, repeat := range []string{"", state.RepeatDaily} {
				t.Run("repeat="+repeat, func(t *testing.T) {
					w, _, _ := notifyingWatch(t, t.TempDir(), false)
					w.Clock = timerRetryClock{w: w}
					watchApp(w.Host).cfg.Guide.Person = personTimo
					old := wakeOwner
					calls := 0
					wakeOwner = func(*app, context.Context, state.Party, string, string, string) error {
						calls++
						if fail && calls == 1 {
							return errors.New("harness unavailable")
						}
						return nil
					}
					t.Cleanup(func() { wakeOwner = old })
					if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
						p := &state.Park{At: relayNow, On: "#3", Waits: verbAnswer}
						if fail {
							p.On, p.Waits = "", "Timer 42: proof running"
						}
						st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Park: p}}
						st.Timers = []state.Timer{{ID: 42, Due: relayNow, Wake: agentOne, Repeat: repeat, What: timerProofFinished}}
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
					fire := func() {
						t.Helper()
						var fires []timerFire
						if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
							_, events, f, _ := watch.SettleTimers(st, nil, watchApp(w.Host).now)
							fires = f
							return events, nil
						}); err != nil {
							t.Fatal(err)
						}
						for _, f := range fires {
							w.WakeTimer(t.Context(), f)
						}
					}
					fire()
					st, err := watchApp(w.Host).store.Read()
					if err != nil || len(st.Timers) != 1 || !st.Timers[0].Due.Equal(relayNow) {
						t.Fatalf("undelivered occurrence lost: %+v, %v", st, err)
					}
					if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
						st.Agents[0].Park.On, st.Agents[0].Park.Waits = "", "Timer 42: proof running"
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
					// Delivery retries retain the occurrence but wait one interval.
					watchApp(w.Host).now = relayNow.Add(timerEvery)
					fire()
					st, err = watchApp(w.Host).store.Read()
					wantCalls := 1
					if fail {
						wantCalls++
					}
					if err != nil || calls != wantCalls || repeat == "" && len(st.Timers) != 0 || repeat != "" && (len(st.Timers) != 1 || !st.Timers[0].Due.After(relayNow)) {
						t.Fatalf("retry did not deliver and settle: calls=%d state=%+v error=%v", calls, st, err)
					}
				})
			}
		})
	}
}

func TestSkippedConditionalTimerStaysOpenPastExpiry(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	w.Clock = timerRetryClock{w: w}
	woke := stubWakes(t)
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Park: &state.Park{On: "#3", Waits: verbAnswer, At: relayNow}}}
		st.Timers = []state.Timer{{ID: 42, Due: relayNow, Wake: agentOne, Probe: probeHolds, Until: relayNow.Add(time.Minute), Expire: true}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	var fires []timerFire
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		_, events, f, _ := watch.SettleTimers(st, map[int]checkResult{42: {Holds: true}}, watchApp(w.Host).now)
		fires = f
		return events, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(fires) != 1 {
		t.Fatalf("expected one fire: %+v", fires)
	}
	w.WakeTimer(t.Context(), fires[0])
	st, err := watchApp(w.Host).store.Read()
	if err != nil || len(st.Timers) != 1 || st.Timers[0].Expire || !st.Timers[0].Checked.Equal(watchApp(w.Host).now) || len(*woke) != 0 {
		t.Fatalf("skip lost retry: %+v error=%v wakes=%v", st, err, *woke)
	}
	watchApp(w.Host).now = relayNow.Add(timerEvery)
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{Waits: "Timer 42: review proof running", At: watchApp(w.Host).now}
		_, events, f, _ := watch.SettleTimers(st, nil, watchApp(w.Host).now)
		fires = f
		return events, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(fires) != 1 || fires[0].TimedOut {
		t.Fatalf("expiry discarded fired wake: %+v", fires)
	}
	w.WakeTimer(t.Context(), fires[0])
	if len(*woke) != 1 {
		t.Fatalf("retry did not wake: %v", *woke)
	}
}

// A one-shot probe may stop holding after its first successful observation.
// Retry the fired wake, rather than waiting for that observation again.
func TestConditionalWakeRetryKeepsFiredOccurrence(t *testing.T) {
	for _, repeat := range []string{"", state.RepeatDaily} {
		t.Run("repeat="+repeat, func(t *testing.T) {
			w, _, _ := notifyingWatch(t, t.TempDir(), false)
			w.Clock = timerRetryClock{w: w}
			woke := stubWakes(t)
			var initial timerFire
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Park: &state.Park{On: "#3", Waits: verbAnswer, At: relayNow}}}
				st.Timers = []state.Timer{{ID: 42, Due: relayNow, Wake: agentOne, Probe: probeHolds, Repeat: repeat}}
				_, events, fires, _ := watch.SettleTimers(st, map[int]checkResult{42: {Holds: true}}, watchApp(w.Host).now)
				if len(fires) != 1 {
					t.Fatalf("expected initial fire: %+v", fires)
				}
				initial = fires[0]
				return events, nil
			}); err != nil {
				t.Fatal(err)
			}
			// The initial occurrence was claimed; delivery is rejected by the note park.
			w.WakeTimer(t.Context(), initial)
			watchApp(w.Host).now = relayNow.Add(timerEvery)
			var fires []timerFire
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park = &state.Park{Waits: "Timer 42: running", At: watchApp(w.Host).now}
				_, events, f, _ := watch.SettleTimers(st, map[int]checkResult{42: {Holds: false}}, watchApp(w.Host).now)
				fires = f
				return events, nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(fires) != 1 || fires[0].Reason != initial.Reason || fires[0].TimedOut {
				t.Fatalf("lost the fired occurrence: %+v", fires)
			}
			w.WakeTimer(t.Context(), fires[0])
			st, err := watchApp(w.Host).store.Read()
			if err != nil || len(*woke) != 1 {
				t.Fatalf("wake not delivered: %v, %v", *woke, err)
			}
			if repeat != "" && (len(st.Timers) != 1 || st.Timers[0].PendingWakeReason != "") {
				t.Fatalf("retry leaked into next occurrence: %+v", st.Timers)
			}
		})
	}
}

// Keep existing simulated tick tests explicit about their delivery clock.
type timerRetryClock struct {
	watch.WallClock
	w *watch.Watcher
}

func (c timerRetryClock) Now() time.Time { return watchApp(c.w.Host).now }

func TestDelayedWakeRetryUsesDeliveryTime(t *testing.T) {
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	clock := newFakeClock(t)
	clock.now = relayNow.Add(4 * time.Minute)
	w.Clock = clock
	timer := state.Timer{ID: 42, Due: relayNow, Wake: agentOne, Every: time.Minute}
	rearmed, err := w.RearmTimer(timerFire{T: timer, Reason: paramDue}, "wake skipped")
	if err != nil || !rearmed {
		t.Fatalf("rearm: %v, %v", rearmed, err)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Timers) != 1 || !st.Timers[0].Checked.Equal(clock.Now()) {
		t.Fatalf("retry timestamp must reflect delivery: %+v", st.Timers)
	}
	if got := fatalTimers(st, clock.Now()); got.overdue != 0 {
		t.Fatalf("fresh delivery reported overdue: %+v", got)
	}
	_, _, fires, _ := watch.SettleTimers(st, nil, clock.Now().Add(30*time.Second))
	if len(fires) != 0 {
		t.Fatal("retry did not wait a full interval")
	}
	_, _, fires, _ = watch.SettleTimers(st, nil, clock.Now().Add(time.Minute))
	if len(fires) != 1 {
		t.Fatal("retry did not fire at the next interval")
	}
}

func TestOwnedExternalWaitTimerIsItsWakeCondition(t *testing.T) {
	owner := state.Party{Name: agentOne, Session: continuationOldSession}
	for _, tc := range []struct {
		name, on, waits, when, repeat string
		by                            state.Party
		want                          bool
	}{
		{name: "proof deadline on pull", on: loopPR, waits: "timer wakes me to verify proof, then close", by: owner, want: true},
		{name: "probe on issue", on: continuationTaskRef, waits: "CI completion timer", by: owner, want: true},
		{name: "review reminder", on: loopPR, waits: deskReviewWait, by: owner, want: true},
		{name: "another worker", on: loopPR, by: state.Party{Name: removeOther}},
		{name: "old session", on: loopPR, by: state.Party{Name: agentOne, Session: "different-session"}},
		{name: "repeating reminder", on: loopPR, by: owner, repeat: state.RepeatDaily},
		{name: "different typed condition", on: loopPR, by: owner, when: "issue-closed o/r#12"},
		{name: paramNote, on: "#3", by: owner},
		{name: holderPerson, on: loopPR, waits: "waiting for the person to answer", by: owner},
		{name: "hold", on: parkedHoldWait, by: owner},
		{name: "lease", on: "lease agentlab-1", by: owner},
		{name: "alert", on: parkedAlertWait, by: owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &state.Park{By: owner, On: tc.on, Waits: tc.waits}
			timer := state.Timer{By: tc.by, When: tc.when, Repeat: tc.repeat}
			if got := watch.TimerSettlesPark(timer, p, personTimo); got != tc.want {
				t.Fatalf("timer settlement = %v, want %v", got, tc.want)
			}
		})
	}
}

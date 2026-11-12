package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/issuemonitor"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const deliveryIntakeFill = "fill"
const deliveryIntakeExistingAgent = "existing delivery"
const deliveryIntakeOtherRepo = "other/repository"

// The reserved slots are at most half the room, rounded down: a done-event
// start takes the last slot, and the log still names the reservation.
func TestReview3366KindNextTakesLastSlotUnderHalfRoomCap(t *testing.T) {
	for _, step := range []string{state.DeliveryFixing, state.DeliveryReviewQueued} {
		t.Run(step, func(t *testing.T) {
			r, busy := fillRig(t)
			old := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == LoopCapacity {
					return deliveryControlCapacity, 0
				}
				return old(args)
			}
			now := r.a.timeNow()
			seed(t, r.a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: deliveryControlPendingRef, Step: step, Agent: holderGone, Started: now.Add(-time.Hour), Since: now}}
			})
			if err := r.a.store.Record(event(state.Party{Name: "completed unrelated worker"}, superloop.VerbDone, "completed unrelated work")); err != nil {
				t.Fatal(err)
			}
			r.pass(t) // settle the unrelated done event; pending work is from an earlier pass
			r.pass(t) // KindNext takes the last slot
			if *busy == 0 || !strings.Contains(r.out.String(), "NEXT RESERVES 1 slots") {
				t.Fatalf("KindNext held by %s reservation: busy=%d calls=%v %s", step, *busy, r.calls, r.out)
			}
		})
	}
}

func TestReview3366FullWIPAllowsExemptLoopItem(t *testing.T) {
	for _, route := range []string{deliveryIntakeFill, superloop.KindNext} {
		t.Run(route, func(t *testing.T) {
			r, busy := fillRig(t)
			r.a.cfg.Delivery.WIPLimit = 1
			r.a.cfg.Board.Order = []config.BoardStep{{Name: "urgent delivery", WIPExempt: true}}
			now := r.a.timeNow()
			seed(t, r.a, func(st *state.State) {
				st.DeliveriesAt = now
				st.StartDelivery(deliveryControlPendingRef, deliveryIntakeExistingAgent, "", now)
			})
			old := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == LoopBoard {
					return deliveryIntakePick(epicTen, true), 0
				}
				return old(args)
			}
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			st, _ := r.a.store.Read()
			if route == deliveryIntakeFill {
				err = l.Fill(t.Context(), st, nil, nil)
			} else {
				_, err = l.StartNext(t.Context(), superloop.Action{Key: "exempt next"})
			}
			if err != nil || *busy != 1 {
				t.Fatalf("exempt %s stranded: busy=%d err=%v calls=%v", route, *busy, err, r.calls)
			}
		})
	}
}

func deliveryIntakePick(ref string, exempt bool) string {
	return fmt.Sprintf(`{"pick":{"ref":%q,"url":%q,"wip_exempt":%t}}`, ref, deliveryURL(ref, false), exempt)
}

func TestReview3366ScopedIntakeRoutesCountAndRejectOrdinary(t *testing.T) {
	for _, route := range []string{deliveryIntakeFill, superloop.KindNext} {
		for _, tc := range []struct {
			name                                                                                                                     string
			configured, repair, nonincident, lifted, expired, wrongRepo, stale, ordinary, releaseDuringRead, liftDuringRead, allowed bool
		}{
			{name: "configured exception", configured: true, allowed: true},
			{name: "stale configured exception", configured: true, stale: true, allowed: true},
			{name: "named incident repair", repair: true, allowed: true},
			{name: "stale named repair", repair: true, stale: true, allowed: true},
			{name: "ordinary full intake"},
			{name: "ordinary stale intake", stale: true},
			{name: "ordinary returned by scoped reader", configured: true, ordinary: true},
			{name: "no fallback when release races read", configured: true, ordinary: true, releaseDuringRead: true},
			{name: "repair cannot pick another issue", repair: true, ordinary: true},
			{name: "repair lifted during discovery", repair: true, liftDuringRead: true},
			{name: "nonincident hold is no exception", repair: true, nonincident: true},
			{name: "lifted repair is no exception", repair: true, lifted: true},
			{name: "expired repair is no exception", repair: true, expired: true},
			{name: "cross repository repair is no exception", repair: true, wrongRepo: true},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				r, busy := fillRig(t)
				r.a.cfg.Delivery.WIPLimit = 1
				r.a.cfg.StateDir = t.TempDir()
				if tc.configured {
					r.a.cfg.Board.Order = []config.BoardStep{{Name: "exceptional intake", WIPExempt: true}}
				}
				now := r.a.timeNow()
				seed(t, r.a, func(st *state.State) {
					st.DeliveriesAt = now
					if tc.stale {
						st.DeliveriesAt = now.Add(-time.Minute)
					}
					st.StartDelivery(deliveryControlPendingRef, deliveryIntakeExistingAgent, "", now)
					if tc.repair {
						h := state.Hold{Target: deliveryFixtureRepo, Except: epicTen, Incident: !tc.nonincident}
						if tc.lifted {
							h.LiftedBy = &state.Party{Name: guideName}
						}
						if tc.expired {
							h.Until = now.Add(-time.Minute)
						}
						if tc.wrongRepo {
							h.Target = deliveryIntakeOtherRepo
						}
						st.Holds = []state.Hold{h}
					}
				})
				if err := issuemonitor.WithState(t.Context(), r.a.issueMonitorDir(), func(s *issuemonitor.State, save func(*issuemonitor.State) error) error {
					s.Queue = []issuemonitor.Pointer{{Ref: idleRef}}
					return save(s)
				}); err != nil {
					t.Fatal(err)
				}
				oldGH := issueMonitorGH
				t.Cleanup(func() { issueMonitorGH = oldGH })
				issueMonitorGH = func(context.Context, ...string) ([]byte, error) {
					t.Error("scoped intake read ordinary monitored queue")
					return nil, errors.New("unexpected monitored read")
				}
				old := r.answer
				boardReads := 0
				r.answer = func(args []string) (string, int) {
					if args[0] == LoopBoard {
						boardReads++
						if !slices.Contains(args, deliveryExemptOnlyFlag) {
							t.Error("full/stale intake used an ordinary board walk")
						}
						if tc.releaseDuringRead {
							seed(t, r.a, func(st *state.State) { st.Deliveries[0].Step = state.DeliveryDone })
						}
						if tc.liftDuringRead {
							seed(t, r.a, func(st *state.State) { st.Holds[0].LiftedBy = &state.Party{Name: guideName} })
						}
						ref := epicTen
						if tc.ordinary {
							ref = idleRef
						}
						return deliveryIntakePick(ref, tc.configured && !tc.ordinary), 0
					}
					out, code := old(args)
					if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
						seed(t, r.a, func(st *state.State) { st.StartDelivery(epicTen, args[2], "", now) })
					}
					return out, code
				}
				l, err := r.a.newSupervisorLoop(false)
				if err != nil {
					t.Fatal(err)
				}
				st, _ := r.a.store.Read()
				if route == deliveryIntakeFill {
					err = l.Fill(t.Context(), st, nil, nil)
				} else {
					_, err = l.StartNext(t.Context(), superloop.Action{Key: "scoped next"})
				}
				if err != nil && !errors.Is(err, ErrLoopLater) {
					t.Fatal(err)
				}
				if (*busy > 0) != tc.allowed {
					t.Fatalf("allowed=%t busy=%d calls=%v", tc.allowed, *busy, r.calls)
				}
				if !tc.configured && (!tc.repair || tc.nonincident || tc.lifted || tc.expired || tc.wrongRepo) && boardReads != 0 {
					t.Fatal("ordinary full intake spent board budget")
				}
				if !l.FillPause.Until.IsZero() {
					t.Fatal("full/stale admission set refill pause")
				}
				st, _ = r.a.store.Read()
				if tc.allowed && readDeliveryWIP(st, 1, time.Second, now).Count != 2 {
					t.Fatalf("exempt start was not counted: %+v", st.Deliveries)
				}
				if err := issuemonitor.WithState(t.Context(), r.a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
					if len(s.Queue) != 1 || s.Queue[0].Ref != idleRef {
						t.Error("ordinary queue consumed")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type deliveryIntakeWriter struct {
	mu sync.Mutex
	io.Writer
}

func (w *deliveryIntakeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Writer.Write(p)
}

func TestReview3366DoneEventDoesNotRaceExistingWork(t *testing.T) {
	for _, step := range []string{state.DeliveryReviewQueued, state.DeliveryFixing} {
		t.Run(step, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.out = &deliveryIntakeWriter{Writer: r.out}
			park := func() {
				r.park(t, loopPR, r.a.timeNow())
				r.started(t, loopWorker, "existing work")
				seed(t, r.a, func(st *state.State) { work := ""; st.Agents[0].Park.PostMerge = &work })
			}
			if step == state.DeliveryFixing {
				park()
				r.pass(t)
			}
			if err := r.a.store.Record(event(state.Party{Name: "unrelated finished worker"}, superloop.VerbDone, "unrelated work complete")); err != nil {
				t.Fatal(err)
			}
			r.pass(t) // settles done; its KindNext is planned on the following pass
			if step == state.DeliveryReviewQueued {
				park()
			} else {
				r.verdict("intake findings", loopHead, superloop.ChangesRequested, "repair required")
			}
			brief := filepath.Join(t.TempDir(), "worker.md")
			if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
				t.Fatal(err)
			}
			r.a.cfg.Supervisor.WorkerBrief = brief
			var existingStarts atomic.Int32
			r.answer = func(args []string) (string, int) {
				if args[0] == LoopCapacity {
					return deliveryControlCapacity, 0
				}
				if args[0] == LoopBoard {
					return deliveryIntakePick(epicTen, false), 0
				}
				if len(args) > 2 && args[0] == agentsName && args[1] == agentStartName {
					existingStarts.Add(1)
					r.started(t, args[2], "resumed existing work")
				}
				return "", 0
			}
			r.pass(t)
			expected := loopReviewer
			if step == state.DeliveryFixing {
				expected = loopWorker
			}
			if r.called(agentsName, agentStartName, expected) == nil || existingStarts.Load() < 1 {
				t.Fatalf("existing work was refused: %v %s", r.calls, r.out)
			}
		})
	}
}

func TestReview3366ExemptScopeKeepsBoardEligibility(t *testing.T) {
	now := time.Now()
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprint(held), func(t *testing.T) {
			cs := boardCandidates(3)
			cs[1].WIPExempt = true
			if held {
				cs[1].Skip = "blocked by existing eligibility"
			}
			st := &state.State{DeliveriesAt: now}
			sc := pickScope{listed: now, alive: func(state.Party) bool { return false }, exemptOnly: true}
			res := nextFree(st, cs, sc)
			if (res.Pick != nil) == held {
				t.Fatalf("scope lost eligibility: %+v", res)
			}
			if res.Pick != nil && res.Pick.Ref != cs[1].Ref {
				t.Fatal("scope admitted ordinary work even below limit")
			}
		})
	}
	a, _ := supervisorApp(t)
	a.cfg.Board.Order = nil
	old := boardGH
	t.Cleanup(func() { boardGH = old })
	boardGH = func(context.Context, ...string) ([]byte, error) {
		t.Error("empty exempt scope made a GitHub call")
		return nil, errors.New("unexpected board read")
	}
	c := a.boardNextCmd()
	c.SetArgs([]string{deliveryExemptOnlyFlag})
	if err := c.Execute(); Code(err) != ExitRefused {
		t.Fatal(err)
	}
}

func TestReview3366FreshPrioritySeesNewPendingWork(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			r, busy := fillRig(t)
			now := r.a.timeNow()
			old := r.answer
			pending := func() {
				seed(t, r.a, func(st *state.State) {
					st.Deliveries = []state.Delivery{{Ref: deliveryControlPendingRef, Step: state.DeliveryFixing, Agent: holderGone, Since: now, Started: now}}
				})
			}
			if !late {
				pending()
			} else {
				r.a.cfg.Agents.Models = []config.AgentModelRule{{Match: ".*", Model: litTestModel}}
			}
			r.answer = func(args []string) (string, int) {
				if args[0] == LoopCapacity {
					if late {
						pending()
					}
					return deliveryControlCapacity, 0
				}
				return old(args)
			}
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			// The plan's empty reservation list predates a committed pending
			// delivery: the fresh check names it, and at room 1 the half-room
			// cap leaves the slot to the new start.
			_, err = l.StartItem(t.Context(), superloop.Action{Key: "fresh reservation"}, LoopBoardItem{Ref: epicTen}, []string{})
			if err != nil || !strings.Contains(r.out.String(), deliveryURL(deliveryControlPendingRef, false)) {
				t.Fatalf("fresh priority missed pending work: busy=%d err=%v %s", *busy, err, r.out)
			}
		})
	}
}

func TestReview3366ExemptionPreservesMonitoredAuthorization(t *testing.T) {
	r, busy := fillRig(t)
	r.a.cfg.Delivery.WIPLimit = 1
	r.a.cfg.StateDir = t.TempDir()
	now := r.a.timeNow()
	seed(t, r.a, func(st *state.State) {
		st.DeliveriesAt = now
		st.StartDelivery(deliveryControlPendingRef, deliveryIntakeExistingAgent, "", now)
	})
	const externalRef = "org/repo#7"
	if err := issuemonitor.WithState(t.Context(), r.a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
		s.Issues[externalRef] = issuemonitor.Record{Class: issuemonitor.External}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := issueMonitorGH
	t.Cleanup(func() { issueMonitorGH = old })
	reads := 0
	issueMonitorGH = func(context.Context, ...string) ([]byte, error) {
		reads++
		if reads == 1 {
			return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"visitor"},"__typename":"Issue"}}}}`), nil
		}
		return nil, errors.New("membership observation denied")
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.StartItem(t.Context(), superloop.Action{Key: "external exempt candidate"}, LoopBoardItem{Ref: externalRef, WIPExempt: true})
	if !errors.Is(err, issuemonitor.ErrExternal) || *busy != 0 || reads == 0 {
		t.Fatalf("WIP exemption bypassed trust: busy=%d reads=%d err=%v", *busy, reads, err)
	}
}

func TestReview3366PreviewUsesScopedIntake(t *testing.T) {
	r, busy := fillRig(t)
	r.a.cfg.Delivery.WIPLimit = 1
	r.a.cfg.Board.Order = []config.BoardStep{{Name: "preview exception", WIPExempt: true}}
	now := r.a.timeNow()
	seed(t, r.a, func(st *state.State) {
		st.DeliveriesAt = now
		st.StartDelivery(deliveryControlPendingRef, deliveryIntakeExistingAgent, "", now)
	})
	old := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == LoopCapacity {
			return deliveryControlCapacity, 0
		}
		if args[0] == LoopBoard {
			if !slices.Contains(args, deliveryExemptOnlyFlag) {
				t.Error("preview used ordinary discovery")
			}
			return deliveryIntakePick(epicTen, true), 0
		}
		out, code := old(args)
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			seed(t, r.a, func(st *state.State) { st.StartDelivery(epicTen, args[2], "", now) })
		}
		return out, code
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.DryRun = true
	if err := l.PreviewFill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *busy != 0 || !strings.Contains(r.out.String(), "WOULD NEXT fill 1") {
		t.Fatalf("wrong preview: busy=%d output=%s", *busy, r.out)
	}
	l.DryRun = false
	st, _ := r.a.store.Read()
	if err := l.Fill(t.Context(), st, nil, nil); err != nil {
		t.Fatal(err)
	}
	if *busy != 1 {
		t.Fatalf("real pass disagreed with preview: %d", *busy)
	}
}

// An intentional admission wait must end an older refill-stall episode:
// otherwise the newer watchdog reports a board-start fault at a full limit.
func TestDeliveryAdmissionClearsRefillStallBeforeBoardRead(t *testing.T) {
	for _, mode := range []string{servingModeFull, kindStale} {
		t.Run(mode, func(t *testing.T) {
			r, busy := fillRig(t)
			r.a.cfg.Delivery.WIPLimit = 1
			now := r.a.timeNow()
			old := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == LoopCapacity {
					return deliveryControlCapacity, 0
				}
				return old(args)
			}
			seed(t, r.a, func(st *state.State) {
				st.DeliveriesAt = now
				if mode == kindStale {
					st.DeliveriesAt = now.Add(-max(11*time.Minute, 4*r.a.cfg.Supervisor.LoopInterval.Duration))
				} else {
					st.StartDelivery(deliveryControlPendingRef, deliveryIntakeExistingAgent, "", now)
				}
			})
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			l.StallWait("", "an earlier start failed")
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Fill(t.Context(), st, nil, nil); err != nil {
				t.Fatal(err)
			}
			if *busy != 0 || r.called(LoopBoard) != nil {
				t.Fatalf("held admission read the board or started work: busy=%d calls=%v", *busy, r.calls)
			}
			if !l.Stall.Since.IsZero() || !l.FillPause.Until.IsZero() {
				t.Fatalf("intentional wait retained a refill alarm or pause: stall=%+v pause=%+v", l.Stall, l.FillPause)
			}
		})
	}
}

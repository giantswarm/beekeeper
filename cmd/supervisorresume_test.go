package cmd

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const recoveryTask = "Work https://github.com/o/r/issues/12"

func TestLoopRecoversEndedSettledPark(t *testing.T) {
	for _, mode := range []string{"successful recovery", "release", "partial start"} {
		failure := mode != "successful recovery"
		t.Run(mode, func(t *testing.T) {
			r, _ := fillRig(t)
			var live []*claude.Session
			stubFresh(t, &live)
			ag := state.Agent{Party: state.Party{Name: "ended worker", Session: "ended-session"}, Task: recoveryTask, Park: &state.Park{At: r.a.now, Resumable: r.a.now, Answer: "note closed"}}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = append(st.Agents, ag); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			r.answer = func(args []string) (string, int) {
				if args[0] == fillCapacityCommand {
					return fillNoRoomJSON, 0
				}
				if args[0] == agentsName && args[1] == agentStartName {
					if args[2] != ag.Name || !strings.Contains(strings.Join(args, " "), "--harness auto") {
						t.Fatalf("start: %v", args)
					}
					b, err := os.ReadFile(args[3])
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(b), ag.Task) || !strings.Contains(string(b), "note closed") {
						t.Fatalf("brief: %s", b)
					}
					if failure {
						if mode == "partial start" {
							if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
								st.Agents[0].Session = "failed-replacement"
								return nil, nil
							}); err != nil {
								t.Fatal(err)
							}
						}
						return "recovery start denied", ExitRefused
					}
					if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Session = "replacement"; return nil, nil }); err != nil {
						t.Fatal(err)
					}
				}
				return "", 0
			}
			r.pass(t)
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if failure {
				if len(st.Agents) != 0 {
					t.Fatalf("dead holder remains: %+v", st.Agents)
				}
			} else if len(st.Agents) != 1 || st.Agents[0].Park != nil || st.Agents[0].Task != ag.Task {
				t.Fatalf("takeover: %+v", st.Agents)
			}
		})
	}
}

func TestLoopRecoveryLeavesUnsettledParksAndStoppedDesk(t *testing.T) {
	for _, mode := range []string{"unsettled", paramDone, "desk stop", dryRunState, "no task", holderLive, "managed SSH"} {
		t.Run(mode, func(t *testing.T) {
			r := newLoopRig(t)
			ag := state.Agent{Party: state.Party{Name: "sleeping worker", Session: "gone-worker"}, Task: recoveryTask, Park: &state.Park{At: r.a.now, Resumable: r.a.now}}
			if mode == "unsettled" {
				ag.Park.Resumable = state.Park{}.Resumable
			}
			if mode == paramDone {
				ag.Done = true
			}
			if mode == "managed SSH" {
				ag.Execution = &execution.Ref{}
			}
			if mode == "no task" {
				ag.Task = ""
			}
			st := &state.State{Agents: []state.Agent{ag}}
			if mode == "desk stop" {
				st.Stop = &state.Stop{}
			}
			var live []*claude.Session
			if mode == holderLive {
				live = []*claude.Session{{ID: ag.Session, Name: ag.Name}}
			}
			stubFresh(t, &live)
			l := &supervisorLoop{a: r.a, dryRun: mode == dryRunState}
			if err := l.recoverParks(t.Context(), st); err != nil {
				t.Fatal(err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("acted: %v", r.calls)
			}
		})
	}
}

func TestLoopRecoveryRequiresDiscovery(t *testing.T) {
	r := newLoopRig(t)
	original := freshSessions
	t.Cleanup(func() { freshSessions = original })
	want := errors.New("process inventory unavailable")
	freshSessions = func(*app) ([]*claude.Session, *proc.Table, error) { return nil, nil, want }
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "park inventory worker"}, Task: recoveryTask, Park: &state.Park{Resumable: r.a.now}}}}
	l := &supervisorLoop{a: r.a}
	if err := l.recoverParks(t.Context(), st); !errors.Is(err, want) {
		t.Fatalf("discovery: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("acted without discovery: %v", r.calls)
	}
}

package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestLoopRecoversEndedSettledPark(t *testing.T) {
	for _, mode := range []string{"restart", "release", "partial start"} {
		failure := mode != "restart"
		t.Run(mode, func(t *testing.T) {
			r, _ := fillRig(t)
			ag := state.Agent{Party: state.Party{Name: "ended worker", Session: "ended-session"}, Task: "Work https://github.com/o/r/issues/12", Park: &state.Park{At: r.a.now, Resumable: r.a.now, Answer: "note closed"}}
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
						return "start refused", ExitRefused
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
	for _, mode := range []string{"unsettled", "done", "stopped", "dry run", "no task", "live"} {
		t.Run(mode, func(t *testing.T) {
			r := newLoopRig(t)
			ag := state.Agent{Party: state.Party{Name: "ended", Session: "ended"}, Task: "Work https://github.com/o/r/issues/12", Park: &state.Park{At: r.a.now, Resumable: r.a.now}}
			if mode == "unsettled" {
				ag.Park.Resumable = state.Park{}.Resumable
			}
			if mode == "done" {
				ag.Done = true
			}
			if mode == "no task" {
				ag.Task = ""
			}
			st := &state.State{Agents: []state.Agent{ag}}
			if mode == "stopped" {
				st.Stop = &state.Stop{}
			}
			var live []*claude.Session
			if mode == "live" {
				live = []*claude.Session{{ID: ag.Session, Name: ag.Name}}
			}
			stubFresh(t, &live)
			l := &supervisorLoop{a: r.a, dryRun: mode == "dry run"}
			if err := l.recoverParks(t.Context(), st); err != nil {
				t.Fatal(err)
			}
			if len(r.calls) != 0 {
				t.Fatalf("acted: %v", r.calls)
			}
		})
	}
}

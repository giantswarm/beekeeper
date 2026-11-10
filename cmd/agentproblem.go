package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

const agentProblemEvent = "agents.problem"

func (a *app) agentProblemCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "problem <finding>",
		Short: "Log a mid-task finding and deliver it to the supervisor",
		Long: `Records a finding immediately as agents.problem and delivers it through the
supervisor's watch. Name what broke, the evidence and the owning repository;
use full issue and pull request URLs. The worker keeps its task and park.
Repeating the same finding, including with idle --problem, records and
delivers it once per worker assignment.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.managedExecutionID() != "" {
				return a.remoteControl(cmd.Context(), "problem", args[0])
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			if a.queueReports {
				return a.submitReport(sessionReport{Kind: "problem", By: me, Finding: args[0]})
			}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i := a.callerAgent(st, me)
				if i < 0 {
					return nil, refused("this session is not registered: `beekeeper agents register` first")
				}
				return a.recordWorkerProblem(st, &st.Agents[i], me, args[0])
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, "problem recorded for the supervisor's watch (once per assignment)")
			return err
		},
	}
}

func (a *app) recordWorkerProblem(st *state.State, ag *state.Agent, me state.Party, finding string) ([]state.Event, error) {
	finding = oneLine(finding)
	if finding == "" || strings.EqualFold(finding, noProblems) {
		return nil, refused("problem requires a finding with evidence and owning repository")
	}
	if ag.Task == "" {
		return nil, refused("a mid-task problem requires an assigned task")
	}
	if missing := post.Unlinked(finding, agentHere(st, ag)); len(missing) > 0 {
		return nil, refused("problem refused: %s", strings.Join(missing, "; "))
	}
	ag.ActivityAt = a.now.UTC()
	found := freshWorkerProblems(ag, []string{finding})
	if len(found) == 0 {
		return nil, nil
	}
	st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: me, At: a.now.UTC(), Task: ag.Task, Problems: found})
	return []state.Event{event(me, agentProblemEvent, "%s: %s", ag.Name, finding)}, nil
}

// Keep receipts independently of the watch's pending reports. A new assignment
// may report the same finding again.
func freshWorkerProblems(ag *state.Agent, findings []string) []string {
	task := ag.Task
	if task == "" {
		task = ag.LastTask
	}
	previous := ag.ReportedProblems
	if previous == nil || previous.Task != task || !previous.AssignedAt.Equal(ag.AssignedAt) {
		previous = &state.WorkerProblems{Task: task, AssignedAt: ag.AssignedAt}
	}
	var fresh []string
	for _, finding := range findings {
		finding = oneLine(finding)
		if !slices.Contains(previous.Findings, finding) {
			previous.Findings = append(previous.Findings, finding)
			fresh = append(fresh, finding)
		}
	}
	if len(fresh) > 0 {
		ag.ReportedProblems = previous
	}
	return fresh
}

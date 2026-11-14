package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

type EpicDelivery struct {
	Item     board.Item   `json:"item"`
	Ref      string       `json:"ref"`
	URL      string       `json:"url"`
	Children []board.Item `json:"children"`
	Missing  []string     `json:"missing,omitempty"`
}

type EpicSnapshot struct {
	At    time.Time      `json:"at"`
	Epics []EpicDelivery `json:"epics"`
}

// Parked owners are woken only when their children's durable observations change.
// A saved signature survives loop restarts, while an unsuccessful wake retries.
func (l *Loop) WakeEpicOwners(ctx context.Context, st *state.State) error {
	var view EpicSnapshot
	if _, err := l.Host.Store().ReadFile(epicSnapshotFile, &view); err != nil {
		return err
	}
	for _, e := range view.Epics {
		owner := Cmd.EpicOwner(st, e.Ref)
		if owner == nil || owner.Park == nil || !strings.EqualFold(LoopParkOn(owner.Park), e.Ref) {
			continue
		}
		var parts []string
		for _, ch := range e.Children {
			part := ch.Ref + ": " + Cmd.EpicChildStatus(st, ch, l.Host.TimeNow(), l.Host.DeliveryPolicy())
			for _, d := range st.Deliveries {
				if strings.EqualFold(d.Ref, ch.Ref) {
					part += fmt.Sprintf(" %s %s %s", d.Step, d.Since.Format(time.RFC3339Nano), d.Verdict)
				}
			}
			for _, report := range st.WorkerReports {
				linked := slices.ContainsFunc(Cmd.NamedRefs(report.Task, ""), func(ref string) bool { return strings.EqualFold(ref, ch.Ref) })
				if !linked {
					for _, d := range st.Deliveries {
						if strings.EqualFold(d.Ref, ch.Ref) && strings.EqualFold(d.Agent, report.By.Name) {
							linked = true
							break
						}
					}
				}
				if linked {
					raw, err := json.Marshal(report)
					if err != nil {
						return err
					}
					part += " report " + string(raw)
				}
			}
			parts = append(parts, part)
		}
		signature := "children:\n" + strings.Join(parts, "\n")
		file := "epic-owner-" + strings.NewReplacer("/", "-", "#", "-").Replace(e.Ref) + ".json"
		var previous string
		if _, err := l.Host.Store().ReadFile(file, &previous); err != nil {
			return err
		}
		if signature == previous {
			continue
		}
		if _, err := l.Run(ctx, agentsName, agentWakeName, owner.Name, "Epic sub-issue observations changed. Read worker reports and reviews, maintain the plan, and prove acceptance when ready.\n"+signature); err != nil {
			return err
		}
		if err := l.Host.Store().WriteFile(file, signature); err != nil {
			return err
		}
	}
	return nil
}

// epicStart runs in the command, which holds what it needs (loopEpicStart).
func (l *Loop) epicStart(ctx context.Context, next LoopBoardItem) error {
	return Cmd.LoopEpicStart(ctx, l, next)
}

var EpicGitHub = github.RunGH

const EpicAcceptanceProven = "Epic acceptance proven: "

// Completion requires the owner's explicit proof report and a fresh GitHub
// observation that no child remains open. A merge alone is never acceptance.
func (l *Loop) CompleteEpics(ctx context.Context, st *state.State) error {
	for _, report := range st.WorkerReports {
		ref, ok := strings.CutPrefix(report.Task, epicOwnerTask)
		if !ok || !report.Finished || !strings.HasPrefix(strings.TrimSpace(report.Text), EpicAcceptanceProven) || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(report.Text), EpicAcceptanceProven)) == "" {
			continue
		}
		r, ok := Cmd.ParseRef(ref)
		if !ok {
			continue
		}
		receipt := "epic-complete-" + strings.NewReplacer("/", "-", "#", "-").Replace(ref) + ".json"
		var completed bool
		if _, err := l.Host.Store().ReadFile(receipt, &completed); err != nil {
			return err
		}
		if completed {
			continue
		}
		raw, err := EpicGitHub(ctx, "api", fmt.Sprintf("repos/%s/issues/%d", r.Repo, r.N), "--jq", `.sub_issues_summary | if (.total | type) == "number" and (.completed | type) == "number" then .total - .completed else null end`)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(raw)) != "0" {
			continue
		}
		if _, err := EpicGitHub(ctx, "issue", "close", fmt.Sprint(r.N), "--repo", r.Repo); err != nil {
			return err
		}
		if _, err := l.Run(ctx, LoopBoard, "move", ref, "done"); err != nil {
			return err
		}
		if err := l.Host.Store().WriteFile(receipt, true); err != nil {
			return err
		}
	}
	return nil
}

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const epicOwnerTask = "Epic owner: "
const epicSnapshotFile = "epic-delivery.json"

type epicDelivery struct {
	Ref      string       `json:"ref"`
	URL      string       `json:"url"`
	Children []board.Item `json:"children"`
	Missing  []string     `json:"missing,omitempty"`
}
type epicSnapshot struct {
	At    time.Time      `json:"at"`
	Epics []epicDelivery `json:"epics"`
}

func epicOwner(st *state.State, ref string) *state.Agent {
	for i := range st.Agents {
		ag := &st.Agents[i]
		if !ag.Done && strings.EqualFold(ag.Task, epicOwnerTask+ref) {
			return ag
		}
	}
	return nil
}

// Offer the owner even after the last child closes: only acceptance ends an epic.
// This uses the same ranked eligibility gates as implementation work.
func epicCandidates(snap *board.Snapshot, cfg config.Board, cands []board.Candidate) ([]board.Candidate, epicSnapshot) {
	view := epicSnapshot{At: time.Now().UTC()}
	var owners []board.Candidate
	for _, it := range snap.Items {
		if !snap.Gate.Epic(it) || it.Status != snap.Gate.Go {
			continue
		}
		view.Epics = append(view.Epics, epicDelivery{Ref: it.Ref, URL: it.URL, Children: it.SubIssues, Missing: it.Missing})
		one := *snap
		one.Order = slices.Clone(snap.Order)
		for i := range one.Order {
			one.Order[i].SubIssues = false
		}
		for _, c := range board.Rank(&one, cfg, view.At) {
			if strings.EqualFold(c.Ref, it.Ref) {
				c.EpicOwner = true
				owners = append(owners, c)
				break
			}
		}
	}
	// Items already offered by their own status still belong to their epic.
	for i := range cands {
		if cands[i].Epic != "" {
			continue
		}
		seen := map[string]bool{}
		for parent := snap.Parent(cands[i].Ref); parent != "" && !seen[parent]; parent = snap.Parent(parent) {
			seen[parent] = true
			if j := slices.IndexFunc(view.Epics, func(e epicDelivery) bool { return strings.EqualFold(e.Ref, parent) }); j >= 0 {
				cands[i].Epic = parent
				if !slices.ContainsFunc(view.Epics[j].Children, func(ch board.Item) bool { return strings.EqualFold(ch.Ref, cands[i].Ref) }) {
					view.Epics[j].Children = append(view.Epics[j].Children, cands[i].Item)
				}
				break
			}
		}
	}
	// Replace an ordinary epic candidate, including epics without remaining children.
	cands = slices.DeleteFunc(cands, func(c board.Candidate) bool {
		return slices.ContainsFunc(view.Epics, func(e epicDelivery) bool { return strings.EqualFold(c.Ref, e.Ref) })
	})
	return append(owners, cands...), view
}

// Admission is recomputed against the locked state when claiming a candidate.
func epicAdmission(st *state.State, c board.Candidate, sc pickScope) string {
	if !c.EpicOwner && c.Epic == "" {
		return ""
	}
	ref := c.Epic
	if c.EpicOwner {
		ref = c.Ref
	}
	if !slices.ContainsFunc(sc.epics.Epics, func(e epicDelivery) bool { return strings.EqualFold(e.Ref, ref) }) {
		return ""
	}
	owner := epicOwner(st, ref)
	if owner == nil && sc.epicLimit > 0 {
		slots := sc.epicLimit
		for _, e := range sc.epics.Epics {
			if epicOwner(st, e.Ref) != nil {
				slots--
			}
		}
		admitted := false
		for _, e := range sc.epics.Epics {
			if epicOwner(st, e.Ref) != nil {
				continue
			}
			if slots <= 0 {
				break
			}
			slots--
			if strings.EqualFold(e.Ref, ref) {
				admitted = true
				break
			}
		}
		if !admitted {
			return "epic WIP limit reached"
		}
	}
	if c.EpicOwner {
		return ""
	}
	if owner == nil {
		return "epic owner has not started"
	}
	n := 0
	for _, e := range sc.epics.Epics {
		if !strings.EqualFold(e.Ref, ref) {
			continue
		}
		for _, ch := range e.Children {
			if strings.EqualFold(ch.Ref, c.Ref) {
				continue
			}
			if epicChildState(st, ch.Ref, sc.listed, sc.wipPolicy) == "flowing" {
				n++
			}
		}
	}
	parallel := sc.epicParallel
	if parallel <= 0 {
		parallel = 3
	}
	if n >= parallel {
		return "epic parallelism limit reached"
	}
	return ""
}

func epicChildState(st *state.State, ref string, now time.Time, policy deliveryPolicy) string {
	for _, d := range st.Deliveries {
		if !strings.EqualFold(d.Ref, ref) {
			continue
		}
		if !d.Active() || d.Step == state.DeliveryReleasing {
			return "merged"
		}
		switch classifyDelivery(st, d, now, policy).Class {
		case deliveryPerson:
			return "waiting on the person"
		case deliveryBlocked:
			return "blocked"
		default:
			return "flowing"
		}
	}
	for _, ag := range st.Agents {
		if ag.Done {
			continue
		}
		for _, r := range namedRefs(ag.Task, "") {
			if strings.EqualFold(r, ref) {
				return "flowing"
			}
		}
	}
	return "queued"
}

func (a *app) epicStatus(st *state.State) (string, error) {
	var view epicSnapshot
	if _, err := a.store.ReadFile(epicSnapshotFile, &view); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d epics in progress of limit %d (last board observation %s)", len(view.Epics), a.cfg.Delivery.EpicLimit, view.At.Format(time.RFC3339))
	for _, e := range view.Epics {
		fmt.Fprintf(&b, "\n%s", e.URL)
		if owner := epicOwner(st, e.Ref); owner != nil {
			fmt.Fprintf(&b, "; owner %s", owner.Name)
		} else {
			b.WriteString("; owner queued")
		}
		for _, ch := range e.Children {
			fmt.Fprintf(&b, "\n  %s: %s", ch.URL, epicChildState(st, ch.Ref, a.timeNow(), a.deliveryPolicy()))
		}
		for _, missing := range e.Missing {
			fmt.Fprintf(&b, "\n  unknown: %s", missing)
		}
	}
	return b.String(), nil
}

// Parked owners are woken only when their children's durable observations change.
// A saved signature survives loop restarts, while an unsuccessful wake retries.
func (l *supervisorLoop) wakeEpicOwners(ctx context.Context, st *state.State) error {
	var view epicSnapshot
	if _, err := l.a.store.ReadFile(epicSnapshotFile, &view); err != nil {
		return err
	}
	for _, e := range view.Epics {
		owner := epicOwner(st, e.Ref)
		if owner == nil || owner.Park == nil || !strings.EqualFold(loopParkOn(owner.Park), e.Ref) {
			continue
		}
		var parts []string
		for _, ch := range e.Children {
			part := ch.Ref + ": " + epicChildState(st, ch.Ref, l.a.timeNow(), l.a.deliveryPolicy())
			for _, d := range st.Deliveries {
				if strings.EqualFold(d.Ref, ch.Ref) {
					part += fmt.Sprintf(" %s %s %s", d.Step, d.Since.Format(time.RFC3339Nano), d.Verdict)
				}
			}
			for _, report := range st.WorkerReports {
				linked := slices.ContainsFunc(namedRefs(report.Task, ""), func(ref string) bool { return strings.EqualFold(ref, ch.Ref) })
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
		if _, err := l.a.store.ReadFile(file, &previous); err != nil {
			return err
		}
		if signature == previous {
			continue
		}
		if _, err := l.run(ctx, agentsName, agentWakeName, owner.Name, "Epic sub-issue observations changed. Read worker reports and reviews, maintain the plan, and prove acceptance when ready.\n"+signature); err != nil {
			return err
		}
		if err := l.a.store.WriteFile(file, signature); err != nil {
			return err
		}
	}
	return nil
}

func (l *supervisorLoop) epicStart(next loopBoardItem) error {
	if !next.EpicOwner && next.Epic == "" {
		return nil
	}
	st, err := l.a.store.Read()
	if err != nil {
		return err
	}
	var view epicSnapshot
	if _, err := l.a.store.ReadFile(epicSnapshotFile, &view); err != nil {
		return err
	}
	if next.EpicOwner && epicOwner(st, next.Ref) != nil {
		return errLoopLater
	}
	c := board.Candidate{Item: board.Item{Ref: next.Ref}, Epic: next.Epic, EpicOwner: next.EpicOwner}
	sc := pickScope{epics: view, epicLimit: l.a.cfg.Delivery.EpicLimit, epicParallel: l.a.cfg.Delivery.EpicParallel, listed: l.a.timeNow(), wipPolicy: l.a.deliveryPolicy()}
	if why := epicAdmission(st, c, sc); why != "" {
		return fmt.Errorf("%s: %s: %w", next.URL, why, errLoopLater)
	}
	return nil
}

var epicGitHub = github.RunGH

const epicAcceptanceProven = "Epic acceptance proven: "

// Completion requires the owner's explicit proof report and a fresh GitHub
// observation that no child remains open. A merge alone is never acceptance.
func (l *supervisorLoop) completeEpics(ctx context.Context, st *state.State) error {
	for _, report := range st.WorkerReports {
		ref, ok := strings.CutPrefix(report.Task, epicOwnerTask)
		if !ok || !report.Finished || !strings.Contains(report.Text, epicAcceptanceProven) {
			continue
		}
		r, ok := parseRef(ref)
		if !ok {
			continue
		}
		receipt := "epic-complete-" + strings.NewReplacer("/", "-", "#", "-").Replace(ref) + ".json"
		var completed bool
		if _, err := l.a.store.ReadFile(receipt, &completed); err != nil {
			return err
		}
		if completed {
			continue
		}
		raw, err := epicGitHub(ctx, "api", fmt.Sprintf("repos/%s/issues/%d", r.Repo, r.N), "--jq", ".sub_issues_summary.open")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(raw)) != "0" {
			continue
		}
		if _, err := epicGitHub(ctx, "issue", "close", fmt.Sprint(r.N), "--repo", r.Repo); err != nil {
			return err
		}
		if _, err := l.run(ctx, loopBoard, "move", ref, "done"); err != nil {
			return err
		}
		if err := l.a.store.WriteFile(receipt, true); err != nil {
			return err
		}
	}
	return nil
}

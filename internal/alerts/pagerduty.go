package alerts

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// The PagerDuty half: the open incidents of the team's PagerDuty services,
// read through muster's read-only x_pd_* tools as the person, become
// PAGERDUTY NEW, ACKNOWLEDGED and RESOLVED lines. A page reaches the watch
// whether or not the installation's Alertmanager is read.

// Incident is an open PagerDuty incident as the watch keeps it.
type Incident struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	// Status is triggered or acknowledged.
	Status string `json:"status"`
	// Installation is the installation label of the incident's first
	// alert; empty while it has not been read.
	Installation string `json:"installation,omitempty"`
	Since        string `json:"since"`
}

// Acknowledged is the status of an incident someone acknowledged.
const Acknowledged = "acknowledged"

// PagerDutyAnswer is one reading of the open incidents, by incident id, or
// why it did not answer.
type PagerDutyAnswer struct {
	OK        bool
	Incidents map[string]Incident
	Why       string
}

// PagerDuty is the incidents' baseline: the last set, kept while PagerDuty
// does not answer. A nil Incidents means it has not been read yet.
type PagerDuty struct {
	Reachable bool                `json:"reachable"`
	Incidents map[string]Incident `json:"incidents"`
	Seen      time.Time           `json:"seen,omitzero"`
	Said      time.Time           `json:"said,omitzero"`
}

// ToolCall calls a muster tool of the PagerDuty server by its short name
// (list_incidents) and returns its text.
type ToolCall func(ctx context.Context, tool string, args map[string]any) (string, error)

// PagerDutyReader reads the open incidents of Services.
type PagerDutyReader struct {
	Call     ToolCall
	Services []string
}

// listIncidentsTool is the PagerDuty server's listing of incidents.
const listIncidentsTool = "list_incidents"

// incidentsLimit bounds one listing; the open incidents of a team's
// services are far fewer.
const incidentsLimit = 100

// Read lists the triggered and acknowledged incidents of the services, of
// every age, and reads the installation of each one known lacks.
func (p PagerDutyReader) Read(ctx context.Context, known map[string]Incident) PagerDutyAnswer {
	text, err := p.listIncidents(ctx)
	if err != nil {
		return PagerDutyAnswer{Why: err.Error()}
	}
	var list struct {
		Response []struct {
			ID        string `json:"id"`
			Number    int    `json:"incident_number"`
			Title     string `json:"title"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(text), &list); err != nil || list.Response == nil {
		return PagerDutyAnswer{Why: "list_incidents: no incident list: " + firstLine(text)}
	}
	out := map[string]Incident{}
	for _, r := range list.Response {
		in := Incident{Number: r.Number, Title: r.Title, Status: r.Status, Since: r.CreatedAt, Installation: known[r.ID].Installation}
		if in.Installation == "" {
			in.Installation = p.installation(ctx, r.ID)
		}
		out[r.ID] = in
	}
	return PagerDutyAnswer{OK: true, Incidents: out}
}

// listIncidents calls list_incidents, once more after retryPause when the
// call fails: muster answers auth_required for a moment while it reconnects
// the PagerDuty server, and the second call reads.
func (p PagerDutyReader) listIncidents(ctx context.Context) (string, error) {
	args := map[string]any{"query_model": map[string]any{
		"service_ids": p.Services, "status": []string{"triggered", Acknowledged},
		"date_range": "all", "limit": incidentsLimit, "sort_by": []string{"created_at:asc"},
	}}
	text, err := p.Call(ctx, listIncidentsTool, args)
	if err == nil {
		return text, nil
	}
	pause := time.NewTimer(retryPause)
	defer pause.Stop()
	select {
	case <-pause.C:
	case <-ctx.Done():
		return text, err
	}
	if text, again := p.Call(ctx, listIncidentsTool, args); again == nil {
		return text, nil
	}
	return text, fmt.Errorf("%w (2 attempts)", err)
}

// installation is the installation label of the incident's first alert, ""
// when it cannot be read: the next reading tries again.
func (p PagerDutyReader) installation(ctx context.Context, id string) string {
	text, err := p.Call(ctx, "list_alerts_from_incident", map[string]any{"incident_id": id, "query_model": map[string]any{"limit": 1}})
	if err != nil {
		return ""
	}
	var v struct {
		Response []struct {
			Body struct {
				CEF struct {
					Details struct {
						Installation string `json:"installation"`
					} `json:"details"`
				} `json:"cef_details"`
			} `json:"body"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(text), &v) != nil || len(v.Response) == 0 {
		return ""
	}
	return v.Response[0].Body.CEF.Details.Installation
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return cmp.Or(line, "an empty answer")
}

// PagerDutyGrace is how long a PagerDuty that read does not answer before
// it is said unseen: muster's auth_required while it reconnects the server
// lasts a reading or two.
const PagerDutyGrace = 3 * time.Minute

// PagerDutyStep returns the lines one reading prints and the baseline it
// leaves; prev is nil on the first reading. It mirrors Triage: a first look
// lists the open incidents, a PagerDuty that does not answer keeps the set
// and is said, once it is PagerDutyGrace unseen, again every UnseenRepeat.
func (r Rules) PagerDutyStep(prev *PagerDuty, ans PagerDutyAnswer, now time.Time) ([]string, *PagerDuty) {
	if !ans.OK && prev != nil && prev.Reachable && prev.Incidents != nil && now.Sub(prev.Seen) < PagerDutyGrace {
		return nil, prev
	}
	if !ans.OK {
		next := &PagerDuty{Said: now}
		if prev != nil {
			next.Incidents, next.Seen = prev.Incidents, prev.Seen
			if !prev.Reachable && now.Sub(prev.Said) < UnseenRepeat {
				next.Said = prev.Said
				return nil, next
			}
		}
		return []string{fmt.Sprintf("PAGERDUTY unreachable, %s: %s", r.unseenIncidents(next, now), ans.Why)}, next
	}
	next := &PagerDuty{Reachable: true, Incidents: ans.Incidents, Seen: now}
	if prev == nil || prev.Incidents == nil {
		lines := []string{fmt.Sprintf("PAGERDUTY first look: %d open", len(ans.Incidents))}
		for _, id := range byNumber(ans.Incidents) {
			lines = append(lines, r.incidentLine("OPEN", ans.Incidents[id], now))
		}
		return lines, next
	}
	var lines []string
	if !prev.Reachable {
		again := "PAGERDUTY reachable again"
		if !prev.Seen.IsZero() {
			again += fmt.Sprintf(" after %s unseen", ago(now.Sub(prev.Seen)))
		}
		lines = append(lines, again)
	}
	for _, id := range byNumber(ans.Incidents) {
		in, was := ans.Incidents[id], prev.Incidents[id]
		switch _, known := prev.Incidents[id]; {
		case !known:
			lines = append(lines, r.incidentLine(New, in, now))
		case in.Status == Acknowledged && was.Status != Acknowledged:
			lines = append(lines, r.incidentLine("ACKNOWLEDGED", in, now))
		}
	}
	for _, id := range byNumber(prev.Incidents) {
		if _, open := ans.Incidents[id]; !open {
			lines = append(lines, r.incidentLine("RESOLVED", prev.Incidents[id], now))
		}
	}
	return lines, next
}

// incidentLine is one incident's change, the team in capitals:
//
//	PAGERDUTY <kind> <installation> <TEAM> #<number> <title> since <start>
func (r Rules) incidentLine(kind string, in Incident, now time.Time) string {
	team := ""
	if r.Team != "" {
		team = strings.ToUpper(r.Team) + " "
	}
	return fmt.Sprintf("PAGERDUTY %s %s %s#%d %s%s", kind, cmp.Or(in.Installation, "-"), team, in.Number, in.Title, sinceSuffix(kind, in.Since, now))
}

// unseenIncidents says since when the incidents are unseen.
func (r Rules) unseenIncidents(p *PagerDuty, now time.Time) string {
	switch {
	case p.Incidents == nil:
		return "incidents never read"
	case p.Seen.IsZero():
		return "incidents unseen since an unknown time"
	}
	return fmt.Sprintf("incidents unseen for %s (since %s)", ago(now.Sub(p.Seen)), sinceText(p.Seen.Format(time.RFC3339Nano), now))
}

// PagerDutySnapshot is the open incidents, oldest first.
func (r Rules) PagerDutySnapshot(ans PagerDutyAnswer, now time.Time) []string {
	if !ans.OK {
		return []string{"pagerduty unreachable: " + ans.Why}
	}
	lines := []string{fmt.Sprintf("pagerduty at %s: %d open", now.UTC().Format("15:04Z"), len(ans.Incidents))}
	for _, id := range byNumber(ans.Incidents) {
		in := ans.Incidents[id]
		lines = append(lines, fmt.Sprintf("  %-12s %-12s #%-6d %s since %s", in.Status, cmp.Or(in.Installation, "-"), in.Number, in.Title, sinceText(in.Since, now)))
	}
	return lines
}

// byNumber are the incidents' ids, by incident number.
func byNumber(incidents map[string]Incident) []string {
	return slices.SortedFunc(maps.Keys(incidents), func(a, b string) int {
		return cmp.Compare(incidents[a].Number, incidents[b].Number)
	})
}

package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// paceReason is the person's words a pace override records; paceOffArg
// and paceOnArg are capacity pace's subcommands, paceUntil its end.
const (
	paceReason = "usage reset in hand"
	paceOffArg = "off"
	paceOnArg  = "on"
	paceUntil  = "2h"
)

// pace runs capacity pace with args and returns its output.
func (r *pickRig) pace(args ...string) (string, error) {
	r.t.Helper()
	a, out := r.app(false)
	c := a.capacityCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs(append([]string{"pace"}, args...))
	err := c.Execute()
	return out.String(), err
}

// With both subscriptions ahead of their pace no worker start fits. The
// person's pace override of claude lets claude's worker starts run ahead of
// its pace, shown under capacity --json, while its weekly cap still holds;
// a new 7-day window, pace on and its time each end it.
func TestCapacityPaceOffLetsAProviderRunAheadOfItsPace(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.claude[1], r.codex[1] = 77, 70
	r.want(noPick)

	if _, err := r.pace(paceOffArg, claudeComm, untilFlag, paceUntil, flagNoteReason, paceReason); err != nil {
		t.Fatalf("pace off: %v", err)
	}
	r.want("claude " + pickOpus)
	var p struct {
		Capacity struct {
			Providers map[string]struct {
				PaceOff *struct{ Reason, By string }
			}
		}
	}
	if err := json.Unmarshal([]byte(r.pick(true)), &p); err != nil {
		t.Fatal(err)
	}
	if po := p.Capacity.Providers[claudeComm].PaceOff; po == nil || po.Reason != paceReason || po.By != agentOne {
		t.Errorf("claude's paceOff = %+v, want %q by %q", po, paceReason, agentOne)
	}
	if po := p.Capacity.Providers["codex"].PaceOff; po != nil {
		t.Errorf("codex's paceOff = %+v, want none", po)
	}
	out, err := r.pace()
	if err != nil || !strings.Contains(out, "pace off claude until") || !strings.Contains(out, paceReason) {
		t.Errorf("capacity pace = %q, %v", out, err)
	}

	// The weekly cap still holds.
	r.claude[1] = 95
	r.want(noPick)
	r.claude[1] = 77

	// A 7-day window that began after the override (the reset used) keeps
	// to its pace again.
	reset := r.reset7d
	r.reset7d = r.t0.Add(7*24*time.Hour + 2*time.Hour)
	r.want(noPick)
	r.reset7d = reset
	r.want("claude " + pickOpus)

	if _, err := r.pace(paceOnArg, claudeComm); err != nil {
		t.Fatalf("pace on: %v", err)
	}
	r.want(noPick)
	if _, err := r.pace(paceOnArg, claudeComm); err == nil {
		t.Error("a second pace on claude: no refusal")
	}

	// Its time ends it.
	if _, err := r.pace(paceOffArg, claudeComm, untilFlag, paceUntil, flagNoteReason, paceReason); err != nil {
		t.Fatalf("pace off again: %v", err)
	}
	r.want("claude " + pickOpus)
	r.t0 = r.t0.Add(2 * time.Hour)
	r.want(noPick)
	if out, err := r.pace(); err != nil || !strings.Contains(out, "no pace overrides") {
		t.Errorf("capacity pace after its time = %q, %v", out, err)
	}

	for _, args := range [][]string{
		{paceOffArg, "nobody", untilFlag, paceUntil, flagNoteReason, paceReason},
		{paceOffArg, executionLocal, untilFlag, paceUntil, flagNoteReason, paceReason},
		{paceOffArg, claudeComm, untilFlag, paceUntil},
		{paceOffArg, claudeComm, flagNoteReason, paceReason},
		{paceOffArg, claudeComm, untilFlag, "-1h", flagNoteReason, paceReason},
	} {
		if _, err := r.pace(args...); err == nil {
			t.Errorf("capacity pace %v: no refusal", args)
		}
	}
}

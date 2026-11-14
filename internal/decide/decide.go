// Package decide is the engine's pure core: given a view assembled from the informer cache, the clock
// and recorded answers, it returns a decision (the status to write, history rows, effects). It reads no
// clock and makes no calls; the reconcilers are the impure shell that applies decisions.
package decide

import (
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
)

// Trigger is what made the engine reconcile.
type Trigger string

// Triggers.
const (
	TriggerWebhook  Trigger = "webhook"
	TriggerResync   Trigger = "resync"
	TriggerDeadline Trigger = "deadline"
	TriggerPoll     Trigger = "poll"
	TriggerStartup  Trigger = "startup"
	TriggerRetry    Trigger = "retry"
	// TriggerEngine is a configuration change or an internal wake (e.g. a Change woke its Item).
	TriggerEngine Trigger = "engine"
)

// Reobserve is the result of the conditional reads made for a wait at its deadline, while polling, or
// on a transient retry.
type Reobserve struct {
	// Made is false when no read could be made (the wait's source is degraded and can't be polled).
	Made bool
	// Changed is true when the reads found the wait's facts changed.
	Changed bool
	// Err is the failure of the reads: transient, rate limited, or deferred by the budget reserve.
	Err *forge.Error
	// Reason is the unobserved reason when no read was made (source-degraded).
	Reason string
}

// ItemView is what decide.Item needs. Item carries the freshly observed facts (issue, board, assignees,
// parent, sub-issues, blockers, changes) and the previously decided fields as last written.
type ItemView struct {
	Now  time.Time
	Spec *v1alpha1.InstallationSpec
	// Team is the configured Team whose board option the item has; nil when there is none.
	Team *v1alpha1.Team
	// Participants are the logins of the Team's Persons, sorted.
	Participants []string
	Item         *v1alpha1.Item
	// Parent is the parent issue's Item, when tracked (its owner is the sub-item's owner).
	Parent *v1alpha1.Item
	// Changes are the existing Changes of Item.Status.Changes.
	Changes   []*v1alpha1.Change
	Trigger   Trigger
	Source    string
	Reobserve *Reobserve
	// LeftScope is true when the item left the tracked set by scope (team changed, archived, deleted);
	// ScopeReason is team-changed, archived or deleted.
	LeftScope   bool
	ScopeReason string
	// DropWaits is the drop_waits test fault.
	DropWaits bool
}

// ItemDecision is decide.Item's result. Status is the whole status to write except Shadow, which only
// the team pass writes.
type ItemDecision struct {
	Status    v1alpha1.ItemStatus
	Events    []history.Event
	Questions []decision.Question
	// NextCheck is when to reconcile the Item again (a wait's next check, retention); nil for none.
	NextCheck *time.Time
	// Delete is true when the Item must go (scope left, or retention passed).
	Delete bool
	// TeamPass is true when the team's decision pass must run (phase, eligibility, owner or order changed).
	TeamPass bool
}

// ChangeView is what decide.Change needs. Change carries the freshly observed facts.
type ChangeView struct {
	Now    time.Time
	Spec   *v1alpha1.InstallationSpec
	Change *v1alpha1.Change
	// Items are the tracked Items the Change closes.
	Items []*v1alpha1.Item
	// CIDurations are the repo's observed pipeline durations, oldest first (the ci deadline).
	CIDurations []time.Duration
	Trigger     Trigger
	Source      string
	Reobserve   *Reobserve
	DropWaits   bool
}

// Duration is a pipeline or stage duration to record.
type Duration struct {
	Kind, Key string
	D         time.Duration
}

// ChangeDecision is decide.Change's result.
type ChangeDecision struct {
	Status    v1alpha1.ChangeStatus
	Events    []history.Event
	Durations []Duration
	NextCheck *time.Time
	// WakeItems are the item refs whose phase depends on what changed.
	WakeItems []string
}

// TeamView is what decide.Team needs.
type TeamView struct {
	Now  time.Time
	Spec *v1alpha1.InstallationSpec
	Team *v1alpha1.Team
	// People are the team's participants, sorted by login.
	People []*v1alpha1.Person
	// Items are every tracked Item in the namespace (the team's, and others for in-flight blockers).
	Items []*v1alpha1.Item
	// GateEverything is the gate_everything test fault.
	GateEverything bool
	// Source is the source of the observation that triggered the pass (webhook, resync, startup,
	// engine); the shadow.* rows carry it. Empty means engine.
	Source string
}

// TeamDecision is decide.Team's result.
type TeamDecision struct {
	Team   v1alpha1.TeamStatus
	People map[string]v1alpha1.PersonStatus // by login
	// Shadows are the new status.shadow of every team Item, by issue ref; nil clears it.
	Shadows   map[string]*v1alpha1.Shadow
	Events    []history.Event
	Questions []decision.Question
}

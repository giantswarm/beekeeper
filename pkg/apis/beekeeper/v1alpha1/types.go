package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaxQueue bounds every queue a status carries: a status never grows without
// bound, and a claim past the cap is refused instead of queued.
const MaxQueue = 20

// Party names a session or a person: whose agent, of which team, on which
// host.
type Party struct {
	// Name is the session's name, or the person's.
	Name string `json:"name"`
	// Session is the Claude Code session id.
	// +optional
	Session string `json:"session,omitempty"`
	// HostSession is the desktop app's id for the session.
	// +optional
	HostSession string `json:"hostSession,omitempty"`
	// Person is the verified email of the person whose agent the party is.
	// +optional
	Person string `json:"person,omitempty"`
	// Team is the person's team.
	// +optional
	Team string `json:"team,omitempty"`
	// Host is the machine or installation the party runs on.
	// +optional
	Host string `json:"host,omitempty"`
}

// Holder is who holds an Environment, for what and since when.
type Holder struct {
	Party   Party       `json:"party"`
	Purpose string      `json:"purpose"`
	Since   metav1.Time `json:"since"`
	// UpgradeUnblock is the reason of the upgrade-unblock grant the claim was
	// admitted by during an upgrade.
	// +optional
	UpgradeUnblock string `json:"upgradeUnblock,omitempty"`
}

// Grant is the supervisor's word that a party may claim an Environment, in
// the order the grants were given.
type Grant struct {
	To Party       `json:"to"`
	By Party       `json:"by"`
	At metav1.Time `json:"at"`
	// UpgradeUnblock is why the supervisor granted a claim the upgrade hold
	// admits: the work that unblocks the upgrade.
	// +optional
	UpgradeUnblock string `json:"upgradeUnblock,omitempty"`
}

// Upgrade is a cluster upgrade running on an Environment.
type Upgrade struct {
	// To is the target release.
	To    string      `json:"to"`
	Since metav1.Time `json:"since"`
}

// EnvironmentSpec declares one Giant Swarm installation.
type EnvironmentSpec struct {
	// Installation is the installation's name.
	Installation string `json:"installation"`
	// MusterCluster is the cluster name muster reaches the installation by.
	// +optional
	MusterCluster string `json:"musterCluster,omitempty"`
	// Team owns the installation.
	// +optional
	Team string `json:"team,omitempty"`
	// AlertFloor is the lowest severity the watch reports.
	// +optional
	AlertFloor string `json:"alertFloor,omitempty"`
}

// EnvironmentStatus is who holds the installation and what waits on it.
type EnvironmentStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Holder is nil while the Environment is free.
	// +optional
	Holder *Holder `json:"holder,omitempty"`
	// Queue are the grants waiting for a claim, oldest first.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Queue []Grant `json:"queue,omitempty"`
	// QueueLength is len(Queue), for the printer column.
	// +optional
	QueueLength int `json:"queueLength,omitempty"`
	// Released is when the Environment was last released; a grant's TTL runs
	// from the later of it and the grant.
	// +optional
	Released *metav1.Time `json:"released,omitempty"`
	// +optional
	Upgrade *Upgrade `json:"upgrade,omitempty"`
	// Alerts counts the firing alerts per severity; the alerts stay in
	// Alertmanager.
	// +optional
	Alerts map[string]int `json:"alerts,omitempty"`
	// Holds names the active Holds on the Environment.
	// +optional
	Holds []string `json:"holds,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Environment is one Giant Swarm installation shared by every team.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=env
// +kubebuilder:printcolumn:name="Holder",type=string,JSONPath=`.status.holder.party.name`
// +kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.status.holder.purpose`
// +kubebuilder:printcolumn:name="Since",type=date,JSONPath=`.status.holder.since`
// +kubebuilder:printcolumn:name="Upgrade",type=string,JSONPath=`.status.upgrade.to`
// +kubebuilder:printcolumn:name="Queue",type=integer,JSONPath=`.status.queueLength`
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EnvironmentSpec `json:"spec"`
	// +optional
	Status EnvironmentStatus `json:"status,omitempty"`
}

// EnvironmentList is a list of Environments.
//
// +kubebuilder:object:root=true
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Environment `json:"items"`
}

// LaneEntry is one merge in a lane.
type LaneEntry struct {
	By   Party  `json:"by"`
	Repo string `json:"repo"`
	// PR is the pull request, 0 for a promotion.
	// +optional
	PR int `json:"pr,omitempty"`
	// Phase is waiting, running or settling.
	// +kubebuilder:validation:Enum=waiting;running;settling
	Phase string `json:"phase"`
	// Arrived orders the queue.
	Arrived metav1.Time `json:"arrived"`
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// Finished and Exit are when and how the merge's run ended.
	// +optional
	Finished *metav1.Time `json:"finished,omitempty"`
	// +optional
	Exit int `json:"exit,omitempty"`
	// Release is the tag the merge released.
	// +optional
	Release string `json:"release,omitempty"`
	// Roll names the HelmReleases (namespace/name) that must reach Release
	// before the lane frees.
	// +optional
	Roll []string `json:"roll,omitempty"`
}

// MergeLaneSpec declares which repositories roll onto an installation.
type MergeLaneSpec struct {
	// +optional
	Installation string `json:"installation,omitempty"`
	// +optional
	Repositories []string `json:"repositories,omitempty"`
}

// MergeLaneStatus is the lane's queue.
type MergeLaneStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Running is the merge running, owner/repo#n.
	// +optional
	Running string `json:"running,omitempty"`
	// Settling is the merge settling, owner/repo#n.
	// +optional
	Settling string `json:"settling,omitempty"`
	// Queue are the lane's merges in arrival order.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Queue []LaneEntry `json:"queue,omitempty"`
	// +optional
	QueueLength int `json:"queueLength,omitempty"`
	// +optional
	Stalled bool `json:"stalled,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MergeLane serializes the merges that roll onto one installation.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=lane
// +kubebuilder:printcolumn:name="Installation",type=string,JSONPath=`.spec.installation`
// +kubebuilder:printcolumn:name="Running",type=string,JSONPath=`.status.running`
// +kubebuilder:printcolumn:name="Settling",type=string,JSONPath=`.status.settling`
// +kubebuilder:printcolumn:name="Queue",type=integer,JSONPath=`.status.queueLength`
// +kubebuilder:printcolumn:name="Stalled",type=boolean,JSONPath=`.status.stalled`
type MergeLane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec MergeLaneSpec `json:"spec,omitempty"`
	// +optional
	Status MergeLaneStatus `json:"status,omitempty"`
}

// MergeLaneList is a list of MergeLanes.
//
// +kubebuilder:object:root=true
type MergeLaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MergeLane `json:"items"`
}

// HoldSpec stops work on a target until it is lifted or Until passes.
type HoldSpec struct {
	// Target is an Environment, a MergeLane or a repository.
	Target string `json:"target"`
	Reason string `json:"reason"`
	// +optional
	Until *metav1.Time `json:"until,omitempty"`
	// Except is what a merge hold lets through: a repository or one pull
	// request, owner/repo#n.
	// +optional
	Except string `json:"except,omitempty"`
	// UpgradeTo is the target release of the upgrade the hold stands for.
	// +optional
	UpgradeTo string `json:"upgradeTo,omitempty"`
}

// HoldStatus is who set the hold and whether it still applies.
type HoldStatus struct {
	// +optional
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	By                 Party       `json:"by"`
	At                 metav1.Time `json:"at"`
	// Active is false once the hold is lifted or expired.
	Active bool `json:"active"`
	// +optional
	LiftedBy *Party `json:"liftedBy,omitempty"`
	// +optional
	LiftedAt *metav1.Time `json:"liftedAt,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Hold stops work on an Environment, a MergeLane or a repository.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Until",type=date,JSONPath=`.spec.until`
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="By",type=string,JSONPath=`.status.by.name`
type Hold struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HoldSpec `json:"spec"`
	// +optional
	Status HoldStatus `json:"status,omitempty"`
}

// HoldList is a list of Holds.
//
// +kubebuilder:object:root=true
type HoldList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Hold `json:"items"`
}

// NoteSpec is a question for a person, or a deadline.
type NoteSpec struct {
	// ID is the note's number, unique on the installation.
	// +kubebuilder:validation:Minimum=1
	ID int `json:"id"`
	// For is the person or team the note asks.
	// +optional
	For  string `json:"for,omitempty"`
	Text string `json:"text"`
	// +optional
	Due *metav1.Time `json:"due,omitempty"`
	// Default is what happens when nobody answers by Due.
	// +optional
	Default string `json:"default,omitempty"`
	// Kind is decision, memo or login.
	// +optional
	Kind string `json:"kind,omitempty"`
	// Until is the shell command a login note closes on.
	// +optional
	Until string `json:"until,omitempty"`
	// Pinned is a standing instruction carried by every hand-over.
	// +optional
	Pinned bool `json:"pinned,omitempty"`
	// Refs are the issues and pull requests the note asks about.
	// +optional
	Refs []string `json:"refs,omitempty"`
	// Environment is the Environment the note concerns.
	// +optional
	Environment string `json:"environment,omitempty"`
	// Question is a decision's question, one line.
	// +kubebuilder:validation:MaxLength=150
	// +optional
	Question string `json:"question,omitempty"`
	// StatusQuo is what is true now and why the decision arises.
	// +kubebuilder:validation:MaxLength=3000
	// +optional
	StatusQuo string `json:"statusQuo,omitempty"`
	// Options are a decision's choices, "<label>: <consequence>" each.
	// +kubebuilder:validation:MaxItems=10
	// +optional
	Options []string `json:"options,omitempty"`
	// Recommend is the recommended option, 1-based.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Recommend int `json:"recommend,omitempty"`
}

// NoteStatus is who filed the note and where it stands.
type NoteStatus struct {
	// +optional
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	By                 Party       `json:"by"`
	At                 metav1.Time `json:"at"`
	// State is open, answered, defaulted or closed.
	// +kubebuilder:validation:Enum=open;answered;defaulted;closed
	// +optional
	State string `json:"state,omitempty"`
	// Posted is klaus-gateway's handle on the decision's message.
	// +optional
	Posted string `json:"posted,omitempty"`
	// Answer is the answer word for word.
	// +optional
	Answer string `json:"answer,omitempty"`
	// +optional
	AnsweredBy string `json:"answeredBy,omitempty"`
	// +optional
	AnsweredAt *metav1.Time `json:"answeredAt,omitempty"`
	// Fired is when a watch reported the note due.
	// +optional
	Fired *metav1.Time `json:"fired,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Note is a question for a person or team, in the filer's team namespace.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ID",type=integer,JSONPath=`.spec.id`
// +kubebuilder:printcolumn:name="For",type=string,JSONPath=`.spec.for`
// +kubebuilder:printcolumn:name="Due",type=date,JSONPath=`.spec.due`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="By",type=string,JSONPath=`.status.by.name`
type Note struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NoteSpec `json:"spec"`
	// +optional
	Status NoteStatus `json:"status,omitempty"`
}

// NoteList is a list of Notes.
//
// +kubebuilder:object:root=true
type NoteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Note `json:"items"`
}

// RosterEntrySpec is a local agent a person's machine publishes.
type RosterEntrySpec struct {
	// Address is the agent's address, local:<machine>/<name>.
	Address string `json:"address"`
	// Party is the agent's session.
	Party Party `json:"party"`
	// +optional
	Harness string `json:"harness,omitempty"`
	// Registered is when the agent joined the roster.
	Registered metav1.Time `json:"registered"`
}

// RosterEntryStatus is what the agent works on, written on transitions only.
type RosterEntryStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Task string `json:"task,omitempty"`
	// State is idle, busy, waiting-for-approval or ended.
	// +kubebuilder:validation:Enum=idle;busy;waiting-for-approval;ended
	// +optional
	State string `json:"state,omitempty"`
	// IdleSince is when the agent last went idle.
	// +optional
	IdleSince *metav1.Time `json:"idleSince,omitempty"`
	// Cwd is the agent's checkout.
	// +optional
	Cwd string `json:"cwd,omitempty"`
	// Conversation is klaus-gateway's conversation the agent holds with its
	// person, a Slack thread.
	// +optional
	Conversation string `json:"conversation,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RosterEntry is one local agent in its team's namespace.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Person",type=string,JSONPath=`.spec.party.person`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Task",type=string,JSONPath=`.status.task`
type RosterEntry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RosterEntrySpec `json:"spec"`
	// +optional
	Status RosterEntryStatus `json:"status,omitempty"`
}

// RosterEntryList is a list of RosterEntries.
//
// +kubebuilder:object:root=true
type RosterEntryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RosterEntry `json:"items"`
}

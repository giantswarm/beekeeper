package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Hold gates (contract §1.2).
const (
	GateTeamCap         = "TeamCap"
	GateEpicWIP         = "EpicWIP"
	GateRepoConcurrency = "RepoConcurrency"
)

// OptIn says which work a team hands to the engine.
type OptIn struct {
	// Work lists the classes of work, e.g. implementable.
	Work   []string `json:"work,omitempty"`
	Alerts bool     `json:"alerts,omitempty"`
}

// Cap is an optional limit across a team's participants.
type Cap struct {
	Items *int `json:"items,omitempty"`
	Epics *int `json:"epics,omitempty"`
}

// TeamSpec configures a team.
type TeamSpec struct {
	// BoardOption is the team's option of the board's Team field.
	BoardOption string `json:"boardOption"`
	GithubTeam  string `json:"githubTeam,omitempty"`
	OptIn       OptIn  `json:"optIn,omitempty"`
	Cap         *Cap   `json:"cap,omitempty"`
}

// TeamPick is an item the engine would start for a person now.
type TeamPick struct {
	Person string `json:"person"`
	Item   string `json:"item"`
}

// TeamHeld is an item a gate holds for a person.
type TeamHeld struct {
	Person string `json:"person"`
	Item   string `json:"item"`
	Gate   string `json:"gate"`
}

// TeamStatus is the result of the team's last decision pass.
type TeamStatus struct {
	// InFlight counts the team's implementable items in Implementing through Validating.
	InFlight int `json:"inFlight"`
	// WIP is the engine's own WIP across the team.
	WIP            WIP          `json:"wip"`
	Picks          []TeamPick   `json:"picks,omitempty"`
	Held           []TeamHeld   `json:"held,omitempty"`
	LastDecisionAt *metav1.Time `json:"lastDecisionAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// Team is a team that opted in to the engine.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TeamSpec   `json:"spec,omitempty"`
	Status TeamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TeamList lists Teams.
type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

// PersonSpec configures a participant.
type PersonSpec struct {
	Login string   `json:"login"`
	Teams []string `json:"teams,omitempty"`
	WIP   WIP      `json:"wip"`
}

// PersonHeld is an item a gate holds for this person.
type PersonHeld struct {
	Item string `json:"item"`
	Gate string `json:"gate"`
}

// PersonStatus is the person's share of the last decision pass.
type PersonStatus struct {
	// WIP is the engine's own WIP for this person.
	WIP WIP `json:"wip"`
	// Free is the limit minus engine WIP.
	Free  WIP          `json:"free"`
	Picks []string     `json:"picks,omitempty"`
	Held  []PersonHeld `json:"held,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// Person is a participant: someone the engine may pick work for.
type Person struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersonSpec   `json:"spec,omitempty"`
	Status PersonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PersonList lists Persons.
type PersonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Person `json:"items"`
}

func init() {
	register(&Team{}, &TeamList{}, &Person{}, &PersonList{})
}

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// BudgetName is the name of the GitHub App's Budget object.
const BudgetName = "github-app"

// APIBudget is one API's rate limit as GitHub last reported it.
type APIBudget struct {
	Limit     int          `json:"limit"`
	Remaining int          `json:"remaining"`
	ResetAt   *metav1.Time `json:"resetAt,omitempty"`
	// Used counts calls (REST, app) or points (GraphQL) spent by the engine in the current window.
	Used int `json:"used,omitempty"`
}

// Deferred says which APIs observation reads are deferred on: below the observation reserve, or
// stopped by a primary or secondary rate limit, until the API's reset.
type Deferred struct {
	REST    bool `json:"rest"`
	GraphQL bool `json:"graphql"`
	App     bool `json:"app,omitempty"`
}

// BudgetStatus is the GitHub App's API budget.
type BudgetStatus struct {
	REST     APIBudget `json:"rest"`
	GraphQL  APIBudget `json:"graphql"`
	App      APIBudget `json:"app"`
	Deferred Deferred  `json:"deferred"`
}

// AppBudgetSpec is empty: the Budget is engine-owned.
type AppBudgetSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced

// Budget is the API budget of a GitHub App.
type Budget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppBudgetSpec `json:"spec,omitempty"`
	Status BudgetStatus  `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BudgetList lists Budgets.
type BudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Budget `json:"items"`
}

func init() {
	register(&Budget{}, &BudgetList{})
}

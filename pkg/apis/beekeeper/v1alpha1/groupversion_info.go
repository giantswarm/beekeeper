// Package v1alpha1 is the beekeeper.giantswarm.io API: the shared
// coordination state `beekeeper serve` keeps for the Giant Swarm
// installations. Each kind declares little in its spec and records the
// runtime state in its status, which only beekeeper writes.
//
// +kubebuilder:object:generate=true
// +groupName=beekeeper.giantswarm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of these kinds.
	GroupVersion = schema.GroupVersion{Group: "beekeeper.giantswarm.io", Version: "v1alpha1"}

	// SchemeBuilder registers the kinds with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the kinds to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Environment{}, &EnvironmentList{},
		&MergeLane{}, &MergeLaneList{},
		&Hold{}, &HoldList{},
		&Note{}, &NoteList{},
		&RosterEntry{}, &RosterEntryList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

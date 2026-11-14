// Package v1alpha1 holds the engine's API: configuration (Installation, Team, Person, applied by Flux)
// and engine-owned state (Item, Change, Budget, everything in status).
// +kubebuilder:object:generate=true
// +groupName=beekeeper.giantswarm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version of every engine kind.
	GroupVersion = schema.GroupVersion{Group: "beekeeper.giantswarm.io", Version: "v1alpha1"}

	// SchemeBuilder registers the engine kinds.
	SchemeBuilder runtime.SchemeBuilder

	// AddToScheme adds the engine kinds to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// register adds the kinds to GroupVersion when a scheme is built, with the meta types every group
// version carries. Each kind's file calls it from init.
func register(objs ...runtime.Object) {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, objs...)
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}

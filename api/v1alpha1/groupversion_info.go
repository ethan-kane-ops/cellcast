// Package v1alpha1 contains the v1alpha1 API types for cellcast.
//
// The API is versioned from the start so that a v1beta1 can be additive rather
// than breaking. Field-level semantics are owned by the tickets that implement
// them: ENG-110 for Cluster, ENG-173 for PlacementPolicy.
//
// This package depends only on apimachinery. Keeping controller-runtime out of
// it means anything that needs the types (the client CLI, a downstream
// consumer) can import them without pulling a controller framework along.
//
// +kubebuilder:object:generate=true
// +groupName=cellcast.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "cellcast.io", Version: "v1alpha1"}

// SchemeBuilder registers the Go types in this package with a scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the types in this package to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

// Resource returns a GroupResource for the named resource in this group.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Cluster{}, &ClusterList{},
		&PlacementPolicy{}, &PlacementPolicyList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

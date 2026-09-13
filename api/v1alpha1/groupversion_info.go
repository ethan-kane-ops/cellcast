// Package v1alpha1 contains the v1alpha1 API types for cellcast.
//
// Deprecated: use package v1beta1. This version is still served so that
// manifests written against it keep applying, and it mirrors v1beta1 field for
// field because conversion between the two is None. Nothing in this repository
// reads it; it exists so that controller-gen emits the served version.
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
		&TrustConfig{}, &TrustConfigList{},
		&WorkloadPlacement{}, &WorkloadPlacementList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

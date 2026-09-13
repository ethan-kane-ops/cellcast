// Package v1beta1 contains the v1beta1 API types for cellcast: the version the
// hub reads and the API server stores. Field-level semantics live on the types
// themselves.
//
// v1alpha1 is still served, with an identical schema, so manifests written
// against it keep applying. Conversion between the two is None: the API server
// rewrites apiVersion and nothing else, which is sound only while the schemas
// match field for field, and TestServedVersionsShareOneSchema holds them to
// that. A field added here goes into api/v1alpha1 as well until v1alpha1 is
// removed. A field that has to differ between them needs a conversion webhook,
// which is a decision rather than an edit (docs/architecture.md ADR-013).
//
// This package depends only on apimachinery. Keeping controller-runtime out of
// it means anything that needs the types (the client CLI, a downstream
// consumer) can import them without pulling a controller framework along.
//
// +kubebuilder:object:generate=true
// +groupName=cellcast.io
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "cellcast.io", Version: "v1beta1"}

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

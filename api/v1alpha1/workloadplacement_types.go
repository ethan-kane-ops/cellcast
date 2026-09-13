package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadPlacementSpec is the cell one workload was last placed in, under one
// policy.
type WorkloadPlacementSpec struct {
	// Policy is the PlacementPolicy the workload was placed under.
	//
	// Part of the key, because a workload name is the caller's own string: the
	// same name under another policy is another team's workload, and one team's
	// deploys must not decide where another's land.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="policy is immutable"
	Policy string `json:"policy"`

	// Workload is the name the caller placed, verbatim.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workload is immutable"
	Workload string `json:"workload"`

	// Cell is the Cluster the workload was last placed in. The next placement
	// prefers it while it stays permitted, eligible and reporting capacity.
	//
	// An operator may edit it to move a workload on its next deploy. The new
	// cell still has to pass the same filter, so the edit moves a workload
	// between cells its policy permits and nowhere else.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Cell string `json:"cell"`

	// LastPlacedAt is when a placement last landed in Cell, to within an hour.
	// The hub rewrites it at most hourly, so a workload deployed every few
	// minutes is not an API server write every few minutes.
	LastPlacedAt metav1.Time `json:"lastPlacedAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ccwp
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policy`
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workload`
// +kubebuilder:printcolumn:name="Cell",type=string,JSONPath=`.spec.cell`
// +kubebuilder:printcolumn:name="Last Placed",type=date,JSONPath=`.spec.lastPlacedAt`

// WorkloadPlacement is the cell a workload was last placed in, which the next
// placement of the same workload under the same policy prefers.
//
// Written by the hub once a credential has been minted, never by a caller and
// never by a dry run. Deleting one makes the next placement decide afresh. It
// holds names and a timestamp: no credential, and nothing a caller sent beyond
// the workload's name. See docs/architecture.md ADR-012.
type WorkloadPlacement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkloadPlacementSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// WorkloadPlacementList contains a list of WorkloadPlacement.
type WorkloadPlacementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadPlacement `json:"items"`
}

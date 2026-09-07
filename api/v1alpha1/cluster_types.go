package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Provider identifies the platform hosting a cell.
// +kubebuilder:validation:Enum=eks;gke;aks;generic
type Provider string

const (
	ProviderEKS     Provider = "eks"
	ProviderGKE     Provider = "gke"
	ProviderAKS     Provider = "aks"
	ProviderGeneric Provider = "generic"
)

// ClusterState is the operator-declared placement state of a cell.
//
// The three values are not interchangeable and the middle one is the reason
// there are three. See docs/architecture.md ADR-008.
//
//	LIVE      receives new placements, serves production traffic
//	DARK      receives placements only when explicitly requested, serves no production traffic
//	DRAINING  receives no new placements, still serves production traffic
//
// +kubebuilder:validation:Enum=LIVE;DARK;DRAINING
type ClusterState string

const (
	// ClusterStateLive is the default: eligible for placement.
	ClusterStateLive ClusterState = "LIVE"
	// ClusterStateDark is reachable only by a caller that explicitly asks for a
	// dark cell, which is what makes QA smoke tests on a dark cell safe.
	ClusterStateDark ClusterState = "DARK"
	// ClusterStateDraining is the upgrade-window state. Existing workloads keep
	// serving; new deploys route elsewhere without anyone editing a pipeline.
	ClusterStateDraining ClusterState = "DRAINING"
)

// TrustConfigReference names the trust configuration used to mint credentials
// for this cell. It is a reference, never inline material: a Cluster never
// carries a credential. See docs/architecture.md ADR-004.
//
// The contents of the referenced object are defined by ENG-113.
type TrustConfigReference struct {
	// Name of the trust configuration object in the hub namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ClusterSpec is the durable registry entry for one cell.
type ClusterSpec struct {
	// Endpoint is the Kubernetes API server URL for this cell.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://`
	Endpoint string `json:"endpoint"`

	// CABundle is the PEM-encoded certificate authority bundle used to verify
	// the endpoint. Omitted when the endpoint presents a publicly trusted
	// certificate.
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`

	// Provider identifies the platform hosting this cell.
	Provider Provider `json:"provider"`

	// TrustConfigRef references the trust configuration used to mint
	// credentials for this cell.
	TrustConfigRef TrustConfigReference `json:"trustConfigRef"`

	// State is the operator-declared placement state. Defaults to LIVE.
	//
	// This is spec rather than status because an operator sets it: `kubectl
	// patch` is a supported interface and every transition lands in the API
	// server audit log.
	// +kubebuilder:default=LIVE
	// +optional
	State ClusterState `json:"state,omitempty"`
}

// ClusterStatus is the observed state of a cell.
//
// Deliberately absent: anything that changes on every agent heartbeat.
// Capacity is high-write data with no durability requirement and lives in
// memory in the hub process (docs/architecture.md ADR-002). Writing heartbeat
// timestamps here would put a write against etcd on every heartbeat from every
// cell in the estate, which is exactly what ADR-002 exists to avoid.
type ClusterStatus struct {
	// ObservedState is the state the hub is currently acting on.
	// +optional
	ObservedState ClusterState `json:"observedState,omitempty"`

	// ObservedGeneration is the .metadata.generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StateSince is when the hub last observed the state change.
	//
	// Not derivable from the AcceptingPlacements condition: DARK and DRAINING
	// both leave that condition False, so a cell moved from DARK straight to
	// DRAINING has no condition transition to read. During an upgrade window
	// "how long has this cell been draining" is the question being asked.
	// +optional
	StateSince *metav1.Time `json:"stateSince,omitempty"`

	// Conditions describe the cell's current condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cc
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Accepting",type=string,JSONPath=`.status.conditions[?(@.type=="AcceptingPlacements")].status`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Cluster is a registered cell in the fleet.
//
// Creating a Cluster is a privileged action. Whoever can create one can point
// cellcast at an endpoint they control and attract real deploys to it. See
// docs/threat-model.md T-04.
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster.
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

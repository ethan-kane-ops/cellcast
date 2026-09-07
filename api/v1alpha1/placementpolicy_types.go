package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScoringStrategy selects between placements that are already permitted and
// eligible. It never widens the candidate set.
// +kubebuilder:validation:Enum=LeastLoaded;RoundRobin
type ScoringStrategy string

const (
	// ScoringLeastLoaded picks the survivor with the lowest committed
	// utilisation.
	ScoringLeastLoaded ScoringStrategy = "LeastLoaded"
	// ScoringRoundRobin cycles through survivors deterministically.
	ScoringRoundRobin ScoringStrategy = "RoundRobin"
)

// SubjectSelector matches an authenticated caller.
//
// Claims are matched against the values extracted from the caller's workload
// identity token by the provider that owns the issuer. All entries must match;
// an empty Claims map matches any caller from the issuer, which is almost
// always too broad for anything but a development policy.
type SubjectSelector struct {
	// Issuer is the OIDC issuer URL the caller's token must come from. It must
	// also be on the hub's issuer allowlist; naming it here does not add it.
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`

	// Claims are exact-match requirements on the extracted caller claims,
	// for example {"repository": "ethan-kane-ops/cellcast"}.
	// +optional
	Claims map[string]string `json:"claims,omitempty"`
}

// TokenTTLPolicy bounds the lifetime of credentials minted under this policy.
//
// A caller may request a shorter TTL than Default. A caller may never request
// one longer than Max. See docs/architecture.md ADR-004.
type TokenTTLPolicy struct {
	// Default is the TTL granted when the caller does not ask for one.
	// +optional
	Default *metav1.Duration `json:"default,omitempty"`

	// Max is the ceiling no request can raise. This is the operator-set bound
	// that limits the blast radius of a compromised hub process
	// (docs/threat-model.md T-01).
	// +optional
	Max *metav1.Duration `json:"max,omitempty"`
}

// PlacementPolicySpec maps authenticated callers to the cells they may reach.
type PlacementPolicySpec struct {
	// Subjects are the callers this policy applies to. A request matching no
	// policy is rejected; there is no implicit "any cell" fallback.
	// +kubebuilder:validation:MinItems=1
	Subjects []SubjectSelector `json:"subjects"`

	// PermittedCells selects the Cluster resources matching callers may reach.
	// This is the filter step, and it runs before any scoring. Running scoring
	// first is how a dev pipeline lands in the least-loaded production cluster
	// (docs/architecture.md ADR-005, docs/threat-model.md T-03).
	PermittedCells metav1.LabelSelector `json:"permittedCells"`

	// Strategy selects among permitted, eligible cells.
	//
	// Chosen by policy rather than per request on purpose: a caller that can
	// pick the strategy can steer itself into a specific cell.
	// +kubebuilder:default=LeastLoaded
	// +optional
	Strategy ScoringStrategy `json:"strategy,omitempty"`

	// TokenTTL bounds credential lifetime for placements made under this
	// policy.
	// +optional
	TokenTTL *TokenTTLPolicy `json:"tokenTTL,omitempty"`

	// AllowDarkTargeting permits matching callers to explicitly request a DARK
	// cell. Off by default: dark cells exist so that QA can smoke-test without
	// production traffic, and a pipeline reaching one by accident defeats that.
	// +kubebuilder:default=false
	// +optional
	AllowDarkTargeting bool `json:"allowDarkTargeting,omitempty"`
}

// PlacementPolicyStatus is the observed state of a policy.
type PlacementPolicyStatus struct {
	// ObservedGeneration is the .metadata.generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe the policy's current condition, including whether its
	// selector currently matches any registered cell.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ccpol
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.strategy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PlacementPolicy authorizes a set of callers to a set of cells.
//
// Policy is a custom resource rather than hub configuration so that it is
// GitOps-managed, reviewable in a pull request, and auditable through the API
// server. Policy nobody can diff is policy that drifts.
type PlacementPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlacementPolicySpec   `json:"spec,omitempty"`
	Status PlacementPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlacementPolicyList contains a list of PlacementPolicy.
type PlacementPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlacementPolicy `json:"items"`
}

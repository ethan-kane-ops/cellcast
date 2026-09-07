package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TrustProvider identifies how credentials are minted for a cell.
//
// Both values exist from the first release even though only one is
// implemented. A trust provider that is named but unimplemented is rejected at
// apply time by the TrustConfig controller, which is the difference between an
// operator seeing the gap when they configure it and a pipeline discovering it
// mid-deploy.
// +kubebuilder:validation:Enum=kubernetes;aws
type TrustProvider string

const (
	// TrustProviderKubernetes mints via the spoke's TokenRequest API. The only
	// provider implemented in v0.1.
	TrustProviderKubernetes TrustProvider = "kubernetes"
	// TrustProviderAWS mints via AWS STS AssumeRole. Lands in v0.2 behind this
	// same interface (docs/architecture.md ADR-004).
	TrustProviderAWS TrustProvider = "aws"
)

// SecretKeyReference names one key in one Secret in the hub namespace.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key holding the material.
	// +kubebuilder:default=kubeconfig
	// +optional
	Key string `json:"key,omitempty"`
}

// CredentialSource is how the hub authenticates to a cell in order to mint.
//
// This is the hub's own identity in the spoke, not the credential handed to the
// caller. The two are different things and conflating them is the fastest way
// to misread the security model: what cellcast never stores is the credential
// it returns. It necessarily holds some way to reach each spoke, exactly as any
// multi-cluster control plane does, and that identity is bounded by the RBAC
// the operator grants it there (docs/threat-model.md T-08).
//
// +kubebuilder:validation:XValidation:rule="(has(self.inCluster) && self.inCluster) != has(self.secretRef)",message="exactly one of inCluster or secretRef must be set"
type CredentialSource struct {
	// InCluster mints against the cluster the hub itself runs in, using the
	// hub's own projected service account token.
	//
	// This is the only configuration that stores no credential at all, and it
	// is what makes the single-cluster demo reproducible with nothing to leak.
	// +optional
	InCluster bool `json:"inCluster,omitempty"`

	// SecretRef names a Secret holding a kubeconfig for the cell.
	//
	// Read uncached at mint time and never retained, so the hub's memory does
	// not hold a resident map of every spoke credential.
	// +optional
	SecretRef *SecretKeyReference `json:"secretRef,omitempty"`
}

// KubernetesTrust configures minting through a spoke's TokenRequest API.
type KubernetesTrust struct {
	// ServiceAccountName is the service account in the cell that tokens are
	// minted for. The permissions the caller receives are exactly this account's
	// permissions, so this name is the RBAC boundary and belongs in review.
	// +kubebuilder:validation:MinLength=1
	ServiceAccountName string `json:"serviceAccountName"`

	// Namespace is the namespace holding that service account.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Audiences the minted token is valid for.
	//
	// Empty means the cell's API server audience, which is what makes the token
	// usable as a kubeconfig credential. Setting this to anything else produces
	// a token the target API server will refuse.
	// +optional
	Audiences []string `json:"audiences,omitempty"`
}

// TrustConfigSpec declares how credentials are minted for the cells that
// reference it.
//
// +kubebuilder:validation:XValidation:rule="self.provider != 'kubernetes' || has(self.kubernetes)",message="spec.kubernetes is required when provider is kubernetes"
type TrustConfigSpec struct {
	// Provider selects the minting mechanism.
	Provider TrustProvider `json:"provider"`

	// CredentialSource is how the hub reaches the cell to mint.
	CredentialSource CredentialSource `json:"credentialSource"`

	// Kubernetes configures the TokenRequest provider.
	// +optional
	Kubernetes *KubernetesTrust `json:"kubernetes,omitempty"`
}

// Conditions and reasons published by the TrustConfig controller.
const (
	// TrustConfigConditionReady reports whether this config could mint if asked
	// right now, as far as can be determined without minting.
	TrustConfigConditionReady = "Ready"

	// TrustReasonValid means the configuration resolves.
	TrustReasonValid = "Valid"
	// TrustReasonProviderNotImplemented means the named provider has no
	// implementation in this build.
	TrustReasonProviderNotImplemented = "ProviderNotImplemented"
	// TrustReasonCredentialMissing means the referenced Secret or key is absent.
	TrustReasonCredentialMissing = "CredentialMissing"
	// TrustReasonInvalidConfiguration means the spec is internally inconsistent.
	TrustReasonInvalidConfiguration = "InvalidConfiguration"
)

// TrustConfigStatus is the observed state of a trust configuration.
type TrustConfigStatus struct {
	// ObservedGeneration is the .metadata.generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe whether this configuration is usable.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cctrust
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Detail",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TrustConfig declares how the hub mints credentials for a set of cells.
//
// It holds configuration, never credential material: the fields here name a
// service account and a namespace, and the only secret it can reference is the
// hub's own way in to the spoke. Nothing a caller receives is stored anywhere
// in this resource. See docs/architecture.md ADR-004.
type TrustConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrustConfigSpec   `json:"spec,omitempty"`
	Status TrustConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TrustConfigList contains a list of TrustConfig.
type TrustConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrustConfig `json:"items"`
}

// Package apitest runs the CRD and controller layer against a real
// kube-apiserver and etcd, via controller-runtime's envtest.
//
// It exists because the fake client the unit tests use does not enforce the
// CRD schema. Every `+kubebuilder:validation:` marker, every `+kubebuilder:
// default=`, both CEL rules on TrustConfig and the status subresource are
// promises made by the generated manifests in config/crd/bases, and a fake
// client accepts objects that a real API server refuses. The unit tests can
// only show that the controllers behave correctly on input that reached them;
// these show which input can reach them at all.
//
// It also supersedes the throwaway kind cluster that `just verify-crds` used
// to spin up. Establishing the CRDs is the first thing the harness does, so
// every test in this package depends on the manifests installing cleanly.
//
// The package holds no non-test code. This file exists so that the directory
// builds, matching internal/boundaries.
package apitest

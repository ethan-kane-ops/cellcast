package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestAddToSchemeRegistersAllTypes guards the hand-written addKnownTypes
// against a type being added to the package and forgotten here. A type missing
// from the scheme fails at runtime on first use, not at compile time.
func TestAddToSchemeRegistersAllTypes(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme() = %v, want nil", err)
	}

	for _, kind := range []string{"Cluster", "ClusterList", "PlacementPolicy", "PlacementPolicyList"} {
		t.Run(kind, func(t *testing.T) {
			gvk := GroupVersion.WithKind(kind)
			if _, err := s.New(gvk); err != nil {
				t.Fatalf("scheme.New(%s) = %v, want the type to be registered", gvk, err)
			}
		})
	}
}

func TestResource(t *testing.T) {
	got := Resource("clusters")
	if got.Group != "cellcast.io" || got.Resource != "clusters" {
		t.Fatalf("Resource(\"clusters\") = %+v, want group cellcast.io resource clusters", got)
	}
}

// TestDeepCopyIsIndependent asserts the generated deepcopy actually copies
// rather than aliasing. A shallow copy of a Cluster would let one reconcile
// mutate another's cached object.
func TestDeepCopyIsIndependent(t *testing.T) {
	original := &Cluster{
		Spec: ClusterSpec{
			Endpoint:       "https://cell-01.example.test",
			Provider:       ProviderEKS,
			State:          ClusterStateLive,
			CABundle:       []byte("original"),
			TrustConfigRef: TrustConfigReference{Name: "cell-01-trust"},
		},
	}

	clone := original.DeepCopy()
	clone.Spec.CABundle[0] = 'X'
	clone.Spec.State = ClusterStateDraining

	if original.Spec.CABundle[0] == 'X' {
		t.Error("mutating the clone's CABundle changed the original; deepcopy is aliasing the slice")
	}
	if original.Spec.State != ClusterStateLive {
		t.Errorf("original state = %q, want %q", original.Spec.State, ClusterStateLive)
	}
}

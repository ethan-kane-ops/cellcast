package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// TestAddToSchemeRegistersAllTypes guards the hand-written addKnownTypes
// against a type being added to the package and forgotten here. A type missing
// from the scheme fails at runtime on first use, not at compile time.
//
// The kinds come from the generated CRDs rather than from a list written out
// here. A hardcoded list cannot detect the mistake it is supposed to catch: it
// only ever checks the types somebody remembered to add to it, which is the
// same set they remembered to register. Reading config/crd/bases means adding a
// root type and running `just manifests` fails this test until the type reaches
// the scheme too.
func TestAddToSchemeRegistersAllTypes(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme() = %v, want nil", err)
	}

	kinds := generatedKinds(t)
	if len(kinds) == 0 {
		t.Fatal("found no generated CRDs; this test cannot verify anything")
	}

	for _, kind := range kinds {
		for _, name := range []string{kind, kind + "List"} {
			t.Run(name, func(t *testing.T) {
				gvk := GroupVersion.WithKind(name)
				if _, err := s.New(gvk); err != nil {
					t.Fatalf("scheme.New(%s) = %v, want the type to be registered", gvk, err)
				}
			})
		}
	}
}

// generatedKinds returns the kind of every CRD controller-gen produced.
func generatedKinds(t *testing.T) []string {
	t.Helper()

	const dir = "../../config/crd/bases"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	var kinds []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}

		var crd struct {
			Spec struct {
				Group string `json:"group"`
				Names struct {
					Kind string `json:"kind"`
				} `json:"names"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		if crd.Spec.Group == GroupVersion.Group && crd.Spec.Names.Kind != "" {
			kinds = append(kinds, crd.Spec.Names.Kind)
		}
	}
	return kinds
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

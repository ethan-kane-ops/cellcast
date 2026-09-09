package apitest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// exampleKinds are the kinds examples/ is expected to cover. Listed rather than
// discovered, so that deleting a file reduces the fleet loudly instead of
// quietly reducing what this test checks.
var exampleKinds = []string{"Cluster", "PlacementPolicy", "TrustConfig"}

// exampleManifests returns every Kubernetes manifest under examples/, including
// the ones in the integration subdirectories.
//
// Only .yaml files. The CI pipeline files beside them are .yml on purpose: a
// GitHub workflow and a Buildkite pipeline are YAML that no API server has an
// opinion about, and handing one to this test would fail on its first document.
func exampleManifests(t *testing.T, root string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking examples/: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("examples/ holds no manifests, so this test checked nothing")
	}
	return out
}

// TestTheExamplesAreAcceptedByTheAPIServer is the difference between an example
// somebody can apply and an example somebody has to debug.
//
// Manifests in a README rot in one direction only: the schema moves, the prose
// stays, and the first person to find out is a reader whose very first command
// against cellcast fails. Every validation the CRDs carry runs here, including
// both CEL rules on TrustConfig and the enums on Cluster, so an example that has
// drifted fails the suite rather than the reader.
func TestTheExamplesAreAcceptedByTheAPIServer(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	// Relative to the package, like the chart paths in this suite.
	dir := filepath.Join("..", "..", "examples")

	var seen []string
	for _, name := range exampleManifests(t, dir) {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}

			var count int
			for _, doc := range splitDocs(string(raw)) {
				obj := &unstructured.Unstructured{}
				if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
					t.Fatalf("parsing a document in %s: %v\n%s", name, err, doc)
				}
				if obj.GetKind() == "" {
					continue
				}
				count++
				if !slices.Contains(seen, obj.GetKind()) {
					seen = append(seen, obj.GetKind())
				}

				// The examples name cellcast-system, which does not exist in
				// the control plane this suite runs. The namespace is not what
				// is under test; the schema is.
				obj.SetNamespace(ns)

				// DryRunAll runs validation, defaulting and CEL and persists
				// nothing, so the objects can reference Secrets that are not
				// here without the test having to invent them.
				if err := c.Create(context.Background(), obj, client.DryRunAll); err != nil {
					t.Errorf("the API server refused %s %s from examples/%s: %v",
						obj.GetKind(), obj.GetName(), name, err)
				}
			}
			if count == 0 {
				t.Errorf("examples/%s holds no objects, so it checked nothing", name)
			}
		})
	}

	for _, kind := range exampleKinds {
		if !slices.Contains(seen, kind) {
			t.Errorf("examples/ holds no %s; a reader cannot assemble a working fleet from it", kind)
		}
	}
}

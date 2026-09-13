package v1beta1

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

// crdVersions is the part of a generated CRD that decides what each served
// version looks like.
type crdVersions struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Conversion *struct {
			Strategy string `json:"strategy"`
		} `json:"conversion"`
		Versions []struct {
			Name                     string           `json:"name"`
			Served                   bool             `json:"served"`
			Storage                  bool             `json:"storage"`
			Deprecated               bool             `json:"deprecated"`
			Schema                   map[string]any   `json:"schema"`
			Subresources             map[string]any   `json:"subresources"`
			AdditionalPrinterColumns []map[string]any `json:"additionalPrinterColumns"`
		} `json:"versions"`
	} `json:"spec"`
}

// TestServedVersionsShareOneSchema holds the condition that makes conversion
// None safe.
//
// Under None the API server converts an object by rewriting its apiVersion,
// then prunes whatever the requested version's schema does not declare. A field
// that v1beta1 has and v1alpha1 lacks is therefore dropped from every object a
// v1alpha1 client reads, and lost for good when that client writes it back.
// Nothing reports it: the write validates, because the field is simply absent.
//
// So every version served beside the storage one carries the same schema, the
// same subresources and the same columns, and is marked deprecated so that a
// client still using it is told to move.
func TestServedVersionsShareOneSchema(t *testing.T) {
	const dir = "../../config/crd/bases"
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated CRDs under %s (%v); run `just manifests`", dir, err)
	}

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var crd crdVersions
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}

		t.Run(crd.Metadata.Name, func(t *testing.T) {
			if c := crd.Spec.Conversion; c != nil && c.Strategy != "None" {
				t.Fatalf("conversion strategy is %s; this test is the safety argument for None and says nothing about a webhook", c.Strategy)
			}

			storage := -1
			for i, v := range crd.Spec.Versions {
				if v.Storage {
					storage = i
				}
			}
			if storage < 0 || crd.Spec.Versions[storage].Name != GroupVersion.Version {
				t.Fatalf("the storage version is not %s; an object stored at a version that is later removed cannot be read",
					GroupVersion.Version)
			}
			want := crd.Spec.Versions[storage]

			for _, v := range crd.Spec.Versions {
				if v.Name == want.Name || !v.Served {
					continue
				}
				if !v.Deprecated {
					t.Errorf("%s is served beside %s and is not marked deprecated", v.Name, want.Name)
				}
				if diff := cmp.Diff(want.Schema, v.Schema); diff != "" {
					t.Errorf("%s and %s have different schemas, which None conversion turns into silent data loss (-%s +%s):\n%s",
						want.Name, v.Name, want.Name, v.Name, diff)
				}
				if diff := cmp.Diff(want.Subresources, v.Subresources); diff != "" {
					t.Errorf("%s and %s have different subresources (-%s +%s):\n%s", want.Name, v.Name, want.Name, v.Name, diff)
				}
				if diff := cmp.Diff(want.AdditionalPrinterColumns, v.AdditionalPrinterColumns); diff != "" {
					t.Errorf("%s and %s print different columns (-%s +%s):\n%s", want.Name, v.Name, want.Name, v.Name, diff)
				}
			}
		})
	}
}

package apitest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	cellcastv1beta1 "github.com/ethan-kane-ops/cellcast/api/v1beta1"
)

// preGraduationFleet is one object of each kind as a manifest written before
// v1beta1 existed would have it: no stickiness on the policy, because that
// field is newer too. Every other optional field is set, so a field that
// either version failed to carry would show.
const preGraduationFleet = `
apiVersion: cellcast.io/v1alpha1
kind: TrustConfig
metadata:
  name: cell-a
spec:
  provider: kubernetes
  credentialSource:
    inCluster: true
  kubernetes:
    serviceAccountName: deployer
    namespace: apps
    audiences: [https://cell-a.example.internal]
---
apiVersion: cellcast.io/v1alpha1
kind: Cluster
metadata:
  name: cell-a
  labels:
    env: prod
spec:
  endpoint: https://cell-a.example.internal:6443
  caBundle: Y2VsbC1h
  provider: generic
  trustConfigRef:
    name: cell-a
  reporter:
    issuer: https://cell-a.example.internal
    subject: system:serviceaccount:cellcast-system:cellcast-agent
  state: DRAINING
---
apiVersion: cellcast.io/v1alpha1
kind: PlacementPolicy
metadata:
  name: deployers
spec:
  subjects:
    - issuer: https://cell-a.example.internal
      subject: system:serviceaccount:apps:deployer
      claims:
        repository: ethan-kane-ops/cellcast
  permittedCells:
    matchLabels:
      env: prod
  strategy: RoundRobin
  tokenTTL:
    default: 15m
    max: 30m
  allowDarkTargeting: true
---
apiVersion: cellcast.io/v1alpha1
kind: WorkloadPlacement
metadata:
  name: wp-payments
spec:
  policy: deployers
  workload: payments
  cell: cell-a
  lastPlacedAt: "2026-09-01T00:00:00Z"
`

// resources maps each kind to the resource the API serves it as.
var resources = map[string]string{
	"Cluster":           "clusters",
	"PlacementPolicy":   "placementpolicies",
	"TrustConfig":       "trustconfigs",
	"WorkloadPlacement": "workloadplacements",
}

func resourceAt(version, kind string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "cellcast.io", Version: version, Resource: resources[kind]}
}

// warnings collects the Warning headers the API server sends back.
type warnings struct {
	mu   sync.Mutex
	seen []string
}

func (w *warnings) HandleWarningHeader(_ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, text)
}

// untyped returns a client that speaks whichever version it is asked to, the
// way kubectl and a hub from an older release do, and records every warning
// it is sent.
func untyped(t *testing.T) (dynamic.Interface, *warnings) {
	t.Helper()
	requireEnv(t)

	w := &warnings{}
	cfg := rest.CopyConfig(restCfg)
	cfg.WarningHandler = w
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("building dynamic client: %v", err)
	}
	return dyn, w
}

// fleetAt returns preGraduationFleet with every object at the given version.
func fleetAt(t *testing.T, version string) []*unstructured.Unstructured {
	t.Helper()

	var out []*unstructured.Unstructured
	for _, doc := range splitDocs(preGraduationFleet) {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
			t.Fatalf("parsing the fleet: %v\n%s", err, doc)
		}
		if obj.GetKind() == "" {
			continue
		}
		obj.SetAPIVersion("cellcast.io/" + version)
		out = append(out, obj)
	}
	if len(out) != len(resources) {
		t.Fatalf("the fleet holds %d objects, want one of each of the %d kinds", len(out), len(resources))
	}
	return out
}

// lost lists the paths under want that got does not hold with the same value.
// A field in got and not in want is allowed: that is a default the API server
// added, which an upgrade may do. A field that went missing or changed is not.
func lost(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{path}
		}
		var out []string
		for k, v := range w {
			out = append(out, lost(path+"."+k, v, g[k])...)
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{path}
		}
		var out []string
		for i := range w {
			out = append(out, lost(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return out
	default:
		if !cmp.Equal(want, got) {
			return []string{path}
		}
		return nil
	}
}

// TestAManifestWrittenAtV1alpha1IsReadAtV1beta1 is an upgrade at the scale of
// one object per kind: applied by an adopter before the graduation, then read
// by a hub that knows only v1beta1.
func TestAManifestWrittenAtV1alpha1IsReadAtV1beta1(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	dyn, warned := untyped(t)
	ctx := context.Background()

	for _, obj := range fleetAt(t, "v1alpha1") {
		kind := obj.GetKind()
		if _, err := dyn.Resource(resourceAt("v1alpha1", kind)).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating %s at v1alpha1: %v", kind, err)
		}
		got, err := dyn.Resource(resourceAt("v1beta1", kind)).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading %s at v1beta1: %v", kind, err)
		}
		if paths := lost("spec", obj.Object["spec"], got.Object["spec"]); len(paths) > 0 {
			t.Errorf("%s written at v1alpha1 lost %v when read at v1beta1", kind, paths)
		}
	}

	// Each create was a request at v1alpha1, and each should have told the
	// client to move.
	var deprecations int
	for _, w := range warned.seen {
		if strings.Contains(w, "cellcast.io/v1alpha1 is deprecated") {
			deprecations++
		}
	}
	if deprecations < len(resources) {
		t.Errorf("%d of %d requests at v1alpha1 drew a deprecation warning; got %q", deprecations, len(resources), warned.seen)
	}

	// The hub's own client decodes it, and the policy carries the stickiness
	// its manifest never mentioned. Here the default lands at write time;
	// `just verify-upgrade` covers an object stored before the field existed,
	// which the API server defaults on read.
	var pol cellcastv1beta1.PlacementPolicy
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "deployers"}, &pol); err != nil {
		t.Fatalf("reading the policy with the hub's client: %v", err)
	}
	if pol.Spec.Stickiness == nil || pol.Spec.Stickiness.Mode != "Preferred" {
		t.Errorf("stickiness = %+v, want mode Preferred by default", pol.Spec.Stickiness)
	}
}

// TestAnObjectWrittenAtV1beta1LosesNothingAtV1alpha1 is the direction None
// conversion can get wrong: a client still on v1alpha1 reading what the hub
// wrote at v1beta1. The API server prunes whatever v1alpha1 does not declare,
// so a field missing from it disappears from that client's view, and from
// the object once that client writes it back.
func TestAnObjectWrittenAtV1beta1LosesNothingAtV1alpha1(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	dyn, _ := untyped(t)
	ctx := context.Background()

	for _, obj := range fleetAt(t, "v1beta1") {
		kind := obj.GetKind()
		// The newest field, set to its non-default value.
		if kind == "PlacementPolicy" {
			if err := unstructured.SetNestedField(obj.Object, "None", "spec", "stickiness", "mode"); err != nil {
				t.Fatalf("setting stickiness: %v", err)
			}
		}
		if _, err := dyn.Resource(resourceAt("v1beta1", kind)).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating %s at v1beta1: %v", kind, err)
		}

		beta, err := dyn.Resource(resourceAt("v1beta1", kind)).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading %s at v1beta1: %v", kind, err)
		}
		alpha, err := dyn.Resource(resourceAt("v1alpha1", kind)).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading %s at v1alpha1: %v", kind, err)
		}
		if diff := cmp.Diff(beta.Object["spec"], alpha.Object["spec"]); diff != "" {
			t.Errorf("%s reads differently at v1alpha1 (-v1beta1 +v1alpha1):\n%s", kind, diff)
		}
	}
}

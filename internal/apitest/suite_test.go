package apitest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub"
)

// crdNames are the manifests every test depends on. Listed rather than
// discovered so that a CRD dropped from config/crd/bases fails the suite
// instead of quietly reducing what is covered.
var crdNames = []string{
	"clusters.cellcast.io",
	"placementpolicies.cellcast.io",
	"trustconfigs.cellcast.io",
}

var (
	restCfg   *rest.Config
	apiScheme *runtime.Scheme

	// unavailable explains why the control plane could not start. Tests skip on
	// it rather than the suite exiting quietly, so a run with no assets still
	// prints one SKIP line per test instead of a bare "ok".
	unavailable string
)

func TestMain(m *testing.M) {
	os.Exit(start(m))
}

func start(m *testing.M) int {
	assets, err := binaryAssets()
	if err != nil {
		unavailable = err.Error()
		return m.Run()
	}

	// Without this, controller-runtime discards every line the manager and the
	// reconcilers emit, so a controller that fails to start would show up only
	// as a test that timed out. Warn level keeps a passing run quiet.
	ctrl.SetLogger(logr.FromSlogHandler(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}).WithAttrs(nil)))

	apiScheme, err = hub.NewScheme()
	if err != nil {
		fmt.Fprintln(os.Stderr, "apitest: building scheme:", err)
		return 1
	}
	// The CRD type is only needed to assert the manifests reach Established.
	if err := apiextensionsv1.AddToScheme(apiScheme); err != nil {
		fmt.Fprintln(os.Stderr, "apitest: registering apiextensions scheme:", err)
		return 1
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
		Scheme:                apiScheme,
	}

	restCfg, err = env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "apitest: starting control plane:", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "apitest: stopping control plane:", err)
		}
	}()

	return m.Run()
}

// binaryAssets locates the etcd and kube-apiserver binaries envtest needs.
//
// KUBEBUILDER_ASSETS is the contract `just envtest` sets and the one CI would
// set. The fallback to bin/envtest exists because `go test ./...` after a bare
// `just envtest-assets` would otherwise skip the whole package for a reason
// that is not visible from the command that was run.
func binaryAssets() (string, error) {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir, nil
	}

	root := filepath.Join("..", "..", "bin", "envtest", "k8s")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", errors.New("KUBEBUILDER_ASSETS is unset and " + root + " does not exist")
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", errors.New("no downloaded assets under " + root)
	}
	// Lexical order is version order for the names setup-envtest writes, and
	// the newest is the one matching the current k8s.io/* modules.
	sort.Strings(dirs)
	return filepath.Join(root, dirs[len(dirs)-1]), nil
}

// requireEnv skips a test when the control plane is not running, naming the
// recipe that fixes it.
func requireEnv(t *testing.T) {
	t.Helper()
	if unavailable != "" {
		t.Skip("no envtest control plane (" + unavailable + "); run `just envtest`")
	}
}

// newClient returns a client bound to the running control plane.
func newClient(t *testing.T) client.Client {
	t.Helper()
	requireEnv(t)

	c, err := client.New(restCfg, client.Options{Scheme: apiScheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

// newNamespace creates a namespace scoped to one test.
//
// Every object these tests create is namespaced, and sharing one namespace
// across the suite makes a leaked object from one test look like a bug in
// another. The namespace is not deleted: envtest has no namespace controller,
// so a delete would hang in Terminating and slow every run down for nothing.
func newNamespace(t *testing.T, c client.Client) string {
	t.Helper()

	name := "t-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-", " ", "-").Replace(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}
	name = strings.Trim(name, "-")

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	return name
}

// eventually retries until fn stops returning an error or the deadline passes,
// then reports the last failure rather than a bare timeout.
func eventually(t *testing.T, timeout time.Duration, what string, fn func() error) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	last := errors.New("never ran")
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: still failing after %s: %v", what, timeout, last)
}

// TestCRDsReachEstablished is the assertion `just verify-crds` used a throwaway
// kind cluster to make. A CRD the API server has accepted but not established
// serves no requests, so every other test here depends on this one.
func TestCRDsReachEstablished(t *testing.T) {
	c := newClient(t)

	for _, name := range crdNames {
		t.Run(name, func(t *testing.T) {
			var crd apiextensionsv1.CustomResourceDefinition
			eventually(t, 30*time.Second, "waiting for "+name, func() error {
				if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &crd); err != nil {
					return err
				}
				if !meta.IsStatusConditionTrue(asConditions(crd.Status.Conditions), string(apiextensionsv1.Established)) {
					return fmt.Errorf("not established: %v", crd.Status.Conditions)
				}
				return nil
			})
		})
	}
}

// asConditions adapts apiextensions conditions to the meta helpers, which only
// understand metav1.Condition.
func asConditions(in []apiextensionsv1.CustomResourceDefinitionCondition) []metav1.Condition {
	out := make([]metav1.Condition, 0, len(in))
	for _, c := range in {
		out = append(out, metav1.Condition{
			Type:   string(c.Type),
			Status: metav1.ConditionStatus(c.Status),
			Reason: c.Reason,
		})
	}
	return out
}

// validCluster is the smallest Cluster the schema accepts. Tests mutate a copy
// to isolate the one field under test.
func validCluster(ns, name string) *cellcastv1alpha1.Cluster {
	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint: "https://cell.example.internal:6443",
			Provider: cellcastv1alpha1.ProviderGeneric,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{
				Name: "shared-trust",
			},
		},
	}
}

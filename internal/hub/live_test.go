package hub

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// TestLiveEndToEnd runs the whole product against a real cluster: a caller
// identity, a policy, a capacity report, a placement, a mint, and a kubeconfig
// that is then used to talk to the cell it names.
//
// Run it with `just verify-e2e`. Every component is the real one except the
// authenticator, which stands in for a CI platform's OIDC issuer; that seam has
// its own end-to-end coverage against a real signing issuer in
// internal/hub/oidc.
func TestLiveEndToEnd(t *testing.T) {
	if os.Getenv("CELLCAST_LIVE") != "1" {
		t.Skip("set CELLCAST_LIVE=1 with a kubeconfig for a throwaway cluster")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	ca := caBundle(t, cfg)
	seedFleet(t, k8s, cfg.Host, ca)

	// Two cells are registered and only one is permitted, so a placement that
	// lands on the right one proves the filter ran rather than that there was
	// nothing else to choose.
	index := capacity.New(capacity.Options{})
	report(t, index, "prod-euw1", 0.20)
	report(t, index, "prod-euw2", 0.80)
	report(t, index, "dev-euw1", 0.01)

	addr := freeAddr(t)
	srv := liveServer(t, cfg, k8s, index, addr)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v, want nil on a clean shutdown", err)
		}
	})
	waitForHub(t, srv)

	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	t.Run("explain shows the filter before the score", func(t *testing.T) {
		out := runCLI(t, addr, "--workload", "checkout-api", "--dry-run", "--explain")
		t.Logf("\n%s", out)

		// dev-euw1 is the least loaded cell in the fleet at 1%. A scoring pass
		// that ran before the permission filter would return it.
		if !strings.Contains(out, "prod-euw1") || strings.Contains(out, "placed checkout-api on dev-euw1") {
			t.Errorf("placement did not choose the permitted, least-loaded cell:\n%s", out)
		}
		if !strings.Contains(out, "permission") {
			t.Errorf("explain does not show the permission stage:\n%s", out)
		}
	})

	t.Run("a real deploy credential is written and works", func(t *testing.T) {
		out := runCLI(t, addr, "--workload", "checkout-api", "--kubeconfig", kubeconfig)
		t.Logf("\n%s", out)

		info, err := os.Stat(kubeconfig)
		if err != nil {
			t.Fatalf("stat kubeconfig: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("kubeconfig mode = %O, want 600", got)
		}

		// The credential has to actually work. `kubectl auth whoami` names the
		// identity the API server resolved it to.
		who := kubectl(t, kubeconfig, "auth", "whoami")
		if !strings.Contains(who, "system:serviceaccount:apps:deployer") {
			t.Errorf("kubeconfig authenticates as:\n%s\nwant apps/deployer", who)
		}

		// And it has to be bounded. The deployer role covers pods in apps and
		// nothing else.
		if allowed := kubectl(t, kubeconfig, "auth", "can-i", "list", "pods", "-n", "apps"); !strings.Contains(allowed, "yes") {
			t.Errorf("cannot list pods in apps: %s", allowed)
		}
		if allowed := kubectl(t, kubeconfig, "auth", "can-i", "list", "secrets", "-n", "kube-system"); strings.Contains(allowed, "yes") {
			t.Error("the minted credential can read secrets in kube-system; it is not scoped")
		}
	})

	t.Run("draining the chosen cell moves the next placement", func(t *testing.T) {
		setState(t.Context(), t, k8s, "prod-euw1", cellcastv1alpha1.ClusterStateDraining)
		t.Cleanup(func() {
			setState(context.Background(), t, k8s, "prod-euw1", cellcastv1alpha1.ClusterStateLive)
		})

		out := runCLI(t, addr, "--workload", "checkout-api", "--dry-run", "--explain")
		t.Logf("\n%s", out)

		if !strings.Contains(out, "prod-euw2") {
			t.Errorf("placement did not move to the remaining live cell:\n%s", out)
		}
		if !strings.Contains(out, "DRAINING") {
			t.Errorf("explain does not say why prod-euw1 was refused:\n%s", out)
		}
	})
}

// liveServer wires every real component behind a stub authenticator.
func liveServer(t *testing.T, cfg *rest.Config, k8s client.Client, index *capacity.Registry, addr string) *Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	connector := broker.NewSecretConnector(k8s, testNamespace, cfg)
	minter := broker.New(k8s, testNamespace, time.Hour, log,
		broker.NewKubernetesProvider(connector.Connect))
	engine := placement.NewEngine(k8s, index, testNamespace, log)

	hubCfg := DefaultConfig()
	hubCfg.Addr = addr
	hubCfg.ProbeAddr = freeAddr(t)
	hubCfg.Namespace = testNamespace

	srv, err := NewServer(hubCfg, log,
		WithClusterClient(k8s),
		WithCapacityRegistry(index),
		WithPlacer(engine),
		WithMinter(minter),
		WithAuthenticator(pipelineIdentity{}),
	)
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}
	return srv
}

// pipelineIdentity stands in for a CI platform's OIDC issuer.
type pipelineIdentity struct{}

func (pipelineIdentity) Authenticate(context.Context, *http.Request) (*identity.Identity, error) {
	return &identity.Identity{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: "repo:acme/checkout:ref:refs/heads/main",
		Claims:  map[string]string{"repository": "acme/checkout"},
	}, nil
}

// runCLI executes the built cellcast binary against the hub.
func runCLI(t *testing.T, addr string, args ...string) string {
	t.Helper()

	bin, err := filepath.Abs("../../bin/cellcast")
	if err != nil {
		t.Fatalf("resolving the client binary: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s not built; run `just build` first", bin)
	}

	cmd := exec.CommandContext(t.Context(), bin, append([]string{"place", "--hub", "http://" + addr}, args...)...)
	cmd.Env = append(os.Environ(), "CELLCAST_TOKEN=stand-in-for-a-real-oidc-token")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cellcast place: %v\n%s", err, out)
	}
	return string(out)
}

func kubectl(t *testing.T, kubeconfig string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	// can-i exits non-zero on "no", which is an answer rather than a failure.
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func seedFleet(t *testing.T, k8s client.Client, endpoint string, ca []byte) {
	t.Helper()

	trust := &cellcastv1alpha1.TrustConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-trust", Namespace: testNamespace},
		Spec: cellcastv1alpha1.TrustConfigSpec{
			Provider:         cellcastv1alpha1.TrustProviderKubernetes,
			CredentialSource: cellcastv1alpha1.CredentialSource{InCluster: true},
			Kubernetes: &cellcastv1alpha1.KubernetesTrust{
				ServiceAccountName: "deployer",
				Namespace:          "apps",
			},
		},
	}

	policy := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-prod", Namespace: testNamespace},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{{
				Issuer: "https://token.actions.githubusercontent.com",
				Claims: map[string]string{"repository": "acme/checkout"},
			}},
			PermittedCells: metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			Strategy:       cellcastv1alpha1.ScoringLeastLoaded,
		},
	}

	objs := []client.Object{trust, policy}
	for _, spec := range []struct{ name, env string }{
		{"prod-euw1", "prod"}, {"prod-euw2", "prod"}, {"dev-euw1", "dev"},
	} {
		objs = append(objs, &cellcastv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: spec.name, Namespace: testNamespace,
				Labels: map[string]string{"env": spec.env},
			},
			Spec: cellcastv1alpha1.ClusterSpec{
				Endpoint:       endpoint,
				CABundle:       ca,
				Provider:       cellcastv1alpha1.ProviderGeneric,
				TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "cell-trust"},
				State:          cellcastv1alpha1.ClusterStateLive,
			},
		})
	}

	for _, obj := range objs {
		if err := k8s.Create(t.Context(), obj); err != nil {
			t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
		}
		t.Cleanup(func() {
			_ = k8s.Delete(context.Background(), obj)
		})
	}
}

// setState takes a context rather than using t.Context, because it also runs
// from t.Cleanup, by which point the test's own context is already cancelled.
func setState(ctx context.Context, t *testing.T, k8s client.Client, name string, state cellcastv1alpha1.ClusterState) {
	t.Helper()

	var cl cellcastv1alpha1.Cluster
	key := client.ObjectKey{Namespace: testNamespace, Name: name}
	if err := k8s.Get(ctx, key, &cl); err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	patch := client.MergeFrom(cl.DeepCopy())
	cl.Spec.State = state
	if err := k8s.Patch(ctx, &cl, patch); err != nil {
		t.Fatalf("patching %s to %s: %v", name, state, err)
	}
}

func report(t *testing.T, index *capacity.Registry, cell string, utilisation float64) {
	t.Helper()
	const allocatable = 100_000
	err := index.Report(capacity.Report{
		Cell:                   cell,
		Nodes:                  3,
		CPUMilliAllocatable:    allocatable,
		CPUMilliCommitted:      int64(allocatable * utilisation),
		MemoryBytesAllocatable: allocatable,
		MemoryBytesCommitted:   int64(allocatable * utilisation),
		Pods:                   10,
		PodCapacity:            110,
	})
	if err != nil {
		t.Fatalf("seeding capacity for %s: %v", cell, err)
	}
}

func caBundle(t *testing.T, cfg *rest.Config) []byte {
	t.Helper()
	if len(cfg.CAData) > 0 {
		return cfg.CAData
	}
	if cfg.CAFile == "" {
		t.Fatal("kubeconfig has no certificate authority")
	}
	raw, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		t.Fatalf("reading %s: %v", cfg.CAFile, err)
	}
	return raw
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

func waitForHub(t *testing.T, srv *Server) {
	t.Helper()
	srv.SetReady(true)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/readyz", srv.cfg.ProbeAddr))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hub did not become ready")
}

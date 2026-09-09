package agent

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// noisy adds the parts of a cached object the agent does not read: the
// managed-fields history, the annotations every controller that has touched the
// object leaves behind, and the per-container status blocks. On a large cell
// these are most of what the agent would otherwise hold in memory.
func noisy(obj metav1.Object) {
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply},
		{Manager: "deployment-controller", Operation: metav1.ManagedFieldsOperationUpdate},
	})
	obj.SetAnnotations(map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
		"deployment.kubernetes.io/revision":                "7",
	})
}

func noisyNode(name, cpu, mem string, pods int64) *corev1.Node {
	n := node(name, cpu, mem, pods)
	noisy(n)
	n.Status.Images = []corev1.ContainerImage{{Names: []string{"registry.example.test/app:v1"}, SizeBytes: 1 << 30}}
	return n
}

func noisyPod(name, cpu, mem string) *corev1.Pod {
	p := pod(name, cpu, mem)
	noisy(p)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 3}}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
	return p
}

// TestTrimKeepsEveryFieldTheSnapshotIsBuiltFrom is the contract between the two
// halves of the agent that nothing else states.
//
// trim runs on every object entering the informer cache and Collect reads that
// cache. A field trimmed today that Collect starts reading tomorrow does not
// fail: it produces a smaller number, and the hub places deploys on it. So the
// test is that trimming changes no number, not that trim strips a given field.
func TestTrimKeepsEveryFieldTheSnapshotIsBuiltFrom(t *testing.T) {
	nodes := []*corev1.Node{noisyNode("node-a", "4", "16Gi", 110), noisyNode("node-b", "8", "32Gi", 110)}
	pods := []*corev1.Pod{noisyPod("api", "500m", "1Gi"), noisyPod("worker", "250m", "512Mi")}

	before := Collect(nodes, pods)
	if before.Nodes == 0 || before.Pods == 0 {
		t.Fatalf("the fixture reports %+v, so this test would pass on an empty snapshot", before)
	}

	for _, n := range nodes {
		if _, err := trim(n); err != nil {
			t.Fatalf("trimming a node: %v", err)
		}
	}
	for _, p := range pods {
		if _, err := trim(p); err != nil {
			t.Fatalf("trimming a pod: %v", err)
		}
	}

	if after := Collect(nodes, pods); after != before {
		t.Errorf("trimming changed the measurement:\n before %+v\n  after %+v", before, after)
	}
}

func TestTrimDropsTheHistoryTheCacheDoesNotRead(t *testing.T) {
	// The other half. Without this, a trim that stopped stripping anything
	// would pass the test above, and the agent's footprint would quietly become
	// a function of how many controllers have touched each object rather than
	// of how many pods the cell runs.
	n := noisyNode("node-a", "4", "16Gi", 110)
	if _, err := trim(n); err != nil {
		t.Fatalf("trimming a node: %v", err)
	}
	if n.ManagedFields != nil || n.Annotations != nil || n.Status.Images != nil {
		t.Errorf("a trimmed node still carries managed fields, annotations or its image list: %+v", n.ObjectMeta)
	}

	p := noisyPod("api", "500m", "1Gi")
	if _, err := trim(p); err != nil {
		t.Fatalf("trimming a pod: %v", err)
	}
	if p.ManagedFields != nil || p.Annotations != nil {
		t.Errorf("a trimmed pod still carries managed fields or annotations: %+v", p.ObjectMeta)
	}
	if p.Status.ContainerStatuses != nil || p.Status.InitContainerStatuses != nil {
		t.Error("a trimmed pod still carries its container status blocks")
	}
}

// TestTrimPassesThroughWhatItDoesNotKnow covers the transform being applied to
// every type in the cache, not only the two it strips. Returning nil for an
// unrecognised object would empty the cache it was meant to shrink.
func TestTrimPassesThroughWhatItDoesNotKnow(t *testing.T) {
	in := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}}
	out, err := trim(in)
	if err != nil {
		t.Fatalf("trim = %v, want nil", err)
	}
	if out != any(in) {
		t.Errorf("trim returned %v, want the object it was given", out)
	}
}

func TestIdentityNamesThisReplica(t *testing.T) {
	// The pod name is what an operator sees in `kubectl get lease`, so it is
	// worth preferring over anything unique but anonymous.
	t.Setenv("POD_NAME", "cellcast-agent-7d9f4b")
	if got := identity(); got != "cellcast-agent-7d9f4b" {
		t.Errorf("identity() = %q, want the pod name", got)
	}

	t.Setenv("POD_NAME", "")
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname to fall back to: %v", err)
	}
	if got := identity(); got != host {
		t.Errorf("identity() = %q, want the hostname %q", got, host)
	}
}

// TestProbesTrackTheCachesAndNotTheLease pins the readiness semantics a
// two-replica Deployment depends on.
//
// A standby replica holds no lease and reports nothing, and is working exactly
// as intended. If readiness tracked leadership instead, one pod of every pair
// would show unhealthy forever and the next person to look would go hunting for
// a bug that is not there.
func TestProbesTrackTheCachesAndNotTheLease(t *testing.T) {
	var ready atomic.Bool
	srv := probeServer("127.0.0.1:0", &ready, discardLogger())
	t.Cleanup(func() { shutdownProbes(srv, discardLogger()) })

	get := func(path string) int {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}

	// Before the caches sync there is nothing to report, and a replica that
	// took traffic here would publish an empty cell.
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz before the caches synced = %d, want %d", got, http.StatusServiceUnavailable)
	}
	// Liveness is not readiness. A process that is starting up is not a process
	// to restart, and conflating the two turns a slow initial list into a
	// crash loop.
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("GET /healthz before the caches synced = %d, want %d", got, http.StatusOK)
	}

	ready.Store(true)
	if got := get("/readyz"); got != http.StatusOK {
		t.Errorf("GET /readyz after the caches synced = %d, want %d", got, http.StatusOK)
	}
}

const explicitKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: cell
    cluster:
      server: https://cell.example.test:6443
contexts:
  - name: cell
    context:
      cluster: cell
      user: cell
current-context: cell
users:
  - name: cell
    user: {}
`

// TestKubeConfigLoadsTheExplicitPath covers the shape that makes verify-agent
// possible: one binary pointed at each of three clusters in turn, from a
// laptop, without building an image for every change.
func TestKubeConfigLoadsTheExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cell.kubeconfig")
	if err := os.WriteFile(path, []byte(explicitKubeconfig), 0o600); err != nil {
		t.Fatalf("writing the kubeconfig: %v", err)
	}

	cfg, err := kubeConfig(path)
	if err != nil {
		t.Fatalf("kubeConfig = %v, want nil", err)
	}
	if cfg.Host != "https://cell.example.test:6443" {
		t.Errorf("kubeConfig resolved %q, want the server the file names", cfg.Host)
	}
}

func TestKubeConfigRefusesAPathThatIsNotThere(t *testing.T) {
	// A silent fall back to the ambient kubeconfig would point the agent at
	// whichever cluster the operator's context happens to name, and it would
	// report that cluster's capacity under this cell's name.
	if _, err := kubeConfig(filepath.Join(t.TempDir(), "absent.kubeconfig")); err == nil {
		t.Error("kubeConfig accepted a path with no file at it")
	}
}

// TestLogLevelSelectsWhatIsWritten covers the mapping behind --log-level.
//
// An unrecognised level resolving to info rather than to nothing is the
// property worth pinning: a typo in a flag that silenced the agent would be
// indistinguishable from an agent that had stopped reporting.
func TestLogLevelSelectsWhatIsWritten(t *testing.T) {
	tests := []struct {
		level   string
		enabled slog.Level
		silent  slog.Level
	}{
		{level: "debug", enabled: slog.LevelDebug, silent: slog.LevelDebug - 1},
		{level: "info", enabled: slog.LevelInfo, silent: slog.LevelDebug},
		{level: "warn", enabled: slog.LevelWarn, silent: slog.LevelInfo},
		{level: "error", enabled: slog.LevelError, silent: slog.LevelWarn},
		{level: "nonsense", enabled: slog.LevelInfo, silent: slog.LevelDebug},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			log := NewLogger(Config{LogLevel: tt.level, LogFormat: "json"})
			if !log.Enabled(t.Context(), tt.enabled) {
				t.Errorf("--log-level %s drops %s", tt.level, tt.enabled)
			}
			if log.Enabled(t.Context(), tt.silent) {
				t.Errorf("--log-level %s writes %s", tt.level, tt.silent)
			}
		})
	}
}

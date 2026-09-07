package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Lease timings for the reporting lock.
//
// A lost lease costs at most one heartbeat interval of silence, which the hub's
// staleness window absorbs, so these are the ordinary client-go defaults rather
// than anything tuned.
const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second

	// leaseName is fixed rather than per-replica: the point of the lease is
	// that every replica of this Deployment contends for the same one.
	leaseName = "cellcast-agent"
)

// Run starts the agent and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	hub, err := NewHubClient(cfg)
	if err != nil {
		return err
	}

	restCfg, err := kubeConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}

	var ready atomic.Bool
	probes := probeServer(cfg.ProbeAddr, &ready, log)
	defer shutdownProbes(probes, log)

	collect, err := startInformers(ctx, clientset, log)
	if err != nil {
		return err
	}
	ready.Store(true)

	reporter := &Reporter{
		Collect:    collect,
		Publish:    hub.Report,
		Interval:   cfg.HeartbeatInterval,
		Timeout:    cfg.RequestTimeout,
		MaxBackoff: cfg.MaxBackoff,
		Log:        log,
	}

	if !cfg.LeaderElection {
		log.Warn("leader election is disabled; run a single replica or the hub will see duplicate reports")
		return reporter.Run(ctx)
	}
	return runElected(ctx, cfg, clientset, log, reporter.Run)
}

// kubeConfig resolves how to reach this cell's API server.
//
// In-cluster first, because that is how the agent actually runs. The ambient
// kubeconfig is the fallback so the same binary can be pointed at a kind
// cluster from a laptop, which is what makes the multi-cluster verification in
// `just verify-agent` possible without building an image for every change.
func kubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		} else if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("loading in-cluster config: %w", err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubernetes client config: %w", err)
	}
	return cfg, nil
}

// startInformers begins watching nodes and pods and returns a collector reading
// from the synced caches.
//
// Informers rather than a LIST on every heartbeat: a full pod list every 30
// seconds from every cell in the estate is real API server load, and the whole
// reason this agent exists is to be cheaper than asking the cluster directly.
func startInformers(ctx context.Context, client kubernetes.Interface, log *slog.Logger) (func() Snapshot, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithTransform(trim))

	nodes := factory.Core().V1().Nodes().Lister()
	pods := factory.Core().V1().Pods().Lister()

	factory.Start(ctx.Done())
	for informer, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			return nil, fmt.Errorf("cache for %s did not sync", informer)
		}
	}
	log.Info("node and pod caches synced")

	return func() Snapshot {
		nodeList, err := nodes.List(labels.Everything())
		if err != nil {
			// A lister backed by a synced cache does not fail in practice.
			// Returning an empty snapshot rather than a partial one keeps the
			// hub's validation the thing that rejects it, so the failure is
			// visible instead of being scored as an idle cell.
			log.Error("listing nodes from cache failed", slog.Any("error", err))
			return Snapshot{}
		}
		podList, err := pods.List(labels.Everything())
		if err != nil {
			log.Error("listing pods from cache failed", slog.Any("error", err))
			return Snapshot{}
		}
		return Collect(nodeList, podList)
	}, nil
}

// trim drops the parts of a cached object nothing here reads.
//
// The agent holds every pod in the cluster in memory, and on a large cell the
// managed-fields history and annotations are most of that. Stripping them at
// the point objects enter the cache is what keeps the agent's footprint a
// function of pod count rather than of how many controllers have touched them.
func trim(obj any) (any, error) {
	switch o := obj.(type) {
	case *corev1.Pod:
		o.ManagedFields = nil
		o.Annotations = nil
		o.Status.ContainerStatuses = nil
		o.Status.InitContainerStatuses = nil
	case *corev1.Node:
		o.ManagedFields = nil
		o.Annotations = nil
		o.Status.Images = nil
	}
	return obj, nil
}

// runElected runs fn only while this replica holds the lease.
//
// Capacity is a cluster-level fact, so a second replica reporting it publishes
// the same number twice. The Deployment runs more than one for availability;
// the lease is what stops that costing the hub duplicate writes.
func runElected(ctx context.Context, cfg Config, client kubernetes.Interface, log *slog.Logger,
	fn func(context.Context) error) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: leaseName, Namespace: cfg.Namespace},
		Client:    client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity(),
		},
	}

	for ctx.Err() == nil {
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: leaseDuration,
			RenewDeadline: renewDeadline,
			RetryPeriod:   retryPeriod,
			// Hand the lease back on shutdown instead of making the next
			// replica wait for it to expire. Without this a rolling update
			// costs a full lease duration of missed heartbeats per cell.
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					log.Info("acquired the reporting lease", slog.String("lease", leaseName))
					if err := fn(ctx); err != nil {
						log.Error("reporting loop stopped", slog.Any("error", err))
					}
				},
				OnStoppedLeading: func() {
					log.Warn("lost the reporting lease, standing by")
				},
			},
		})
		if err != nil {
			return fmt.Errorf("building leader elector: %w", err)
		}
		// Returns when leadership is lost or ctx is cancelled. Re-contending
		// rather than exiting: another replica taking over is normal, and a
		// process that died on it would crash-loop through every handover.
		elector.Run(ctx)
	}
	return nil
}

// identity names this replica in the lease.
//
// The pod name, which is unique per replica and is what an operator sees in
// `kubectl get lease`. A random string would work for correctness and tell
// nobody which pod is reporting.
func identity() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return "cellcast-agent"
}

// probeServer starts the health and readiness listener.
//
// Readiness tracks the informer caches, not leadership. A standby replica is
// working exactly as intended and must report ready, or a two-replica
// Deployment permanently shows one pod unhealthy and the next person to look
// at it goes hunting for a bug that is not there.
func probeServer(addr string, ready *atomic.Bool, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("listening", slog.String("server", "probe"), slog.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("probe server stopped", slog.Any("error", err))
		}
	}()
	return srv
}

func shutdownProbes(srv *http.Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("probe server shutdown failed", slog.Any("error", err))
	}
}

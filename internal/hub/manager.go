package hub

import (
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// NewScheme returns a scheme with the Kubernetes and cellcast types registered.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("registering client-go scheme: %w", err)
	}
	if err := cellcastv1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("registering cellcast scheme: %w", err)
	}
	return s, nil
}

// ManagerOptions configures the controller-runtime manager.
type ManagerOptions struct {
	// Namespace scopes the informer cache. The hub only ever reads its own
	// resources, so watching the whole cluster would ask for RBAC it does not
	// need and cache objects it never reads.
	Namespace string
	// MetricsAddr is the listen address for the controller metrics endpoint.
	// "0" disables it.
	//
	// Empty is not the same as disabled: controller-runtime then falls back to
	// its own default of :8080, which is the hub's API port. ENG-178 owns what
	// this endpoint should expose.
	MetricsAddr string
	// LeaderElection enables leader election for the reconciler path.
	//
	// The API path is stateless and every replica can answer any request, so
	// only the controllers elect. See docs/architecture.md ADR-006 and ENG-179.
	LeaderElection bool
	// LeaderElectionNamespace is where the lease lives.
	LeaderElectionNamespace string
}

// NewManager builds the controller-runtime manager that owns the Cluster and
// PlacementPolicy reconcilers.
//
// Reconcilers are added by RegisterControllers. ENG-112 extends the Cluster
// reconciler and ENG-173 adds the PlacementPolicy one.
//
// log is bridged into controller-runtime's global logger. That call is not
// optional: without it every line the controller machinery emits is discarded,
// so a reconcile that fails repeatedly, a lost leader election, or a cache that
// cannot start would all be invisible. It also keeps the project to one log
// sink rather than two.
func NewManager(opts ManagerOptions, log *slog.Logger) (manager.Manager, error) {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}

	// GetConfig rather than GetConfigOrDie: a hub with no cluster access cannot
	// serve the registry, and that deserves an error the operator can read
	// rather than an exit inside a library call.
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubernetes client config: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: opts.MetricsAddr},
		Cache:                   cache.Options{DefaultNamespaces: map[string]cache.Config{opts.Namespace: {}}},
		LeaderElection:          opts.LeaderElection,
		LeaderElectionID:        "cellcast-hub.cellcast.io",
		LeaderElectionNamespace: opts.LeaderElectionNamespace,
	})
	if err != nil {
		return nil, fmt.Errorf("building manager: %w", err)
	}
	return mgr, nil
}

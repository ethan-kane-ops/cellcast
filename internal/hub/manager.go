package hub

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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
	// MetricsAddr is the listen address for the controller metrics endpoint.
	// Empty disables it.
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
// Reconcilers are registered by the tickets that own them: ENG-110 and ENG-112
// for Cluster, ENG-173 for PlacementPolicy.
func NewManager(opts ManagerOptions) (manager.Manager, error) {
	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: opts.MetricsAddr},
		LeaderElection:          opts.LeaderElection,
		LeaderElectionID:        "cellcast-hub.cellcast.io",
		LeaderElectionNamespace: opts.LeaderElectionNamespace,
	})
	if err != nil {
		return nil, fmt.Errorf("building manager: %w", err)
	}
	return mgr, nil
}

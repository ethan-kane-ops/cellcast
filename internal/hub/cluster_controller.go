package hub

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// ClusterReconciler publishes the state the hub is acting on into a Cluster's
// status.
//
// It is deliberately thin. The placement state machine proper, including what
// DRAINING does to in-flight work and how a cell is promoted out of DARK, is
// ENG-112. What this gives the operator today is the answer to "has the hub
// seen my edit yet", which `kubectl get cluster` can show because status
// carries the generation the observed state was derived from.
//
// Nothing here writes on a heartbeat. Status is touched only when spec changes,
// which is what keeps the etcd write rate proportional to operator edits rather
// than to fleet size (docs/architecture.md ADR-002).
type ClusterReconciler struct {
	Client client.Client
}

// Reconcile reflects spec.state into status.observedState.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cl cellcastv1alpha1.Cluster
	if err := r.Client.Get(ctx, req.NamespacedName, &cl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The CRD defaults state to LIVE, but an object created by a client that
	// bypassed defaulting still has to resolve to something.
	state := cl.Spec.State
	if state == "" {
		state = cellcastv1alpha1.ClusterStateLive
	}

	if cl.Status.ObservedState == state && cl.Status.ObservedGeneration == cl.Generation {
		return ctrl.Result{}, nil
	}

	cl.Status.ObservedState = state
	cl.Status.ObservedGeneration = cl.Generation
	if err := r.Client.Status().Update(ctx, &cl); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating cluster status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with mgr.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cellcastv1alpha1.Cluster{}).
		Named("cluster").
		Complete(r)
}

// RegisterControllers wires the hub's reconcilers into mgr.
//
// Reconcilers are added here rather than in main so that the set of controllers
// the hub runs is a property of the package that owns them.
func RegisterControllers(mgr manager.Manager) error {
	if err := (&ClusterReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering cluster controller: %w", err)
	}
	return nil
}

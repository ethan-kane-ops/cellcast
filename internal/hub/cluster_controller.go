package hub

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// ClusterReconciler publishes the state the hub is acting on into a Cluster's
// status.
//
// The state itself is operator input on spec, so this controller never decides
// what state a cell is in. What it does is make the hub's view of that state
// observable: which state was seen, at which generation, since when, and what
// that means for placement. Without it, "I patched the cell to DRAINING an hour
// ago and deploys are still landing there" has no answer short of reading hub
// logs.
//
// Nothing here writes on a heartbeat. Status is touched only when the state or
// generation actually moves, which is what keeps the etcd write rate
// proportional to operator edits rather than to fleet size
// (docs/architecture.md ADR-002).
type ClusterReconciler struct {
	Client   client.Client
	Recorder recorder.EventRecorder

	// Now supplies the transition timestamp. Overridden only in tests, where
	// two transitions land inside metav1.Time's one-second resolution and the
	// assertion would otherwise be untestable rather than merely imprecise.
	Now func() metav1.Time
}

func (r *ClusterReconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

// Reconcile reflects spec.state into status.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cl cellcastv1alpha1.Cluster
	if err := r.Client.Get(ctx, req.NamespacedName, &cl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	state := cl.Spec.State.Normalize()
	previous := cl.Status.ObservedState

	before := cl.Status.DeepCopy()
	applyClusterStatus(&cl, state, r.now())
	if equality(before, &cl.Status) {
		return ctrl.Result{}, nil
	}

	if err := r.Client.Status().Update(ctx, &cl); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating cluster status: %w", err)
	}

	// The event is emitted after the write succeeds, so `kubectl describe`
	// never shows a transition that was rolled back by a conflict.
	if previous != state && r.Recorder != nil {
		r.Recorder.Eventf(&cl, nil, corev1.EventTypeNormal, "StateChanged", "Reconcile",
			"placement state %s to %s", orNone(previous), state)
	}
	return ctrl.Result{}, nil
}

func orNone(s cellcastv1alpha1.ClusterState) string {
	if s == "" {
		return "<none>"
	}
	return string(s)
}

// applyClusterStatus computes the status for a cell in the given state.
//
// Split out from Reconcile so the mapping from state to condition is testable
// without a client, and so the comparison that decides whether to write is
// against a value rather than against a sequence of assignments.
func applyClusterStatus(cl *cellcastv1alpha1.Cluster, state cellcastv1alpha1.ClusterState, now metav1.Time) {
	if cl.Status.ObservedState != state {
		cl.Status.StateSince = &now
	}
	cl.Status.ObservedState = state
	cl.Status.ObservedGeneration = cl.Generation

	status, reason, message := acceptingCondition(state)
	meta.SetStatusCondition(&cl.Status.Conditions, metav1.Condition{
		Type:               cellcastv1alpha1.ClusterConditionAccepting,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cl.Generation,
	})
}

// acceptingCondition renders one state as a condition.
//
// DARK reports False rather than Unknown. The condition answers the default
// question, "will an ordinary placement land here", and for a dark cell the
// answer is no; the message carries the exception. Unknown would suggest the
// hub cannot tell, which is a different and more alarming situation.
func acceptingCondition(state cellcastv1alpha1.ClusterState) (metav1.ConditionStatus, string, string) {
	switch state {
	case cellcastv1alpha1.ClusterStateLive:
		return metav1.ConditionTrue, cellcastv1alpha1.ClusterReasonLive,
			"accepting new placements"
	case cellcastv1alpha1.ClusterStateDark:
		return metav1.ConditionFalse, cellcastv1alpha1.ClusterReasonDark,
			"accepting only placements that explicitly target a dark cell"
	case cellcastv1alpha1.ClusterStateDraining:
		return metav1.ConditionFalse, cellcastv1alpha1.ClusterReasonDraining,
			"accepting no new placements; workloads already here continue to serve"
	default:
		return metav1.ConditionUnknown, cellcastv1alpha1.ClusterReasonUnknown,
			"state is not recognised by this hub version"
	}
}

// equality reports whether two statuses are the same for the purpose of
// deciding whether to issue a write.
//
// Condition timestamps are excluded: meta.SetStatusCondition preserves
// LastTransitionTime when nothing changed, so comparing the rest is enough, and
// comparing timestamps would make every reconcile a write.
func equality(a, b *cellcastv1alpha1.ClusterStatus) bool {
	if a.ObservedState != b.ObservedState || a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason ||
			x.Message != y.Message || x.ObservedGeneration != y.ObservedGeneration {
			return false
		}
	}
	return true
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
	r := &ClusterReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder("cellcast-hub"),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering cluster controller: %w", err)
	}
	return nil
}

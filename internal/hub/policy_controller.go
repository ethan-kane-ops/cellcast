package hub

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// PlacementPolicy status condition types and reasons.
const (
	// PolicyConditionReady reports whether the policy currently selects any
	// registered cell.
	PolicyConditionReady = "Ready"

	PolicyReasonCellsMatched   = "CellsMatched"
	PolicyReasonNoCellsMatched = "NoCellsMatched"
	PolicyReasonInvalid        = "InvalidSelector"
)

// PlacementPolicyReconciler reports how many cells each policy permits.
//
// A selector that matches nothing is a deny-everything policy that looks
// correct in Git, and the only symptom is deploys failing with "policy permits
// no registered cell" some time later. Publishing the match count turns a
// mislabelled selector into something `kubectl get placementpolicy` shows
// before anyone deploys against it.
//
// It is reporting, not enforcement. Placement re-evaluates the selector at
// decision time and never reads this status, so a stale condition cannot widen
// what a caller may reach.
type PlacementPolicyReconciler struct {
	Client client.Client
}

// Reconcile publishes the policy's current match count.
func (r *PlacementPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy cellcastv1alpha1.PlacementPolicy
	if err := r.Client.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status, reason, message := metav1.ConditionFalse, PolicyReasonNoCellsMatched, ""

	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PermittedCells)
	if err != nil {
		status, reason = metav1.ConditionFalse, PolicyReasonInvalid
		message = fmt.Sprintf("permittedCells is not a valid selector: %v", err)
	} else {
		var clusters cellcastv1alpha1.ClusterList
		if err := r.Client.List(ctx, &clusters,
			client.InNamespace(req.Namespace),
			client.MatchingLabelsSelector{Selector: selector},
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing clusters for policy %s: %w", policy.Name, err)
		}

		switch n := len(clusters.Items); n {
		case 0:
			message = fmt.Sprintf("selector %q matches no registered cell", selector)
		default:
			status, reason = metav1.ConditionTrue, PolicyReasonCellsMatched
			message = fmt.Sprintf("selector %q matches %d registered cell(s)", selector, n)
		}
	}

	before := policy.Status.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               PolicyConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})

	if policyStatusEqual(before, &policy.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Client.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating placement policy status: %w", err)
	}
	return ctrl.Result{}, nil
}

func policyStatusEqual(a, b *cellcastv1alpha1.PlacementPolicyStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration || len(a.Conditions) != len(b.Conditions) {
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
//
// It watches Clusters as well as policies: registering, relabelling or deleting
// a cell changes what every policy matches, and a count that only updates when
// the policy itself is edited would be wrong most of the time.
func (r *PlacementPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cellcastv1alpha1.PlacementPolicy{}).
		Watches(&cellcastv1alpha1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.policiesInNamespace)).
		Named("placementpolicy").
		Complete(r)
}

// policiesInNamespace enqueues every policy in the changed cell's namespace.
//
// Enqueuing all of them rather than working out which selectors the change
// affects: the fleet is tens of cells and a handful of policies, and the
// cleverer version is a cache invalidation problem with a silent wrong answer
// as its failure mode.
func (r *PlacementPolicyReconciler) policiesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var policies cellcastv1alpha1.PlacementPolicyList
	if err := r.Client.List(ctx, &policies, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	out := make([]reconcile.Request, 0, len(policies.Items))
	for i := range policies.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: policies.Items[i].Namespace,
			Name:      policies.Items[i].Name,
		}})
	}
	return out
}

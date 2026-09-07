package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
)

// recheckInterval is how often a trust configuration that is not ready is
// re-examined.
//
// Only unready configurations requeue. A working one is re-checked when it
// changes and not otherwise, so the steady state costs nothing, while an
// operator who creates the missing Secret sees the condition clear without
// having to touch the TrustConfig to prompt it.
const recheckInterval = 2 * time.Minute

// TrustConfigReconciler reports whether a trust configuration could mint.
//
// The alternative is finding out during a deploy. A cell whose trust config
// names a service account that does not exist, or a Secret nobody created,
// registers cleanly, passes placement, and fails at the last step of the
// pipeline that needed it. Publishing readiness moves that discovery to
// `kubectl get trustconfig`, at the moment the operator configures it.
//
// It checks configuration, not permission. Whether the referenced service
// account can actually do anything useful in the cell is that cluster's RBAC
// to answer, and asking would mean the hub holding permission to introspect it.
type TrustConfigReconciler struct {
	Client client.Client

	// Secrets reads credential material and must be uncached. The controller
	// only ever checks for a key's presence, and it deliberately does not hold
	// the value it read (docs/threat-model.md T-08).
	Secrets client.Reader

	// Namespace is where credential Secrets live.
	Namespace string
}

// Reconcile publishes whether this configuration resolves.
func (r *TrustConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var trust cellcastv1alpha1.TrustConfig
	if err := r.Client.Get(ctx, req.NamespacedName, &trust); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reason, message := r.evaluate(ctx, &trust)
	status := metav1.ConditionFalse
	if reason == cellcastv1alpha1.TrustReasonValid {
		status = metav1.ConditionTrue
	}

	before := trust.Status.DeepCopy()
	trust.Status.ObservedGeneration = trust.Generation
	meta.SetStatusCondition(&trust.Status.Conditions, metav1.Condition{
		Type:               cellcastv1alpha1.TrustConfigConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: trust.Generation,
	})

	if !trustStatusEqual(before, &trust.Status) {
		if err := r.Client.Status().Update(ctx, &trust); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating trust config status: %w", err)
		}
	}

	if status == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: recheckInterval}, nil
}

// evaluate returns the condition reason and message for a trust configuration.
func (r *TrustConfigReconciler) evaluate(ctx context.Context, trust *cellcastv1alpha1.TrustConfig) (string, string) {
	if err := broker.ValidateTrust(trust); err != nil {
		// A named-but-unbuilt provider is called out separately from a broken
		// one. "aws is not implemented in this build" tells an operator to wait
		// for v0.2; "invalid" would send them looking for a typo.
		if errors.Is(err, broker.ErrProviderNotImplemented) {
			return cellcastv1alpha1.TrustReasonProviderNotImplemented,
				fmt.Sprintf("provider %q is not implemented in this build", trust.Spec.Provider)
		}
		return cellcastv1alpha1.TrustReasonInvalidConfiguration, err.Error()
	}

	if ref := trust.Spec.CredentialSource.SecretRef; ref != nil {
		if reason, msg := r.checkSecret(ctx, ref); reason != "" {
			return reason, msg
		}
	}

	k := trust.Spec.Kubernetes
	return cellcastv1alpha1.TrustReasonValid,
		fmt.Sprintf("mints for %s/%s via %s", k.Namespace, k.ServiceAccountName, trust.Spec.Provider)
}

// checkSecret reports a problem with the referenced credential, or empty
// strings when it resolves.
func (r *TrustConfigReconciler) checkSecret(ctx context.Context, ref *cellcastv1alpha1.SecretKeyReference) (string, string) {
	key := ref.Key
	if key == "" {
		key = "kubeconfig"
	}

	var secret corev1.Secret
	err := r.Secrets.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: ref.Name}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		return cellcastv1alpha1.TrustReasonCredentialMissing,
			fmt.Sprintf("secret %q does not exist", ref.Name)
	case err != nil:
		return cellcastv1alpha1.TrustReasonCredentialMissing,
			fmt.Sprintf("secret %q could not be read: %v", ref.Name, err)
	case len(secret.Data[key]) == 0:
		return cellcastv1alpha1.TrustReasonCredentialMissing,
			fmt.Sprintf("secret %q has no key %q", ref.Name, key)
	}
	return "", ""
}

func trustStatusEqual(a, b *cellcastv1alpha1.TrustConfigStatus) bool {
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
// It does not watch Secrets. Watching them would mean caching every Secret in
// the namespace, which is precisely the resident credential map this design
// avoids; an unready config polls instead, which is cheap because only broken
// ones do it.
func (r *TrustConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cellcastv1alpha1.TrustConfig{}).
		Named("trustconfig").
		Complete(r)
}

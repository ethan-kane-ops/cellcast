package hub

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

func trustConfig(name string, mutate func(*cellcastv1alpha1.TrustConfig)) *cellcastv1alpha1.TrustConfig {
	tc := &cellcastv1alpha1.TrustConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: cellcastv1alpha1.TrustConfigSpec{
			Provider:         cellcastv1alpha1.TrustProviderKubernetes,
			CredentialSource: cellcastv1alpha1.CredentialSource{InCluster: true},
			Kubernetes: &cellcastv1alpha1.KubernetesTrust{
				ServiceAccountName: "deployer",
				Namespace:          "apps",
			},
		},
	}
	if mutate != nil {
		mutate(tc)
	}
	return tc
}

func reconcileTrust(t *testing.T, objs ...client.Object) (*cellcastv1alpha1.TrustConfig, ctrl.Result, client.Client) {
	t.Helper()

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&cellcastv1alpha1.TrustConfig{}).
		Build()

	r := &TrustConfigReconciler{Client: k8s, Secrets: k8s, Namespace: testNamespace}
	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "cell-trust"},
	})
	if err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	var got cellcastv1alpha1.TrustConfig
	if err := k8s.Get(t.Context(), types.NamespacedName{Namespace: testNamespace, Name: "cell-trust"}, &got); err != nil {
		t.Fatalf("Get() = %v, want the reconciled trust config", err)
	}
	return &got, res, k8s
}

func TestTrustConfigReadiness(t *testing.T) {
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke-kubeconfig", Namespace: testNamespace},
		Data:       map[string][]byte{"kubeconfig": []byte("placeholder")},
	}

	tests := []struct {
		name        string
		trust       *cellcastv1alpha1.TrustConfig
		extra       []client.Object
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantRequeue bool
	}{
		{
			name:       "an in-cluster kubernetes config is ready",
			trust:      trustConfig("cell-trust", nil),
			wantStatus: metav1.ConditionTrue,
			wantReason: cellcastv1alpha1.TrustReasonValid,
		},
		{
			name: "a secret reference that resolves is ready",
			trust: trustConfig("cell-trust", func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{
					SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig", Key: "kubeconfig"},
				}
			}),
			extra:      []client.Object{kubeconfigSecret},
			wantStatus: metav1.ConditionTrue,
			wantReason: cellcastv1alpha1.TrustReasonValid,
		},
		{
			// The whole point of the condition: this is discovered when the
			// operator applies the config, not by the pipeline that needed it.
			name: "a secret reference that does not resolve is not ready",
			trust: trustConfig("cell-trust", func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{
					SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "absent"},
				}
			}),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  cellcastv1alpha1.TrustReasonCredentialMissing,
			wantRequeue: true,
		},
		{
			name: "a provider this build cannot use is called out as such",
			trust: trustConfig("cell-trust", func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.Provider = cellcastv1alpha1.TrustProviderAWS
			}),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  cellcastv1alpha1.TrustReasonProviderNotImplemented,
			wantRequeue: true,
		},
		{
			name: "an internally inconsistent config is invalid",
			trust: trustConfig("cell-trust", func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.Kubernetes = nil
			}),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  cellcastv1alpha1.TrustReasonInvalidConfiguration,
			wantRequeue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{tt.trust}, tt.extra...)
			got, res, _ := reconcileTrust(t, objs...)

			cond := meta.FindStatusCondition(got.Status.Conditions, cellcastv1alpha1.TrustConfigConditionReady)
			if cond == nil {
				t.Fatal("no Ready condition was published")
			}
			if cond.Status != tt.wantStatus {
				t.Errorf("Ready = %s (%s: %s), want %s", cond.Status, cond.Reason, cond.Message, tt.wantStatus)
			}
			if cond.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", cond.Reason, tt.wantReason)
			}
			if gotRequeue := res.RequeueAfter > 0; gotRequeue != tt.wantRequeue {
				t.Errorf("RequeueAfter = %s, want a requeue: %v", res.RequeueAfter, tt.wantRequeue)
			}
		})
	}
}

// TestTrustConfigRecoversWhenTheSecretAppears covers the reason unready configs
// requeue at all.
//
// Secrets are not watched, because watching them would cache every credential
// in the namespace. An operator who applies the TrustConfig before the Secret
// must still see the condition clear without editing the TrustConfig to prompt
// a reconcile.
func TestTrustConfigRecoversWhenTheSecretAppears(t *testing.T) {
	trust := trustConfig("cell-trust", func(tc *cellcastv1alpha1.TrustConfig) {
		tc.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{
			SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig"},
		}
	})

	got, res, k8s := reconcileTrust(t, trust)
	if meta.IsStatusConditionTrue(got.Status.Conditions, cellcastv1alpha1.TrustConfigConditionReady) {
		t.Fatal("Ready is True with no secret present")
	}
	if res.RequeueAfter != recheckInterval {
		t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, recheckInterval)
	}

	if err := k8s.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke-kubeconfig", Namespace: testNamespace},
		Data:       map[string][]byte{"kubeconfig": []byte("placeholder")},
	}); err != nil {
		t.Fatalf("Create(secret) = %v, want nil", err)
	}

	r := &TrustConfigReconciler{Client: k8s, Secrets: k8s, Namespace: testNamespace}
	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "cell-trust"},
	})
	if err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want no requeue once ready", res.RequeueAfter)
	}

	var after cellcastv1alpha1.TrustConfig
	if err := k8s.Get(t.Context(), types.NamespacedName{Namespace: testNamespace, Name: "cell-trust"}, &after); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if !meta.IsStatusConditionTrue(after.Status.Conditions, cellcastv1alpha1.TrustConfigConditionReady) {
		cond := meta.FindStatusCondition(after.Status.Conditions, cellcastv1alpha1.TrustConfigConditionReady)
		t.Errorf("Ready = %+v, want True once the secret exists", cond)
	}
}

// TestTrustConfigMessageNamesTheBoundary asserts the Ready message says what
// the credential will actually be scoped to.
//
// The service account named here is the RBAC boundary a caller receives, so
// `kubectl get trustconfig` has to show it rather than only that something is
// fine.
func TestTrustConfigMessageNamesTheBoundary(t *testing.T) {
	got, _, _ := reconcileTrust(t, trustConfig("cell-trust", nil))

	cond := meta.FindStatusCondition(got.Status.Conditions, cellcastv1alpha1.TrustConfigConditionReady)
	if cond == nil {
		t.Fatal("no Ready condition was published")
	}
	if want := "apps/deployer"; !strings.Contains(cond.Message, want) {
		t.Errorf("message = %q, want it to name %q", cond.Message, want)
	}
}

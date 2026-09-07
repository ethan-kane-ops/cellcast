package hub

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

func clusterFixture(name string, generation int64, state cellcastv1alpha1.ClusterState) *cellcastv1alpha1.Cluster {
	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNamespace,
			Generation: generation,
		},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       "https://" + name + ".example.test",
			Provider:       cellcastv1alpha1.ProviderGeneric,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "t1"},
			State:          state,
		},
	}
}

func TestClusterReconcilerPublishesObservedState(t *testing.T) {
	tests := []struct {
		name  string
		spec  cellcastv1alpha1.ClusterState
		want  cellcastv1alpha1.ClusterState
		genIn int64
	}{
		{name: "live", spec: cellcastv1alpha1.ClusterStateLive, want: cellcastv1alpha1.ClusterStateLive, genIn: 1},
		{name: "dark", spec: cellcastv1alpha1.ClusterStateDark, want: cellcastv1alpha1.ClusterStateDark, genIn: 4},
		{name: "draining", spec: cellcastv1alpha1.ClusterStateDraining, want: cellcastv1alpha1.ClusterStateDraining, genIn: 7},
		{name: "unset defaults to live", spec: "", want: cellcastv1alpha1.ClusterStateLive, genIn: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := newFakeClient(t, clusterFixture("c1", tt.genIn, tt.spec))
			r := &ClusterReconciler{Client: k8s}

			key := types.NamespacedName{Namespace: testNamespace, Name: "c1"}
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile() = %v, want nil", err)
			}

			var got cellcastv1alpha1.Cluster
			if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
				t.Fatalf("Get() = %v, want nil", err)
			}
			if got.Status.ObservedState != tt.want {
				t.Errorf("status.observedState = %q, want %q", got.Status.ObservedState, tt.want)
			}
			if got.Status.ObservedGeneration != tt.genIn {
				t.Errorf("status.observedGeneration = %d, want %d", got.Status.ObservedGeneration, tt.genIn)
			}
		})
	}
}

// TestClusterReconcilerIsIdempotent pins that a settled Cluster costs no etcd
// write. Status here must stay proportional to operator edits, never to
// reconcile frequency (docs/architecture.md ADR-002).
func TestClusterReconcilerIsIdempotent(t *testing.T) {
	k8s := newFakeClient(t, clusterFixture("c1", 3, cellcastv1alpha1.ClusterStateDark))
	r := &ClusterReconciler{Client: k8s}
	key := types.NamespacedName{Namespace: testNamespace, Name: "c1"}

	for i := range 3 {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile() call %d = %v, want nil", i, err)
		}
	}

	var got cellcastv1alpha1.Cluster
	if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if got.Status.ObservedState != cellcastv1alpha1.ClusterStateDark {
		t.Errorf("status.observedState = %q, want DARK", got.Status.ObservedState)
	}
	if got.ResourceVersion != "1000" {
		t.Errorf("resourceVersion = %q, want a single write past the initial object", got.ResourceVersion)
	}
}

// TestClusterReconcilerIgnoresDeleted asserts a deleted Cluster is not an
// error. Requeueing forever on an object that no longer exists is a hot loop.
func TestClusterReconcilerIgnoresDeleted(t *testing.T) {
	r := &ClusterReconciler{Client: newFakeClient(t)}
	key := types.NamespacedName{Namespace: testNamespace, Name: "gone"}

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() = %v, want nil for a missing cluster", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("Reconcile() = %+v, want no requeue", res)
	}
}

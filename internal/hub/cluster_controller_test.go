package hub

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

// TestClusterReconcilerPublishesAcceptingCondition covers the observable an
// operator actually reads. `kubectl get cluster` prints this column, and during
// an upgrade window it is the answer to "why is nothing landing here".
func TestClusterReconcilerPublishesAcceptingCondition(t *testing.T) {
	tests := []struct {
		name       string
		state      cellcastv1alpha1.ClusterState
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "live",
			state:      cellcastv1alpha1.ClusterStateLive,
			wantStatus: metav1.ConditionTrue,
			wantReason: cellcastv1alpha1.ClusterReasonLive,
		},
		{
			name:       "dark",
			state:      cellcastv1alpha1.ClusterStateDark,
			wantStatus: metav1.ConditionFalse,
			wantReason: cellcastv1alpha1.ClusterReasonDark,
		},
		{
			name:       "draining",
			state:      cellcastv1alpha1.ClusterStateDraining,
			wantStatus: metav1.ConditionFalse,
			wantReason: cellcastv1alpha1.ClusterReasonDraining,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := newFakeClient(t, clusterFixture("c1", 5, tt.state))
			r := &ClusterReconciler{Client: k8s}
			key := types.NamespacedName{Namespace: testNamespace, Name: "c1"}

			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile() = %v, want nil", err)
			}

			var got cellcastv1alpha1.Cluster
			if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
				t.Fatalf("Get() = %v, want nil", err)
			}

			cond := meta.FindStatusCondition(got.Status.Conditions, cellcastv1alpha1.ClusterConditionAccepting)
			if cond == nil {
				t.Fatalf("conditions = %+v, want an %s condition",
					got.Status.Conditions, cellcastv1alpha1.ClusterConditionAccepting)
			}
			if cond.Status != tt.wantStatus {
				t.Errorf("condition status = %q, want %q", cond.Status, tt.wantStatus)
			}
			if cond.Reason != tt.wantReason {
				t.Errorf("condition reason = %q, want %q", cond.Reason, tt.wantReason)
			}
			if cond.ObservedGeneration != 5 {
				t.Errorf("condition observedGeneration = %d, want 5", cond.ObservedGeneration)
			}
			if got.Status.StateSince == nil {
				t.Error("status.stateSince is nil, want the time the state was first observed")
			}
		})
	}
}

// TestStateSinceMovesOnlyOnTransition pins the field that answers "how long has
// this cell been draining". A timestamp rewritten on every reconcile answers
// nothing.
func TestStateSinceMovesOnlyOnTransition(t *testing.T) {
	k8s := newFakeClient(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive))

	clock := metav1.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &ClusterReconciler{Client: k8s, Now: func() metav1.Time { return clock }}
	key := types.NamespacedName{Namespace: testNamespace, Name: "c1"}

	reconcile := func() cellcastv1alpha1.Cluster {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile() = %v, want nil", err)
		}
		var got cellcastv1alpha1.Cluster
		if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
			t.Fatalf("Get() = %v, want nil", err)
		}
		return got
	}

	first := reconcile()
	if first.Status.StateSince == nil {
		t.Fatal("status.stateSince is nil after the first reconcile")
	}
	settled := *first.Status.StateSince

	if again := reconcile(); !again.Status.StateSince.Equal(&settled) {
		t.Errorf("status.stateSince = %v after a no-op reconcile, want it unchanged at %v",
			again.Status.StateSince, settled)
	}

	// Drain the cell and confirm the clock restarts.
	var cl cellcastv1alpha1.Cluster
	if err := k8s.Get(t.Context(), client.ObjectKey(key), &cl); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	cl.Spec.State = cellcastv1alpha1.ClusterStateDraining
	if err := k8s.Update(t.Context(), &cl); err != nil {
		t.Fatalf("Update() = %v, want nil", err)
	}
	clock = metav1.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	drained := reconcile()
	if drained.Status.ObservedState != cellcastv1alpha1.ClusterStateDraining {
		t.Fatalf("status.observedState = %q, want DRAINING", drained.Status.ObservedState)
	}
	if drained.Status.StateSince.Equal(&settled) {
		t.Error("status.stateSince did not move when the cell entered DRAINING")
	}
}

// TestClusterReconcilerRecordsTransitions asserts the operator-visible audit
// trail. A state change that leaves no trace in `kubectl describe` is one an
// operator has to correlate against hub logs they may not have.
func TestClusterReconcilerRecordsTransitions(t *testing.T) {
	k8s := newFakeClient(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateDraining))
	rec := events.NewFakeRecorder(4)
	r := &ClusterReconciler{Client: k8s, Recorder: rec}
	key := types.NamespacedName{Namespace: testNamespace, Name: "c1"}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}
	// A settled cell must not keep emitting events.
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	select {
	case event := <-rec.Events:
		if !strings.Contains(event, "StateChanged") || !strings.Contains(event, "DRAINING") {
			t.Errorf("event = %q, want a StateChanged event naming DRAINING", event)
		}
	default:
		t.Fatal("no event recorded, want one for the first observed state")
	}

	select {
	case event := <-rec.Events:
		t.Errorf("event = %q, want no second event for an unchanged state", event)
	default:
	}
}

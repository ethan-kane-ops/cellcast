package apitest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
)

// startManager runs the hub's real controller set against the test control
// plane, scoped to one namespace, and stops it when the test ends.
//
// It goes through hub.RegisterControllers rather than constructing reconcilers
// directly. The unit tests already call Reconcile by hand, so what is left to
// prove is the wiring: that each controller is registered, that its watches
// fire, and that a reconcile it did not ask for still reaches it.
func startManager(t *testing.T, ns string, index *capacity.Registry) {
	t.Helper()
	requireEnv(t)

	mgr, err := manager.New(restCfg, manager.Options{
		Scheme:  apiScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		// Controller names are unique per process, not per manager, and each
		// test here starts its own manager so that one test's watches cannot
		// service another's objects. The names still collide, and the check
		// exists to stop two controllers reporting the same metric, which is
		// not a concern in a test binary that exports no metrics.
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("building manager: %v", err)
	}
	if err := hub.RegisterControllers(mgr, index, ns); err != nil {
		t.Fatalf("registering controllers: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("manager exited with: %v", err)
		}
	})

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("caches never synced")
	}
}

// accepting reads the AcceptingPlacements condition off a cell.
func accepting(cl *cellcastv1alpha1.Cluster) *metav1.Condition {
	return meta.FindStatusCondition(cl.Status.Conditions, cellcastv1alpha1.ClusterConditionAccepting)
}

// TestClusterStateMachineThroughAManager drives the three states through a real
// API server and a running controller.
//
// The unit tests call Reconcile directly, so they show what the reconciler does
// with a request it is handed. This shows that patching spec.state produces
// that request at all, and that the status write survives a round trip through
// the status subresource. DRAINING and DARK both leave the condition False,
// which is why the reason is asserted rather than just the status: they are
// distinguishable only there.
func TestClusterStateMachineThroughAManager(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	startManager(t, ns, nil)
	ctx := context.Background()

	cl := validCluster(ns, "cell-1")
	if err := c.Create(ctx, cl); err != nil {
		t.Fatalf("create: %v", err)
	}
	key := client.ObjectKey{Name: "cell-1", Namespace: ns}

	// wantReason is a literal rather than the ClusterReason* constant on
	// purpose. Asserting against the constant compares the code to itself:
	// renaming ClusterReasonDark to "Draining" moves both sides together and
	// the test still passes, which is the thing this subtest exists to catch.
	// The reason is also API surface, read straight out of `kubectl get cluster
	// -o json` by whoever is asking why nothing is landing on a cell, so the
	// string itself is worth pinning.
	//
	// observedState is asserted alongside the condition because it is what the
	// placement engine reads. A condition that moved while observedState did
	// not would leave kubectl telling the truth and the hub acting on the old
	// state.
	awaitState := func(t *testing.T, want cellcastv1alpha1.ClusterState, wantStatus metav1.ConditionStatus, wantReason string) {
		t.Helper()
		eventually(t, 20*time.Second, fmt.Sprintf("waiting for %s/%s", want, wantReason), func() error {
			var got cellcastv1alpha1.Cluster
			if err := c.Get(ctx, key, &got); err != nil {
				return err
			}
			if got.Status.ObservedState != want {
				return fmt.Errorf("observedState = %q, want %q", got.Status.ObservedState, want)
			}
			cond := accepting(&got)
			if cond == nil {
				return fmt.Errorf("no %s condition", cellcastv1alpha1.ClusterConditionAccepting)
			}
			if cond.Status != wantStatus || cond.Reason != wantReason {
				return fmt.Errorf("condition = %s/%s, want %s/%s", cond.Status, cond.Reason, wantStatus, wantReason)
			}
			if got.Status.ObservedGeneration != got.Generation {
				return fmt.Errorf("observedGeneration = %d, generation = %d", got.Status.ObservedGeneration, got.Generation)
			}
			if got.Status.StateSince == nil {
				return fmt.Errorf("stateSince is unset")
			}
			return nil
		})
	}

	setState := func(t *testing.T, state cellcastv1alpha1.ClusterState) {
		t.Helper()
		var got cellcastv1alpha1.Cluster
		if err := c.Get(ctx, key, &got); err != nil {
			t.Fatalf("get: %v", err)
		}
		got.Spec.State = state
		if err := c.Update(ctx, &got); err != nil {
			t.Fatalf("patching state to %s: %v", state, err)
		}
	}

	t.Run("a new cell accepts placements", func(t *testing.T) {
		awaitState(t, cellcastv1alpha1.ClusterStateLive, metav1.ConditionTrue, "Live")
	})

	t.Run("draining stops new placements", func(t *testing.T) {
		setState(t, cellcastv1alpha1.ClusterStateDraining)
		awaitState(t, cellcastv1alpha1.ClusterStateDraining, metav1.ConditionFalse, "Draining")
	})

	t.Run("dark is distinguishable from draining", func(t *testing.T) {
		setState(t, cellcastv1alpha1.ClusterStateDark)
		awaitState(t, cellcastv1alpha1.ClusterStateDark, metav1.ConditionFalse, "Dark")
	})

	t.Run("a cell comes back", func(t *testing.T) {
		setState(t, cellcastv1alpha1.ClusterStateLive)
		awaitState(t, cellcastv1alpha1.ClusterStateLive, metav1.ConditionTrue, "Live")
	})

}

// TestDeletingACellReleasesItsCapacitySlot proves the delete watch reaches the
// reconciler.
//
// The unit test for this calls Reconcile with a request for an object that is
// already gone. That is the right unit test and it cannot show the part that
// breaks in production: whether a real delete produces that request. The index
// is bounded, so a delete that never arrives leaks a slot per decommissioned
// cell until retention expires.
func TestDeletingACellReleasesItsCapacitySlot(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	index := capacity.New(capacity.Options{
		Staleness: time.Minute,
		Retention: time.Hour,
		MaxCells:  8,
	})
	startManager(t, ns, index)
	ctx := context.Background()

	cl := validCluster(ns, "doomed")
	if err := c.Create(ctx, cl); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Precondition: without this, a manager that never started would fail the
	// delete assertion below and read as a broken delete watch.
	eventually(t, 20*time.Second, "waiting for the controller to observe the new cell", func() error {
		var got cellcastv1alpha1.Cluster
		if err := c.Get(ctx, client.ObjectKey{Name: "doomed", Namespace: ns}, &got); err != nil {
			return err
		}
		if got.Status.ObservedState == "" {
			return fmt.Errorf("status not written yet")
		}
		return nil
	})

	if err := index.Report(capacity.Report{
		Cell:                   "doomed",
		Nodes:                  3,
		CPUMilliAllocatable:    12000,
		CPUMilliCommitted:      4000,
		MemoryBytesAllocatable: 48 << 30,
		MemoryBytesCommitted:   16 << 30,
		Pods:                   40,
		PodCapacity:            330,
	}); err != nil {
		t.Fatalf("seeding capacity: %v", err)
	}
	if _, ok := index.Lookup("doomed"); !ok {
		t.Fatal("the index did not accept the seed report")
	}

	if err := c.Delete(ctx, cl); err != nil {
		t.Fatalf("delete: %v", err)
	}

	eventually(t, 20*time.Second, "waiting for the capacity slot to be released", func() error {
		if _, ok := index.Lookup("doomed"); ok {
			return fmt.Errorf("the index still holds the deleted cell")
		}
		return nil
	})
}

// TestPolicyReadinessFollowsTheFleet exercises the cross-resource watch.
//
// The policy controller watches Clusters as well as policies, because
// registering or relabelling a cell changes what every policy matches. That
// watch is registered in SetupWithManager and is invisible to a test that calls
// Reconcile directly, so this is the only place a dropped Watches() clause
// would be caught. The failure it prevents is a policy that reads NoCellsMatched
// forever after the cell it selects is registered.
func TestPolicyReadinessFollowsTheFleet(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	startManager(t, ns, nil)
	ctx := context.Background()

	pol := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-only", Namespace: ns},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{
				{Issuer: "https://token.actions.githubusercontent.com"},
			},
			PermittedCells: metav1.LabelSelector{
				MatchLabels: map[string]string{"tier": "prod"},
			},
		},
	}
	if err := c.Create(ctx, pol); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	polKey := client.ObjectKey{Name: "prod-only", Namespace: ns}

	awaitReady := func(t *testing.T, wantStatus metav1.ConditionStatus, wantReason string) {
		t.Helper()
		eventually(t, 20*time.Second, "waiting for Ready="+string(wantStatus)+"/"+wantReason, func() error {
			var got cellcastv1alpha1.PlacementPolicy
			if err := c.Get(ctx, polKey, &got); err != nil {
				return err
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, hub.PolicyConditionReady)
			if cond == nil {
				return fmt.Errorf("no Ready condition")
			}
			if cond.Status != wantStatus || cond.Reason != wantReason {
				return fmt.Errorf("Ready = %s/%s, want %s/%s", cond.Status, cond.Reason, wantStatus, wantReason)
			}
			return nil
		})
	}

	t.Run("a policy matching nothing says so", func(t *testing.T) {
		awaitReady(t, metav1.ConditionFalse, "NoCellsMatched")
	})

	t.Run("registering a matching cell makes it ready", func(t *testing.T) {
		cl := validCluster(ns, "prod-euw1")
		cl.Labels = map[string]string{"tier": "prod"}
		if err := c.Create(ctx, cl); err != nil {
			t.Fatalf("create cell: %v", err)
		}
		awaitReady(t, metav1.ConditionTrue, "CellsMatched")
	})

	t.Run("relabelling the cell away makes it unready again", func(t *testing.T) {
		var cl cellcastv1alpha1.Cluster
		key := client.ObjectKey{Name: "prod-euw1", Namespace: ns}
		if err := c.Get(ctx, key, &cl); err != nil {
			t.Fatalf("get cell: %v", err)
		}
		cl.Labels = map[string]string{"tier": "dev"}
		if err := c.Update(ctx, &cl); err != nil {
			t.Fatalf("relabel: %v", err)
		}
		awaitReady(t, metav1.ConditionFalse, "NoCellsMatched")
	})
}

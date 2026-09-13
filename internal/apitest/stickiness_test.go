package apitest

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// pipeline is the caller the stickiness tests place as.
var pipeline = &identity.Identity{
	Issuer:  envtestIssuer,
	Subject: "repo:acme/checkout:ref:refs/heads/main",
	Claims:  map[string]string{"repository": "acme/checkout"},
}

// startReplica runs one hub replica's placement path against the test control
// plane: a manager and its cache, the memory registered on it the way the hub
// registers it, and an engine reading through both. stop is safe to call more
// than once.
func startReplica(t *testing.T, ns string, index *capacity.Registry) (engine *placement.Engine, stop func()) {
	t.Helper()
	requireEnv(t)

	mgr, err := manager.New(restCfg, manager.Options{
		Scheme:  apiScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("building manager: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	memory := placement.NewMemory(mgr.GetClient(), ns, placement.MemoryOptions{
		ExpireAfter:  placement.DefaultExpireAfter,
		MaxPerPolicy: placement.DefaultMaxPerPolicy,
	}, log)

	ctx, cancel := context.WithCancel(context.Background())
	if err := memory.SetupWithManager(ctx, mgr); err != nil {
		cancel()
		t.Fatalf("registering the memory: %v", err)
	}
	engine = placement.NewEngine(mgr.GetClient(), index, ns, log, placement.WithMemory(memory))

	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-stopped; err != nil {
				t.Errorf("manager exited with: %v", err)
			}
		})
	}
	t.Cleanup(stop)

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("caches never synced")
	}
	return engine, stop
}

// heartbeats returns a capacity index as a replica rebuilds it from agent
// reports.
func heartbeats(t *testing.T, util map[string]float64) *capacity.Registry {
	t.Helper()
	index := capacity.New(capacity.Options{})
	for cell, u := range util {
		err := index.Report(capacity.Report{
			Cell:                   cell,
			Nodes:                  3,
			CPUMilliAllocatable:    10000,
			CPUMilliCommitted:      int64(u * 10000),
			MemoryBytesAllocatable: 10000,
		})
		if err != nil {
			t.Fatalf("reporting %s: %v", cell, err)
		}
	}
	return index
}

// seedTwoCells registers two cells and a policy permitting both.
func seedTwoCells(t *testing.T, c client.Client, ns string) *cellcastv1alpha1.PlacementPolicy {
	t.Helper()
	ctx := context.Background()

	for _, name := range []string{"c-a", "c-b"} {
		cl := validCluster(ns, name)
		cl.Labels = map[string]string{"env": "dev"}
		if err := c.Create(ctx, cl); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}
	pol := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: ns},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{{
				Issuer: envtestIssuer,
				Claims: map[string]string{"repository": "acme/checkout"},
			}},
			PermittedCells: metav1.LabelSelector{MatchLabels: map[string]string{"env": "dev"}},
		},
	}
	if err := c.Create(ctx, pol); err != nil {
		t.Fatalf("creating the policy: %v", err)
	}
	return pol
}

// TestAWorkloadStaysInItsCellAcrossAHubRestart is the case stickiness exists
// for. A replaced replica rebuilds capacity from heartbeats and may rank the
// fleet differently than its predecessor did; what was remembered survives in
// the API server, and the new replica keeps the workload where the old one put
// it.
func TestAWorkloadStaysInItsCellAcrossAHubRestart(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	pol := seedTwoCells(t, c, ns)
	ctx := context.Background()

	first, stop := startReplica(t, ns, heartbeats(t, map[string]float64{"c-a": 0.1, "c-b": 0.5}))
	d, err := first.Place(ctx, pipeline, placement.Request{Workload: "checkout-api"})
	if err != nil {
		t.Fatalf("first Place() = %v, want a placement", err)
	}
	if d.Cell != "c-a" {
		t.Fatalf("first placement = %q, want c-a, the least loaded", d.Cell)
	}
	if err := first.Remember(ctx, d); err != nil {
		t.Fatalf("Remember() = %v, want nil", err)
	}
	stop()

	// By the time the replacement has heard from the fleet, c-a is the busier
	// cell, and least-loaded alone would move the workload.
	second, _ := startReplica(t, ns, heartbeats(t, map[string]float64{"c-a": 0.8, "c-b": 0.1}))
	again, err := second.Place(ctx, pipeline, placement.Request{Workload: "checkout-api"})
	if err != nil {
		t.Fatalf("second Place() = %v, want a placement", err)
	}
	if again.Cell != "c-a" || !again.Kept() {
		t.Errorf("after the restart the workload went to %q (previous %q), want it kept in c-a", again.Cell, again.Previous)
	}

	// What survived is an object in the API server, owned by the policy it was
	// placed under so that deleting the policy deletes it.
	var recs cellcastv1alpha1.WorkloadPlacementList
	if err := c.List(ctx, &recs, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing workload placements: %v", err)
	}
	if len(recs.Items) != 1 {
		t.Fatalf("stored records = %d, want 1", len(recs.Items))
	}
	rec := recs.Items[0]
	if rec.Spec.Policy != "checkout" || rec.Spec.Workload != "checkout-api" || rec.Spec.Cell != "c-a" {
		t.Errorf("stored record = %+v, want checkout-api under checkout in c-a", rec.Spec)
	}
	if len(rec.OwnerReferences) != 1 || rec.OwnerReferences[0].UID != pol.UID {
		t.Errorf("owner = %+v, want the policy %s", rec.OwnerReferences, pol.UID)
	}
}

// TestAPolicyDeclaresItsStickiness: on unless a policy says otherwise, and the
// stored object says which, so `kubectl get -o yaml` answers the question
// without the reader knowing the default.
func TestAPolicyDeclaresItsStickiness(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	ctx := context.Background()

	pol := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: ns},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{{Issuer: envtestIssuer}},
		},
	}
	if err := c.Create(ctx, pol); err != nil {
		t.Fatalf("create: %v", err)
	}
	if pol.Spec.Stickiness == nil || pol.Spec.Stickiness.Mode != cellcastv1alpha1.StickinessPreferred {
		t.Errorf("stickiness = %+v, want mode Preferred filled in by the API server", pol.Spec.Stickiness)
	}

	bad := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-mode", Namespace: ns},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects:   []cellcastv1alpha1.SubjectSelector{{Issuer: envtestIssuer}},
			Stickiness: &cellcastv1alpha1.Stickiness{Mode: "Always"},
		},
	}
	err := c.Create(ctx, bad)
	if err == nil {
		t.Fatal("the API server accepted an unknown stickiness mode")
	}
	if !strings.Contains(err.Error(), "spec.stickiness.mode") {
		t.Errorf("error does not name spec.stickiness.mode: %v", err)
	}
}

// TestAWorkloadPlacementCanBeMovedAndNotRekeyed. Cell is how an operator moves
// a workload on its next deploy. Policy and workload are the key the object's
// name is derived from, so changing either would leave a record that matches
// nothing and looks as if it does.
func TestAWorkloadPlacementCanBeMovedAndNotRekeyed(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	ctx := context.Background()

	wp := &cellcastv1alpha1.WorkloadPlacement{
		ObjectMeta: metav1.ObjectMeta{Name: "wp-rekey", Namespace: ns},
		Spec: cellcastv1alpha1.WorkloadPlacementSpec{
			Policy:       "checkout",
			Workload:     "checkout-api",
			Cell:         "c-a",
			LastPlacedAt: metav1.NewTime(time.Now()),
		},
	}
	if err := c.Create(ctx, wp); err != nil {
		t.Fatalf("create: %v", err)
	}

	wp.Spec.Cell = "c-b"
	if err := c.Update(ctx, wp); err != nil {
		t.Fatalf("moving the workload to c-b: %v", err)
	}

	for field, edit := range map[string]func(*cellcastv1alpha1.WorkloadPlacement){
		"policy":   func(w *cellcastv1alpha1.WorkloadPlacement) { w.Spec.Policy = "payments" },
		"workload": func(w *cellcastv1alpha1.WorkloadPlacement) { w.Spec.Workload = "payments-api" },
	} {
		var current cellcastv1alpha1.WorkloadPlacement
		if err := c.Get(ctx, client.ObjectKeyFromObject(wp), &current); err != nil {
			t.Fatalf("get: %v", err)
		}
		edit(&current)
		err := c.Update(ctx, &current)
		if err == nil || !strings.Contains(err.Error(), field+" is immutable") {
			t.Errorf("changing %s: %v, want it refused as immutable", field, err)
		}
	}
}

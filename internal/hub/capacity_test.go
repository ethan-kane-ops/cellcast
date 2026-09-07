package hub

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const validCapacityBody = `{"nodes":6,"cpuMilliAllocatable":24000,"cpuMilliCommitted":6000,` +
	`"memoryBytesAllocatable":103079215104,"memoryBytesCommitted":25769803776,` +
	`"pods":80,"podCapacity":660}`

// capacityServer returns an authenticated server with a registry and a
// capacity index, plus the index so a test can inspect it directly.
func capacityServer(t *testing.T, objs ...client.Object) (*Server, *capacity.Registry, client.Client) {
	t.Helper()
	k8s := newFakeClient(t, objs...)
	index := capacity.New(capacity.Options{})
	id := &Identity{Issuer: "https://example.test", Subject: "system:serviceaccount:cellcast:agent"}
	srv := testServer(t,
		WithAuthenticator(stubAuthenticator{id: id}),
		WithClusterClient(k8s),
		WithCapacityRegistry(index),
	)
	return srv, index, k8s
}

func TestReportCapacityRequiresAuthentication(t *testing.T) {
	index := capacity.New(capacity.Options{})
	srv := testServer(t, WithClusterClient(newFakeClient(t)), WithCapacityRegistry(index))

	rec := do(t, srv, http.MethodPost, "/api/v1/clusters/c1/capacity", validCapacityBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST capacity = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if _, ok := index.Lookup("c1"); ok {
		t.Error("an unauthenticated report reached the index")
	}
}

// TestReportCapacityRequiresARegisteredCell is both the correctness rule and
// the bound on the index: capacity for a cell nobody registered can never be
// placed on, and accepting it would let an authenticated caller fill hub memory
// with invented names (docs/threat-model.md T-06).
func TestReportCapacityRequiresARegisteredCell(t *testing.T) {
	srv, index, _ := capacityServer(t)

	rec := do(t, srv, http.MethodPost, "/api/v1/clusters/ghost/capacity", validCapacityBody)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST capacity for an unregistered cell = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if _, ok := index.Lookup("ghost"); ok {
		t.Error("a report for an unregistered cell reached the index")
	}
}

func TestReportCapacity(t *testing.T) {
	srv, index, _ := capacityServer(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive))

	rec := do(t, srv, http.MethodPost, "/api/v1/clusters/c1/capacity", validCapacityBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST capacity = %d, want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	entry, ok := index.Lookup("c1")
	if !ok {
		t.Fatal("the report did not reach the index")
	}
	if entry.Health != capacity.HealthFresh {
		t.Errorf("health = %q, want %q", entry.Health, capacity.HealthFresh)
	}
	if entry.Nodes != 6 {
		t.Errorf("nodes = %d, want 6", entry.Nodes)
	}

	var got capacityEntryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Cell != "c1" {
		t.Errorf("cell = %q, want %q", got.Cell, "c1")
	}
	// 24000 committed against 96Gi, and 6000 against 24000 millicores: both
	// dimensions are a quarter, so the worst is a quarter.
	if got.Utilisation < 0.24 || got.Utilisation > 0.26 {
		t.Errorf("utilisation = %v, want about 0.25", got.Utilisation)
	}
}

func TestReportCapacityRejectsBadPayloads(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "not json", body: `not json`, want: http.StatusBadRequest},
		{name: "unknown field", body: `{"nodes":1,"cpuMilliAllocatable":1,"gpuCount":4}`, want: http.StatusBadRequest},
		{
			// The cell comes from the path. Accepting it in the body creates a
			// mismatch with no correct resolution.
			name: "cell in the body",
			body: `{"cell":"other","nodes":1,"cpuMilliAllocatable":1}`,
			want: http.StatusBadRequest,
		},
		{name: "negative committed", body: `{"cpuMilliAllocatable":1000,"cpuMilliCommitted":-1}`, want: http.StatusBadRequest},
		{name: "nothing allocatable", body: `{"nodes":3,"pods":10}`, want: http.StatusBadRequest},
		{name: "two objects", body: `{"cpuMilliAllocatable":1}{"cpuMilliAllocatable":2}`, want: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, index, _ := capacityServer(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive))

			rec := do(t, srv, http.MethodPost, "/api/v1/clusters/c1/capacity", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("POST capacity = %d, want %d (body %s)", rec.Code, tt.want, rec.Body.String())
			}
			if _, ok := index.Lookup("c1"); ok {
				t.Error("a rejected report reached the index")
			}
		})
	}
}

func TestReportCapacityWithoutIndex(t *testing.T) {
	srv := testServer(t,
		WithAuthenticator(stubAuthenticator{id: &Identity{Issuer: "https://example.test"}}),
		WithClusterClient(newFakeClient(t)),
	)

	rec := do(t, srv, http.MethodPost, "/api/v1/clusters/c1/capacity", validCapacityBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST capacity with no index = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestListCapacity(t *testing.T) {
	srv, _, _ := capacityServer(t,
		clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive),
		clusterFixture("c2", 1, cellcastv1alpha1.ClusterStateLive),
	)
	for _, cell := range []string{"c2", "c1"} {
		if rec := do(t, srv, http.MethodPost, "/api/v1/clusters/"+cell+"/capacity", validCapacityBody); rec.Code != http.StatusAccepted {
			t.Fatalf("POST capacity for %s = %d, want %d", cell, rec.Code, http.StatusAccepted)
		}
	}

	rec := do(t, srv, http.MethodGet, "/api/v1/capacity", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET capacity = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload struct {
		Cells []struct {
			Cell   string `json:"cell"`
			Health string `json:"health"`
		} `json:"cells"`
		StalenessWindow string `json:"stalenessWindow"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(payload.Cells) != 2 {
		t.Fatalf("cells = %+v, want two entries", payload.Cells)
	}
	// Ordered by name so two calls are diffable, despite being reported out of
	// order above.
	if payload.Cells[0].Cell != "c1" || payload.Cells[1].Cell != "c2" {
		t.Errorf("cells = %+v, want them ordered by name", payload.Cells)
	}
	if payload.StalenessWindow != capacity.DefaultStaleness.String() {
		t.Errorf("stalenessWindow = %q, want %q", payload.StalenessWindow, capacity.DefaultStaleness)
	}
}

// TestDeletingAClusterForgetsItsCapacity keeps a decommissioned cell from
// holding a slot in the bounded index until retention expires.
func TestDeletingAClusterForgetsItsCapacity(t *testing.T) {
	srv, index, k8s := capacityServer(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive))

	if rec := do(t, srv, http.MethodPost, "/api/v1/clusters/c1/capacity", validCapacityBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST capacity = %d, want %d", rec.Code, http.StatusAccepted)
	}

	cl := clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive)
	if err := k8s.Delete(t.Context(), cl); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}

	r := &ClusterReconciler{Client: k8s, Capacity: index}
	if _, err := r.Reconcile(t.Context(), reconcileRequest("c1")); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	if _, ok := index.Lookup("c1"); ok {
		t.Error("capacity survived the cluster being deleted")
	}
}

// TestCapacityConfigRejectsRetentionUnderStaleness pins the guard that would
// otherwise drop entries while they are still fresh.
func TestCapacityConfigRejectsRetentionUnderStaleness(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CapacityStaleness = 5 * time.Minute
	cfg.CapacityRetention = time.Minute

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for retention shorter than staleness")
	}
}

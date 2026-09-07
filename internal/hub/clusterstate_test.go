package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

func decodeCluster(t *testing.T, body string) clusterResponse {
	t.Helper()
	var got clusterResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding response %q: %v", body, err)
	}
	return got
}

// TestSetClusterStateRequiresAuthentication pins the state endpoint to the same
// fail-closed default as the rest of the API. Draining a cell diverts every
// deploy that would have landed on it.
func TestSetClusterStateRequiresAuthentication(t *testing.T) {
	srv := testServer(t, WithClusterClient(newFakeClient(t)))

	rec := do(t, srv, http.MethodPatch, "/api/v1/clusters/c1/state", `{"state":"DRAINING"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("PATCH state = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSetClusterState(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     int
		wantSate cellcastv1alpha1.ClusterState
	}{
		{name: "drain", body: `{"state":"DRAINING"}`, want: http.StatusOK, wantSate: cellcastv1alpha1.ClusterStateDraining},
		{name: "darken", body: `{"state":"DARK"}`, want: http.StatusOK, wantSate: cellcastv1alpha1.ClusterStateDark},
		{name: "back to live", body: `{"state":"LIVE"}`, want: http.StatusOK, wantSate: cellcastv1alpha1.ClusterStateLive},
		{name: "unknown state", body: `{"state":"RETIRED"}`, want: http.StatusBadRequest},
		{name: "omitted state", body: `{}`, want: http.StatusBadRequest},
		{name: "lowercase is not accepted", body: `{"state":"draining"}`, want: http.StatusBadRequest},
		{name: "unknown field", body: `{"state":"DARK","force":true}`, want: http.StatusBadRequest},
		{name: "not an object", body: `"DARK"`, want: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := newFakeClient(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive))
			srv := registryServer(t, k8s)

			rec := do(t, srv, http.MethodPatch, "/api/v1/clusters/c1/state", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("PATCH state = %d, want %d (body %s)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusOK {
				return
			}

			var stored cellcastv1alpha1.Cluster
			key := client.ObjectKey{Namespace: testNamespace, Name: "c1"}
			if err := k8s.Get(t.Context(), key, &stored); err != nil {
				t.Fatalf("Get() = %v, want nil", err)
			}
			if stored.Spec.State != tt.wantSate {
				t.Errorf("spec.state = %q, want %q", stored.Spec.State, tt.wantSate)
			}
			if got := decodeCluster(t, rec.Body.String()); got.State != string(tt.wantSate) {
				t.Errorf("response state = %q, want %q", got.State, tt.wantSate)
			}
		})
	}
}

// TestSetClusterStateLeavesTheRestOfSpecAlone pins the patch to one field. The
// read that produces the patch comes from a cache that may be behind, so a full
// update would let a drain silently revert an endpoint or label edit made in
// between.
func TestSetClusterStateLeavesTheRestOfSpecAlone(t *testing.T) {
	fixture := clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive)
	fixture.Labels = map[string]string{"env": "prd"}
	k8s := newFakeClient(t, fixture)
	srv := registryServer(t, k8s)

	if rec := do(t, srv, http.MethodPatch, "/api/v1/clusters/c1/state", `{"state":"DRAINING"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH state = %d, want %d", rec.Code, http.StatusOK)
	}

	var stored cellcastv1alpha1.Cluster
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "c1"}, &stored); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if stored.Spec.Endpoint != "https://c1.example.test" {
		t.Errorf("spec.endpoint = %q, want it untouched", stored.Spec.Endpoint)
	}
	if stored.Spec.TrustConfigRef.Name != "t1" {
		t.Errorf("spec.trustConfigRef.name = %q, want it untouched", stored.Spec.TrustConfigRef.Name)
	}
	if stored.Labels["env"] != "prd" {
		t.Errorf("labels = %v, want them untouched", stored.Labels)
	}
}

func TestSetClusterStateOnMissingCluster(t *testing.T) {
	srv := registryServer(t, newFakeClient(t))

	rec := do(t, srv, http.MethodPatch, "/api/v1/clusters/nope/state", `{"state":"DARK"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH state = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSetClusterStateWithoutRegistry(t *testing.T) {
	srv := registryServer(t, nil)

	rec := do(t, srv, http.MethodPatch, "/api/v1/clusters/c1/state", `{"state":"DARK"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PATCH state = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestSetClusterStateRejectsOversizedBody keeps a one-enum endpoint from being
// a memory sink (docs/threat-model.md T-06).
func TestSetClusterStateRejectsOversizedBody(t *testing.T) {
	srv := registryServer(t, newFakeClient(t, clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateLive)))

	body := `{"state":"` + strings.Repeat("A", maxStateChangeBytes) + `"}`
	rec := do(t, srv, http.MethodPatch, "/api/v1/clusters/c1/state", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH oversized state = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestGetCluster(t *testing.T) {
	fixture := clusterFixture("c1", 1, cellcastv1alpha1.ClusterStateDraining)
	srv := registryServer(t, newFakeClient(t, fixture))

	rec := do(t, srv, http.MethodGet, "/api/v1/clusters/c1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cluster = %d, want %d", rec.Code, http.StatusOK)
	}

	got := decodeCluster(t, rec.Body.String())
	if got.Name != "c1" {
		t.Errorf("name = %q, want %q", got.Name, "c1")
	}
	if got.State != string(cellcastv1alpha1.ClusterStateDraining) {
		t.Errorf("state = %q, want DRAINING", got.State)
	}
}

func TestGetClusterNotFound(t *testing.T) {
	srv := registryServer(t, newFakeClient(t))

	rec := do(t, srv, http.MethodGet, "/api/v1/clusters/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET cluster = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

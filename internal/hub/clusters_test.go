package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

const testNamespace = "cellcast-system"

// newFakeClient returns a client backed by an in-memory tracker, with the
// status subresource enabled so reconciler writes behave as they do against a
// real API server. Installing the CRDs into an actual API server is ENG-177.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cellcastv1alpha1.Cluster{}).
		WithObjects(objs...).
		Build()
}

// registryServer returns an authenticated server backed by k8s.
func registryServer(t *testing.T, k8s client.Client) *Server {
	t.Helper()
	id := &identity.Identity{Issuer: "https://example.test", Subject: "repo:example/app"}
	opts := []Option{WithAuthenticator(stubAuthenticator{id: id})}
	if k8s != nil {
		opts = append(opts, WithClusterClient(k8s))
	}
	return testServer(t, opts...)
}

func do(t *testing.T, srv *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	rec := httptest.NewRecorder()
	srv.apiHandler().ServeHTTP(rec, req)
	return rec
}

// selfSignedPEM returns a real PEM certificate for CA bundle fixtures.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cellcast-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func validRegistration(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(clusterRegistration{
		Name:           "eks-prod-euw1",
		Endpoint:       "https://prod-euw1.example.test",
		CABundle:       selfSignedPEM(t),
		Provider:       "eks",
		TrustConfigRef: "prod-irsa",
		Labels:         map[string]string{"env": "prd", "group": "devstacks", "region": "eu-west-1"},
	})
	if err != nil {
		t.Fatalf("marshalling registration: %v", err)
	}
	return string(body)
}

// TestRegisterClusterRequiresAuthentication pins that the registry endpoints
// inherit the fail-closed default. Registering a cell is the privileged action
// described in docs/threat-model.md T-04.
func TestRegisterClusterRequiresAuthentication(t *testing.T) {
	srv := testServer(t, WithClusterClient(newFakeClient(t)))

	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/clusters"},
		{http.MethodGet, "/api/v1/clusters"},
	} {
		rec := do(t, srv, tc.method, tc.target, `{}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.target, rec.Code, http.StatusUnauthorized)
		}
	}
}

// TestRegisterClusterRejectsCredentials is the invariant that keeps ADR-004
// true: the registry holds trust configuration and never a credential.
func TestRegisterClusterRejectsCredentials(t *testing.T) {
	base := `"name":"c1","endpoint":"https://c1.example.test","provider":"generic","trustConfigRef":"t1"`

	tests := []struct {
		name string
		body string
	}{
		{name: "bearer token", body: `{` + base + `,"token":"eyJhbGciOi"}`},
		{name: "kubeconfig", body: `{` + base + `,"kubeconfig":"apiVersion: v1"}`},
		{name: "client key data", body: `{` + base + `,"clientKeyData":"LS0tLS1CRUdJTg=="}`},
		{name: "basic auth password", body: `{` + base + `,"username":"admin","password":"hunter2"}`},
		{name: "exec provider", body: `{` + base + `,"execProvider":{"command":"aws"}}`},
		{name: "capitalised token", body: `{` + base + `,"Token":"eyJhbGciOi"}`},
		{name: "nested under an unknown object", body: `{` + base + `,"auth":{"token":"eyJhbGciOi"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := newFakeClient(t)
			rec := do(t, registryServer(t, k8s), http.MethodPost, "/api/v1/clusters", tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /api/v1/clusters = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body)
			}

			var list cellcastv1alpha1.ClusterList
			if err := k8s.List(t.Context(), &list); err != nil {
				t.Fatalf("List() = %v, want nil", err)
			}
			if len(list.Items) != 0 {
				t.Fatalf("rejected registration still stored %d cluster(s)", len(list.Items))
			}
		})
	}
}

func TestRegisterClusterValidation(t *testing.T) {
	base := func(mutate func(*clusterRegistration)) string {
		reg := clusterRegistration{
			Name:           "c1",
			Endpoint:       "https://c1.example.test",
			Provider:       "generic",
			TrustConfigRef: "t1",
		}
		mutate(&reg)
		body, err := json.Marshal(reg)
		if err != nil {
			t.Fatalf("marshalling registration: %v", err)
		}
		return string(body)
	}

	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{
			name:    "missing name",
			body:    base(func(r *clusterRegistration) { r.Name = "" }),
			wantMsg: "name is required",
		},
		{
			name:    "name is not a dns subdomain",
			body:    base(func(r *clusterRegistration) { r.Name = "Prod Cluster" }),
			wantMsg: "name:",
		},
		{
			name:    "plaintext endpoint",
			body:    base(func(r *clusterRegistration) { r.Endpoint = "http://c1.example.test" }),
			wantMsg: "endpoint must use https",
		},
		{
			name:    "endpoint embeds credentials",
			body:    base(func(r *clusterRegistration) { r.Endpoint = "https://admin:hunter2@c1.example.test" }),
			wantMsg: "endpoint must not embed credentials",
		},
		{
			name:    "unknown provider",
			body:    base(func(r *clusterRegistration) { r.Provider = "openshift" }),
			wantMsg: "provider must be one of",
		},
		{
			name:    "missing trust config reference",
			body:    base(func(r *clusterRegistration) { r.TrustConfigRef = "" }),
			wantMsg: "trustConfigRef is required",
		},
		{
			name:    "unknown state",
			body:    base(func(r *clusterRegistration) { r.State = "PAUSED" }),
			wantMsg: "state must be one of",
		},
		{
			name:    "reserved label prefix",
			body:    base(func(r *clusterRegistration) { r.Labels = map[string]string{"cellcast.io/verified": "true"} }),
			wantMsg: "reserved",
		},
		{
			name:    "reserved label subdomain",
			body:    base(func(r *clusterRegistration) { r.Labels = map[string]string{"policy.cellcast.io/tier": "gold"} }),
			wantMsg: "reserved",
		},
		{
			name:    "invalid label value",
			body:    base(func(r *clusterRegistration) { r.Labels = map[string]string{"env": "not a label value"} }),
			wantMsg: `label \"env\"`,
		},
		{
			name:    "ca bundle is not pem",
			body:    base(func(r *clusterRegistration) { r.CABundle = []byte("not a certificate") }),
			wantMsg: "caBundle is not valid PEM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, registryServer(t, newFakeClient(t)), http.MethodPost, "/api/v1/clusters", tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /api/v1/clusters = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tt.wantMsg) {
				t.Errorf("error body = %s, want it to mention %q", rec.Body.String(), tt.wantMsg)
			}
		})
	}
}

func TestRegisterClusterStoresTheEntry(t *testing.T) {
	k8s := newFakeClient(t)
	rec := do(t, registryServer(t, k8s), http.MethodPost, "/api/v1/clusters", validRegistration(t))

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/clusters = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body)
	}

	var stored cellcastv1alpha1.Cluster
	key := client.ObjectKey{Namespace: testNamespace, Name: "eks-prod-euw1"}
	if err := k8s.Get(t.Context(), key, &stored); err != nil {
		t.Fatalf("Get(%v) = %v, want the registered cluster", key, err)
	}

	if got, want := stored.Spec.Provider, cellcastv1alpha1.ProviderEKS; got != want {
		t.Errorf("spec.provider = %q, want %q", got, want)
	}
	if got, want := stored.Spec.TrustConfigRef.Name, "prod-irsa"; got != want {
		t.Errorf("spec.trustConfigRef.name = %q, want %q", got, want)
	}
	if got, want := stored.Spec.State, cellcastv1alpha1.ClusterStateLive; got != want {
		t.Errorf("spec.state = %q, want %q by default", got, want)
	}
	if got, want := stored.Labels["group"], "devstacks"; got != want {
		t.Errorf("labels[group] = %q, want %q", got, want)
	}

	var body clusterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Name != "eks-prod-euw1" || body.State != string(cellcastv1alpha1.ClusterStateLive) {
		t.Errorf("response = %+v, want the stored registration echoed back", body)
	}
}

func TestRegisterClusterRejectsDuplicateName(t *testing.T) {
	k8s := newFakeClient(t)
	srv := registryServer(t, k8s)

	if rec := do(t, srv, http.MethodPost, "/api/v1/clusters", validRegistration(t)); rec.Code != http.StatusCreated {
		t.Fatalf("first POST = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec := do(t, srv, http.MethodPost, "/api/v1/clusters", validRegistration(t)); rec.Code != http.StatusConflict {
		t.Fatalf("second POST = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestListClustersFiltersByLabelSelector(t *testing.T) {
	cell := func(name, env, group string) *cellcastv1alpha1.Cluster {
		return &cellcastv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
				Labels:    map[string]string{"env": env, "group": group},
			},
			Spec: cellcastv1alpha1.ClusterSpec{
				Endpoint:       "https://" + name + ".example.test",
				Provider:       cellcastv1alpha1.ProviderGeneric,
				TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "t1"},
				State:          cellcastv1alpha1.ClusterStateLive,
			},
		}
	}

	k8s := newFakeClient(t,
		cell("prod-a", "prd", "devstacks"),
		cell("prod-b", "prd", "platform"),
		cell("stage-a", "stg", "devstacks"),
	)
	srv := registryServer(t, k8s)

	tests := []struct {
		name     string
		selector string
		want     []string
	}{
		{name: "no selector returns everything", selector: "", want: []string{"prod-a", "prod-b", "stage-a"}},
		{name: "equality", selector: "env=prd", want: []string{"prod-a", "prod-b"}},
		{name: "conjunction", selector: "env=prd,group=devstacks", want: []string{"prod-a"}},
		{name: "inequality", selector: "env!=prd", want: []string{"stage-a"}},
		{name: "set membership", selector: "group in (devstacks)", want: []string{"prod-a", "stage-a"}},
		{name: "no match is empty, not an error", selector: "env=qa", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/api/v1/clusters"
			if tt.selector != "" {
				target += "?labelSelector=" + url.QueryEscape(tt.selector)
			}
			rec := do(t, srv, http.MethodGet, target, "")

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d (body: %s)", target, rec.Code, http.StatusOK, rec.Body)
			}

			var body struct {
				Clusters []clusterResponse `json:"clusters"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding response: %v", err)
			}

			got := make([]string, 0, len(body.Clusters))
			for _, c := range body.Clusters {
				got = append(got, c.Name)
			}
			sort.Strings(got)

			if !slices.Equal(got, tt.want) {
				t.Errorf("clusters = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListClustersRejectsInvalidSelector(t *testing.T) {
	srv := registryServer(t, newFakeClient(t))
	rec := do(t, srv, http.MethodGet, "/api/v1/clusters?labelSelector=env%3D%3D%3Dprd", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET with a malformed selector = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestRegistryUnavailableWithoutClient asserts that a hub with no cluster
// access reports it rather than panicking on the first registration.
func TestRegistryUnavailableWithoutClient(t *testing.T) {
	srv := registryServer(t, nil)

	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/clusters", `{}`},
		{http.MethodGet, "/api/v1/clusters", ""},
	} {
		rec := do(t, srv, tc.method, tc.target, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.target, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

// TestRegistrationBodyIsBounded keeps an oversized payload from being buffered.
func TestRegistrationBodyIsBounded(t *testing.T) {
	oversized := `{"name":"c1","endpoint":"https://c1.example.test","provider":"generic","trustConfigRef":"` +
		strings.Repeat("t", maxRegistrationBytes) + `"}`

	rec := do(t, registryServer(t, newFakeClient(t)), http.MethodPost, "/api/v1/clusters", oversized)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized POST = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// The two handlers fuzzed here read bodies chosen by an authenticated caller,
// which under the threat model is a CI pipeline somebody else controls. The
// handler is called directly rather than through apiHandler, because the
// recoverPanic middleware would turn the failure this is looking for into a
// tidy 500.

// fuzzRequest runs one body through a handler with an authenticated caller in
// context, and returns the status and body.
func fuzzRequest(t *testing.T, h http.HandlerFunc, method, path, body string) (int, string) {
	t.Helper()

	id, err := fixedIdentity{}.Authenticate(t.Context(), nil)
	if err != nil {
		t.Fatalf("building the caller identity: %v", err)
	}

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(withIdentity(t.Context(), id))
	rec := httptest.NewRecorder()
	h(rec, req)

	return rec.Code, rec.Body.String()
}

// FuzzPlacementRequestBody covers the route that decides and mints.
//
// The property that matters is the last one checked: a credential leaves the
// hub only on a 200. Everything else in this function is there so that a body
// that reaches an unexpected code fails loudly rather than being explained away.
func FuzzPlacementRequestBody(f *testing.F) {
	f.Add(`{"workload":"checkout-api"}`)
	f.Add(`{"workload":"checkout-api","ttl":"15m","dryRun":true,"explain":true}`)
	f.Add(`{"workload":"checkout-api","targetDark":true}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`{"workload":null}`)
	f.Add(`{"workload":"a","ttl":"-5m"}`)
	f.Add(`{"workload":"a","ttl":"9223372036854775807h"}`)
	f.Add(`{"workload":"a","unknownField":1}`)
	f.Add(`{"workload":"` + strings.Repeat("a", 8192) + `"}`)
	f.Add(strings.Repeat(`{"workload":"a"}`, 100))
	f.Add("{\"workload\":\"\x00\"}")

	// Reasons a client is contractually able to act on. A refusal carrying
	// anything else is one the client records as Unknown and treats as an
	// authorization refusal, which is safe but not the answer.
	known := refusal.All()

	f.Fuzz(func(t *testing.T, body string) {
		scheme, err := NewScheme()
		if err != nil {
			t.Fatalf("NewScheme() = %v, want nil", err)
		}
		k8s := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(registeredCell("prod-euw1")).Build()

		srv := testServer(t,
			WithClusterClient(k8s),
			WithPlacer(&stubPlacer{decision: testDecision()}),
			WithMinter(&stubMinter{}),
		)

		status, out := fuzzRequest(t, srv.handlePlacement, http.MethodPost, "/api/v1/placement", body)

		allowed := []int{
			http.StatusOK, http.StatusBadRequest, http.StatusForbidden,
			http.StatusConflict, http.StatusInternalServerError, http.StatusServiceUnavailable,
		}
		if !slices.Contains(allowed, status) {
			t.Fatalf("body %q produced status %d, which is not in the route's contract", body, status)
		}

		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("body %q produced a response that is not JSON: %s", body, out)
		}

		if status != http.StatusOK {
			reason, _ := decoded["reason"].(string)
			if !slices.Contains(known, refusal.Reason(reason)) {
				t.Fatalf("body %q was refused with reason %q, which is not in the wire contract", body, reason)
			}
		}

		// The one that would matter. Everything above is a shape check; this is
		// the boundary.
		if strings.Contains(out, mintedToken) && status != http.StatusOK {
			t.Fatalf("body %q leaked a credential on a %d response: %s", body, status, out)
		}
		if status == http.StatusOK && decoded["decidedFor"] != "repo:acme/app:ref:refs/heads/main" {
			t.Fatalf("body %q was decided for %v, not for the authenticated caller", body, decoded["decidedFor"])
		}
	})
}

// FuzzClusterRegistration covers the payload that defines what the hub will
// later mint against.
//
// It also checks a claim made in a comment and never tested: credentialFields
// scans only top-level keys, on the grounds that DisallowUnknownFields catches
// a nested one because the object containing it is itself unknown. That is the
// kind of reasoning a fuzzer is for.
func FuzzClusterRegistration(f *testing.F) {
	valid := `{"name":"prod-euw1","endpoint":"https://prod.example.test",` +
		`"provider":"eks","trustConfigRef":"trust"}`

	f.Add(valid)
	f.Add(``)
	f.Add(`{}`)
	f.Add(`{"name":"prod-euw1","endpoint":"https://x.test","provider":"eks","trustConfigRef":"t","bearerToken":"secret"}`)
	f.Add(`{"name":"prod-euw1","endpoint":"https://x.test","provider":"eks","trustConfigRef":"t","token":{"nested":"secret"}}`)
	f.Add(`{"name":"prod-euw1","endpoint":"https://x.test","provider":"eks","trustConfigRef":"t","reporter":{"issuer":"https://i.test","subject":"s","token":"secret"}}`)
	f.Add(`{"name":"UPPERCASE","endpoint":"http://insecure.test","provider":"nope","trustConfigRef":""}`)
	f.Add(`{"name":"a","endpoint":"https://x.test","provider":"eks","trustConfigRef":"t","labels":{"":""}}`)
	f.Add(`{"name":"` + strings.Repeat("a", 300) + `","endpoint":"https://x.test","provider":"eks","trustConfigRef":"t"}`)
	f.Add(valid + valid)
	f.Add(`{"name":"a","endpoint":"https://x.test","provider":"eks","trustConfigRef":"t","caBundle":"!!!not base64!!!"}`)

	f.Fuzz(func(t *testing.T, body string) {
		scheme, err := NewScheme()
		if err != nil {
			t.Fatalf("NewScheme() = %v, want nil", err)
		}
		k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
		srv := testServer(t, WithClusterClient(k8s))

		status, out := fuzzRequest(t, srv.handleRegisterCluster, http.MethodPost, "/api/v1/clusters", body)

		allowed := []int{
			http.StatusCreated, http.StatusBadRequest,
			http.StatusConflict, http.StatusInternalServerError, http.StatusServiceUnavailable,
		}
		if !slices.Contains(allowed, status) {
			t.Fatalf("body %q produced status %d, which is not in the route's contract", body, status)
		}
		if !json.Valid([]byte(out)) {
			t.Fatalf("body %q produced a response that is not JSON: %s", body, out)
		}
		if status != http.StatusCreated {
			return
		}

		// ADR-004: the registry holds trust configuration, never credentials.
		// A registration that stored one would make every later mint pointless.
		if field, found := anyCredentialField(body); found {
			t.Fatalf("body %q was accepted while carrying credential field %q", body, field)
		}

		var stored cellcastv1alpha1.ClusterList
		if err := k8s.List(t.Context(), &stored, client.InNamespace(srv.cfg.Namespace)); err != nil {
			t.Fatalf("listing the registry: %v", err)
		}
		if len(stored.Items) != 1 {
			t.Fatalf("a 201 stored %d cells, want exactly 1", len(stored.Items))
		}

		cell := stored.Items[0]
		if cell.Name == "" {
			t.Fatal("a 201 stored a cell with no name")
		}
		if !strings.HasPrefix(cell.Spec.Endpoint, "https://") {
			t.Fatalf("a 201 stored the plaintext endpoint %q", cell.Spec.Endpoint)
		}
		switch cell.Spec.Provider {
		case cellcastv1alpha1.ProviderEKS, cellcastv1alpha1.ProviderGKE,
			cellcastv1alpha1.ProviderAKS, cellcastv1alpha1.ProviderGeneric:
		default:
			t.Fatalf("a 201 stored the unknown provider %q", cell.Spec.Provider)
		}
		if cell.Spec.TrustConfigRef.Name == "" {
			t.Fatal("a 201 stored a cell with no trust configuration; it can never be minted for")
		}
	})
}

// anyCredentialField finds a banned key at any depth, which is stricter than
// the top-level scan the decoder performs. The decoder is supposed to reject a
// nested one anyway, as an unknown field.
func anyCredentialField(body string) (string, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return "", false
	}
	return walkForCredential(decoded)
}

func walkForCredential(v any) (string, bool) {
	switch value := v.(type) {
	case map[string]any:
		for key, nested := range value {
			for _, banned := range credentialFields {
				if strings.EqualFold(key, banned) {
					return key, true
				}
			}
			if found, ok := walkForCredential(nested); ok {
				return found, true
			}
		}
	case []any:
		for _, nested := range value {
			if found, ok := walkForCredential(nested); ok {
				return found, true
			}
		}
	}
	return "", false
}

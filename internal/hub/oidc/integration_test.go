package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub"
)

// TestEndToEndPipelineRegistersACluster runs the whole request path: a real
// RS256 token from a real issuer, over real HTTP, through the authentication
// middleware, into the registration handler, and out to the registry.
//
// This is the criterion ENG-172 defers from a live GitHub Actions job to a
// fixture issuer while the repository is private. Nothing here is stubbed
// except the issuer and the API server.
func TestEndToEndPipelineRegistersACluster(t *testing.T) {
	iss := newTestIssuer(t)

	scheme, err := hub.NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cellcastv1alpha1.Cluster{}).
		Build()

	authn := testAuthenticator(t, iss, "github")

	cfg := hub.DefaultConfig()
	cfg.Addr = freePort(t)
	cfg.ProbeAddr = freePort(t)

	srv, err := hub.NewServer(cfg, discardLogger(),
		hub.WithAuthenticator(authn),
		hub.WithClusterClient(k8s),
	)
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitForReady(t, cfg.ProbeAddr)

	api := "http://" + cfg.Addr + "/api/v1/clusters"
	body := `{"name":"eks-prod-euw1","endpoint":"https://prod-euw1.example.test",` +
		`"provider":"eks","trustConfigRef":"prod-irsa",` +
		`"labels":{"env":"prd","group":"devstacks"}}`

	t.Run("no token is rejected", func(t *testing.T) {
		resp := postCluster(t, api, "", body)
		if resp != http.StatusUnauthorized {
			t.Errorf("POST without a token = %d, want %d", resp, http.StatusUnauthorized)
		}
	})

	t.Run("token from an unknown issuer is rejected", func(t *testing.T) {
		claims := validClaims(iss)
		claims["iss"] = "https://evil.example.test"
		resp := postCluster(t, api, iss.sign(t, claims), body)
		if resp != http.StatusUnauthorized {
			t.Errorf("POST with a foreign issuer = %d, want %d", resp, http.StatusUnauthorized)
		}
	})

	t.Run("a valid pipeline token registers the cell", func(t *testing.T) {
		resp := postCluster(t, api, iss.sign(t, validClaims(iss)), body)
		if resp != http.StatusCreated {
			t.Fatalf("POST with a valid token = %d, want %d", resp, http.StatusCreated)
		}

		var stored cellcastv1alpha1.Cluster
		key := client.ObjectKey{Namespace: cfg.Namespace, Name: "eks-prod-euw1"}
		if err := k8s.Get(t.Context(), key, &stored); err != nil {
			t.Fatalf("Get(%v) = %v, want the registered cluster", key, err)
		}
		if stored.Spec.TrustConfigRef.Name != "prod-irsa" {
			t.Errorf("spec.trustConfigRef.name = %q, want %q", stored.Spec.TrustConfigRef.Name, "prod-irsa")
		}
	})

	t.Run("the same token lists the fleet", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, api+"?labelSelector=env%3Dprd", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+iss.sign(t, validClaims(iss)))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s = %v, want nil", api, err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		var payload struct {
			Clusters []struct {
				Name string `json:"name"`
			} `json:"clusters"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if len(payload.Clusters) != 1 || payload.Clusters[0].Name != "eks-prod-euw1" {
			t.Errorf("clusters = %+v, want the one registered cell", payload.Clusters)
		}
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run() = %v, want nil on a clean shutdown", err)
	}
}

func postCluster(t *testing.T, url, token, body string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s = %v, want nil", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// freePort reserves a loopback port and releases it, so the server under test
// can bind it without a hard-coded port colliding with anything.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

func waitForReady(t *testing.T, probeAddr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/readyz", probeAddr))
		if err == nil {
			closeErr := resp.Body.Close()
			if resp.StatusCode == http.StatusOK && closeErr == nil {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become ready within 5s")
}

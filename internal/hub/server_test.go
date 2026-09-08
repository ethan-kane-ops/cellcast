package hub

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

func testServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Prepended, so a test that wants to read the trail can override it. The
	// default auditor writes to stdout, which is right for a deployed hub and
	// wrong for a test suite.
	opts = append([]Option{WithAuditor(audit.New(log, nil))}, opts...)
	srv, err := NewServer(DefaultConfig(), log, opts...)
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}
	return srv
}

func TestNewServerRejectsInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = ""

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewServer(cfg, log); err == nil {
		t.Fatal("NewServer() = nil error, want an error for empty addr")
	}
}

func TestProbeEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		ready    bool
		wantCode int
	}{
		{name: "healthz is up before readiness", path: "/healthz", ready: false, wantCode: http.StatusOK},
		{name: "readyz is unavailable before ready", path: "/readyz", ready: false, wantCode: http.StatusServiceUnavailable},
		{name: "readyz is ok once ready", path: "/readyz", ready: true, wantCode: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t)
			srv.SetReady(tt.ready)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			srv.probeHandler().ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("GET %s = %d, want %d", tt.path, rec.Code, tt.wantCode)
			}
		})
	}
}

// TestAPIDeniesByDefault pins the security property that no intermediate state
// of this repository serves an unauthenticated API. A server built without an
// authenticator must reject every caller rather than treat them as anonymous.
func TestAPIDeniesByDefault(t *testing.T) {
	srv := testServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	srv.apiHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/version = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// stubAuthenticator stands in for the OIDC implementation landing in ENG-172.
type stubAuthenticator struct{ id *identity.Identity }

func (s stubAuthenticator) Authenticate(context.Context, *http.Request) (*identity.Identity, error) {
	if s.id == nil {
		return nil, identity.ErrUnauthenticated
	}
	return s.id, nil
}

func TestAPIServesAuthenticatedCaller(t *testing.T) {
	want := &identity.Identity{Issuer: "https://token.actions.githubusercontent.com", Subject: "repo:example/app"}
	srv := testServer(t, WithAuthenticator(stubAuthenticator{id: want}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	srv.apiHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/version = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id header is empty, want a correlation id on every response")
	}
}

func TestIdentityRoundTripsThroughContext(t *testing.T) {
	want := &identity.Identity{Issuer: "https://example.test", Subject: "sub", Claims: map[string]string{"repository": "example/app"}}

	var got *identity.Identity
	var ok bool
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = IdentityFrom(r.Context())
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticate(stubAuthenticator{id: want}, log)(handler).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !ok {
		t.Fatal("IdentityFrom() reported no identity, want the authenticated caller")
	}
	if got.Subject != want.Subject || got.Claims["repository"] != want.Claims["repository"] {
		t.Fatalf("IdentityFrom() = %+v, want %+v", got, want)
	}
}

// TestRecoverPanicKeepsServing asserts that one bad request cannot take the
// process down. The hub is in the deploy critical path for a whole estate.
func TestRecoverPanicKeepsServing(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	chain(panicking, requestID, recoverPanic(log)).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("response body leaked the panic value: %s", rec.Body.String())
	}
}

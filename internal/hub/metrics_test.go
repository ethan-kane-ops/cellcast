package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/metrics"
	"github.com/ethan-kane-ops/cellcast/internal/hub/oidc"
)

// meteredServer is auditedServer with the Prometheus view attached too, so a
// request exercises both consumers of one audit record.
func meteredServer(t *testing.T, placer Placer, minter Minter, cells ...client.Object) (http.Handler, *metrics.Metrics, *prometheus.Registry) {
	t.Helper()

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cells...).Build()

	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		t.Fatalf("metrics.New() = %v, want nil", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := []Option{
		WithClusterClient(k8s),
		WithAuthenticator(fixedIdentity{}),
		WithMetrics(m),
		// The auditor is built explicitly here because supplying one bypasses
		// the default, and the default is what wires the metrics view.
		WithAuditor(audit.New(log, m)),
	}
	if placer != nil {
		opts = append(opts, WithPlacer(placer))
	}
	if minter != nil {
		opts = append(opts, WithMinter(minter))
	}
	return testServer(t, opts...).apiHandler(), m, reg
}

// TestAPlacementIsCountedAndTimed walks one real request through the handler
// and checks both metrics views of it.
func TestAPlacementIsCountedAndTimed(t *testing.T) {
	h, _, reg := meteredServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))

	if rec := post(t, h, `{"workload":"checkout-api","ttl":"12m"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}

	if got := testutil.CollectAndCount(reg, "cellcast_placement_requests_total"); got != 1 {
		t.Errorf("placement series = %d, want 1", got)
	}
	if got := testutil.CollectAndCount(reg, "cellcast_tokens_minted_total"); got != 1 {
		t.Errorf("mint series = %d, want 1", got)
	}
	// The duration is labelled by the request's final outcome, which for a full
	// mint is the mint's, not the placement's.
	if got := testutil.CollectAndCount(reg, "cellcast_placement_duration_seconds"); got != 1 {
		t.Fatalf("duration series = %d, want 1", got)
	}
}

// TestARefusedRequestIsStillTimed pins the half a latency panel usually misses.
// A hub refusing every request in three milliseconds looks fast, and a p99 that
// only counts successes will not show the outage.
func TestARefusedRequestIsStillTimed(t *testing.T) {
	h, _, reg := meteredServer(t, nil, nil)

	if rec := post(t, h, `{"workload":"checkout-api"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST = %d, want 503", rec.Code)
	}

	want := `
# HELP cellcast_placement_requests_total Placement decisions, by outcome, refusal reason, governing policy and chosen cell.
# TYPE cellcast_placement_requests_total counter
cellcast_placement_requests_total{cell="",outcome="refused",policy="",reason="PlacementUnavailable"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "cellcast_placement_requests_total"); err != nil {
		t.Error(err)
	}
	if got := testutil.CollectAndCount(reg, "cellcast_placement_duration_seconds"); got != 1 {
		t.Error("a refused request was not timed")
	}
}

// TestAuthRejectionsAreClassified is why the oidc errors carry a label rather
// than being counted by their message. The label set has to stay bounded, and
// the buckets have to separate a broken pipeline from somebody probing.
func TestAuthRejectionsAreClassified(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "an expired token is a clock or a retry loop", err: oidc.ErrExpired, want: "Expired"},
		{name: "an unknown issuer is not", err: oidc.ErrIssuerNotAllowed, want: "IssuerNotAllowed"},
		{
			name: "the reason survives being wrapped for the caller",
			err:  fmt.Errorf("%w: %w", identity.ErrUnauthenticated, oidc.ErrSignature),
			want: "Signature",
		},
		{
			name: "a hub with no authenticator is its own bucket",
			err:  nil, // denyAll's error, produced by the default server
			want: "NoAuthenticator",
		},
		{
			name: "anything unrecognised is visible rather than dropped",
			err:  errors.New("something nobody classified"),
			want: "Unclassified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m, err := metrics.New(reg)
			if err != nil {
				t.Fatalf("metrics.New() = %v, want nil", err)
			}

			var authn Authenticator = failingAuthenticator{err: tt.err}
			if tt.err == nil {
				authn = denyAll{}
			}

			srv := testServer(t, WithAuthenticator(authn), WithMetrics(m))
			rec := httptest.NewRecorder()
			srv.apiHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}

			want := fmt.Sprintf(`
# HELP cellcast_auth_rejections_total Requests rejected before reaching a handler, by why the token was refused.
# TYPE cellcast_auth_rejections_total counter
cellcast_auth_rejections_total{reason=%q} 1
`, tt.want)
			if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "cellcast_auth_rejections_total"); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestTheCallerLearnsNothingFromTheClassification keeps the precise reason on
// the operator's side of the boundary. Telling a caller that its issuer is not
// allowlisted, rather than simply that it is unauthenticated, is a probing oracle.
func TestTheCallerLearnsNothingFromTheClassification(t *testing.T) {
	srv := testServer(t, WithAuthenticator(failingAuthenticator{err: oidc.ErrIssuerNotAllowed}))

	rec := httptest.NewRecorder()
	srv.apiHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if body := rec.Body.String(); !strings.Contains(body, "unauthenticated") || strings.Contains(body, "issuer") {
		t.Errorf("response body = %s, want only the coarse refusal", body)
	}
}

type failingAuthenticator struct{ err error }

func (f failingAuthenticator) Authenticate(context.Context, *http.Request) (*identity.Identity, error) {
	return nil, f.err
}

package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// callerToken is the bearer token a traced request presents. A sentinel like
// mintedToken, and for the same reason: it must turn up on no span.
const callerToken = "SENTINEL-CALLER-TOKEN-7d1e0b"

// callerTrace is the trace a pipeline is already in when it asks.
var callerTrace = trace.NewSpanContext(trace.SpanContextConfig{
	TraceID:    trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
	SpanID:     trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
	TraceFlags: trace.FlagsSampled,
	Remote:     true,
})

// bearerIdentity authenticates any caller presenting a bearer token as the
// pipeline fixedIdentity names. Unlike fixedIdentity it insists on the header,
// so the token is in every request the hub traces.
type bearerIdentity struct{}

func (bearerIdentity) Authenticate(ctx context.Context, r *http.Request) (*identity.Identity, error) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return nil, errors.New("no bearer token")
	}
	return fixedIdentity{}.Authenticate(ctx, r)
}

// tracedServer builds a placement server whose spans land in the returned
// recorder.
func tracedServer(t *testing.T, minter Minter, opts ...Option) (http.Handler, *tracetest.SpanRecorder) {
	t.Helper()

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(registeredCell("prod-euw1")).Build()

	sr := tracetest.NewSpanRecorder()
	opts = append([]Option{
		WithClusterClient(k8s),
		WithAuthenticator(bearerIdentity{}),
		WithPlacer(&stubPlacer{decision: testDecision()}),
		WithMinter(minter),
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))),
	}, opts...)
	return testServer(t, opts...).apiHandler(), sr
}

// traceparent is callerTrace as a pipeline would send it.
func traceparent() string {
	return "00-" + callerTrace.TraceID().String() + "-" + callerTrace.SpanID().String() + "-01"
}

// placeAs sends one placement from inside callerTrace.
func placeAs(t *testing.T, h http.Handler, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/placement", strings.NewReader(`{"workload":"checkout-api"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	req.Header.Set("traceparent", traceparent())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// byName indexes the recorded spans. Every span one request produces is named
// for a different step, so a name recorded twice fails the test.
func byName(t *testing.T, sr *tracetest.SpanRecorder) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	out := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		if _, dup := out[s.Name()]; dup {
			t.Fatalf("two spans are named %q", s.Name())
		}
		out[s.Name()] = s
	}
	return out
}

func names(spans map[string]sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for name := range spans {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func attr(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

// spanText is everything a span would export, flattened for searching.
func spanText(s sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", s.Name(), s.Status().Description)
	for _, kv := range s.Attributes() {
		fmt.Fprintf(&b, " %s=%s", kv.Key, kv.Value.String())
	}
	for _, e := range s.Events() {
		fmt.Fprintf(&b, " %s", e.Name)
		for _, kv := range e.Attributes {
			fmt.Fprintf(&b, " %s=%s", kv.Key, kv.Value.String())
		}
	}
	for _, l := range s.Links() {
		for _, kv := range l.Attributes {
			fmt.Fprintf(&b, " %s=%s", kv.Key, kv.Value.String())
		}
	}
	return b.String()
}

func TestAPlacementIsOneTraceUnderTheCaller(t *testing.T) {
	// "Why did that placement take 800ms" has an answer only if the pipeline's
	// own trace continues into the hub and each step hangs off it.
	h, sr := tracedServer(t, &stubMinter{})
	if rec := placeAs(t, h, "Bearer "+callerToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body)
	}

	spans := byName(t, sr)
	server, ok := spans["POST /api/v1/placement"]
	if !ok {
		t.Fatalf("no span is named for the route; recorded %v", names(spans))
	}
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("the request span is a %s span, want server", server.SpanKind())
	}
	if server.SpanContext().TraceID() != callerTrace.TraceID() || server.Parent().SpanID() != callerTrace.SpanID() {
		t.Errorf("the request span is not a child of the caller's: trace %s, parent %s",
			server.SpanContext().TraceID(), server.Parent().SpanID())
	}
	for _, step := range []string{"authenticate", "place", "mint"} {
		s, ok := spans[step]
		if !ok {
			t.Errorf("no %q span; recorded %v", step, names(spans))
			continue
		}
		if s.Parent().SpanID() != server.SpanContext().SpanID() {
			t.Errorf("the %q span is not a child of the request span", step)
		}
	}
	if got, _ := attr(server, "http.route"); got != "/api/v1/placement" {
		t.Errorf("http.route = %q, want the route pattern", got)
	}
	if got, _ := attr(server, "http.response.status_code"); got != "200" {
		t.Errorf("http.response.status_code = %q, want 200", got)
	}
}

func TestSpansCarryTheAuditRecordAndNoTokenMaterial(t *testing.T) {
	// The request carried a caller token and the response carried a minted
	// one. Neither may reach a span, and nothing that is on a span may have
	// come from anywhere but the audit record's traced fields.
	h, sr := tracedServer(t, &stubMinter{})
	if rec := placeAs(t, h, "Bearer "+callerToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body)
	}
	spans := byName(t, sr)

	for span, want := range map[string]map[string]string{
		"POST /api/v1/placement": {
			"cellcast.event":    "placement",
			"cellcast.outcome":  "granted",
			"cellcast.cell":     "prod-euw1",
			"cellcast.policy":   "app-prod",
			"cellcast.workload": "checkout-api",
			"cellcast.subject":  "repo:acme/app:ref:refs/heads/main",
		},
		"mint": {
			"cellcast.event":           "mint",
			"cellcast.outcome":         "granted",
			"cellcast.namespace":       "apps",
			"cellcast.service_account": "deployer",
		},
	} {
		for key, value := range want {
			if got, _ := attr(spans[span], key); got != value {
				t.Errorf("%s: %s = %q, want %q", span, key, got, value)
			}
		}
	}

	for _, s := range sr.Ended() {
		text := spanText(s)
		for _, secret := range []string{mintedToken, callerToken} {
			if strings.Contains(text, secret) {
				t.Errorf("the %q span carries token material:\n%s", s.Name(), text)
			}
		}
		for _, key := range []string{"cellcast.token_sha256", "cellcast.claims", "cellcast.candidates", "cellcast.error"} {
			if _, ok := attr(s, key); ok {
				t.Errorf("the %q span carries %s, which the audit classification keeps off spans", s.Name(), key)
			}
		}
	}
}

func TestAFailedMintMarksTheMintSpan(t *testing.T) {
	h, sr := tracedServer(t, &stubMinter{err: broker.ErrTrustConfigMissing})
	if rec := placeAs(t, h, "Bearer "+callerToken); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503\n%s", rec.Code, rec.Body)
	}
	spans := byName(t, sr)

	if s := spans["mint"].Status(); s.Code != codes.Error || s.Description != string(refusal.MintFailed) {
		t.Errorf("mint span status = %v %q, want an error naming the refusal", s.Code, s.Description)
	}
	if s := spans["POST /api/v1/placement"].Status(); s.Code != codes.Error {
		t.Errorf("a 503 left the request span %v, want an error", s.Code)
	}
}

func TestAnUnauthenticatedRequestIsTracedWithoutItsCredential(t *testing.T) {
	h, sr := tracedServer(t, &stubMinter{})
	if rec := placeAs(t, h, "Token "+callerToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401\n%s", rec.Code, rec.Body)
	}
	spans := byName(t, sr)

	auth, ok := spans["authenticate"]
	if !ok {
		t.Fatalf("no authenticate span; recorded %v", names(spans))
	}
	if s := auth.Status(); s.Code != codes.Error || s.Description != "Unclassified" {
		t.Errorf("authenticate span status = %v %q, want an error naming the classified rejection", s.Code, s.Description)
	}
	// Rejected before routing, so the request span keeps the method's name.
	if s := spans["POST"].Status(); s.Code == codes.Error {
		t.Error("a 401 was recorded as a server error; a rejected caller is the hub working")
	}
	for _, s := range sr.Ended() {
		if text := spanText(s); strings.Contains(text, callerToken) {
			t.Errorf("the %q span carries the rejected credential:\n%s", s.Name(), text)
		}
	}
}

func TestTheRequestLogNamesTheTrace(t *testing.T) {
	var logged bytes.Buffer
	srv, err := NewServer(DefaultConfig(), slog.New(slog.NewJSONHandler(&logged, nil)),
		WithAuditor(audit.New(slog.New(slog.NewTextHandler(io.Discard, nil)))),
		WithAuthenticator(bearerIdentity{}),
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tracetest.NewSpanRecorder()))),
	)
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+callerToken)
	req.Header.Set("traceparent", traceparent())
	srv.apiHandler().ServeHTTP(httptest.NewRecorder(), req)

	if want := `"trace_id":"` + callerTrace.TraceID().String() + `"`; !strings.Contains(logged.String(), want) {
		t.Errorf("the request log line does not name the trace; want %s in\n%s", want, logged.String())
	}
}

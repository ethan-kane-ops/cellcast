package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestTheMintCarriesItsTraceToTheSpoke(t *testing.T) {
	// The mint is the one step of a placement that leaves the process, and a
	// spoke API server with tracing enabled continues the trace only if the
	// hub sends it.
	c := newConnector(t, &rest.Config{Host: "https://hub.example.test"})
	var got *rest.Config
	c.build = func(cfg *rest.Config) (kubernetes.Interface, error) {
		got = cfg
		return fake.NewSimpleClientset(), nil
	}
	if _, err := c.Connect(t.Context(), testCluster("hub-cell", nil), testTrust("cell-trust")); err != nil {
		t.Fatalf("Connect() = %v, want nil", err)
	}
	if got.WrapTransport == nil {
		t.Fatal("the spoke client's transport is not wrapped, so no trace context can reach the spoke")
	}

	var sent http.Header
	rt := got.WrapTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	}))

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(t.Context(), "mint")
	defer span.End()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"https://spoke.example.test/api/v1/namespaces/apps/serviceaccounts/deployer/token", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() = %v, want nil", err)
	}
	sc := span.SpanContext()
	if want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"; sent.Get("traceparent") != want {
		t.Errorf("traceparent = %q, want %q", sent.Get("traceparent"), want)
	}
	if req.Header.Get("traceparent") != "" {
		t.Error("the request handed to the transport was modified; a RoundTripper must work on a copy")
	}

	// With tracing off there is no span, and the spoke is sent nothing new.
	bare := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://spoke.example.test/", nil)
	if _, err := rt.RoundTrip(bare); err != nil {
		t.Fatalf("RoundTrip() = %v, want nil", err)
	}
	if sent.Get("traceparent") != "" {
		t.Errorf("a request with no span carried traceparent %q", sent.Get("traceparent"))
	}
}

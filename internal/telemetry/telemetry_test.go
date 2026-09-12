package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promb "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// otelEnv is every variable that decides whether and where this package, or
// the exporters under it, send anything.
var otelEnv = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_SDK_DISABLED",
	"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
}

// cleanEnv isolates a test from whatever the shell running it exports.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range otelEnv {
		t.Setenv(k, "")
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		envProt string
		wantErr string
	}{
		{name: "nothing configured", cfg: DefaultConfig()},
		{name: "a collector over http", cfg: Config{Endpoint: "http://collector:4318", SampleRatio: 1}},
		{name: "a collector over grpc with tls", cfg: Config{Endpoint: "https://collector:4317", Protocol: ProtocolGRPC, SampleRatio: 0.1}},
		{name: "no scheme", cfg: Config{Endpoint: "collector:4318", SampleRatio: 1}, wantErr: "http:// or https://"},
		{name: "no host", cfg: Config{Endpoint: "http://", SampleRatio: 1}, wantErr: "must include a host"},
		{name: "credentials in the URL", cfg: Config{Endpoint: "https://user:secret@collector:4318", SampleRatio: 1}, wantErr: "must not embed credentials"},
		{name: "a protocol the exporters do not speak", cfg: Config{Protocol: "http/json", SampleRatio: 1}, wantErr: "otlp-protocol"},
		{name: "the same from the environment", cfg: DefaultConfig(), envProt: "thrift", wantErr: "otlp-protocol"},
		{name: "a ratio above one", cfg: Config{SampleRatio: 1.5}, wantErr: "trace-sample-ratio"},
		{name: "a negative ratio", cfg: Config{SampleRatio: -0.1}, wantErr: "trace-sample-ratio"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tt.envProt)

			err := tt.cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestExportFollowsTheFlagAndTheEnvironment(t *testing.T) {
	tests := []struct {
		name            string
		endpoint        string
		env             map[string]string
		traces, metrics bool
	}{
		{name: "nothing anywhere"},
		{name: "the flag", endpoint: "http://c:4318", traces: true, metrics: true},
		{name: "the shared variable", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, traces: true, metrics: true},
		{name: "a traces-only variable", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://c:4318/v1/traces"}, traces: true},
		{name: "one signal switched off", endpoint: "http://c:4318", env: map[string]string{"OTEL_METRICS_EXPORTER": "none"}, traces: true},
		{name: "the SDK switched off", endpoint: "http://c:4318", env: map[string]string{"OTEL_SDK_DISABLED": "true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg := Config{Endpoint: tt.endpoint, SampleRatio: 1}
			if got := cfg.exports("TRACES"); got != tt.traces {
				t.Errorf("traces exported = %v, want %v", got, tt.traces)
			}
			if got := cfg.exports("METRICS"); got != tt.metrics {
				t.Errorf("metrics exported = %v, want %v", got, tt.metrics)
			}
		})
	}
}

func TestAnEndpointIsABaseURL(t *testing.T) {
	// The flag means what OTEL_EXPORTER_OTLP_ENDPOINT means. Posting to the
	// collector's root instead answers 404, which reads in the log exactly like
	// a collector that is down.
	for base, want := range map[string]string{
		"http://c:4318":        "http://c:4318/v1/traces",
		"http://c:4318/":       "http://c:4318/v1/traces",
		"https://gw.test/otlp": "https://gw.test/otlp/v1/traces",
	} {
		if got := signalURL(base, "traces"); got != want {
			t.Errorf("signalURL(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestNothingIsExportedUnlessSomethingSaysWhere(t *testing.T) {
	cleanEnv(t)
	tel, err := Start(t.Context(), DefaultConfig(), "cellcast-test", discard(), WithMetrics(promb.NewMetricProducer()))
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	_, span := tel.TracerProvider().Tracer("test").Start(t.Context(), "probe")
	defer span.End()

	if span.IsRecording() {
		t.Error("a span records with no endpoint anywhere; tracing that goes nowhere should cost nothing")
	}
	if len(tel.shutdown) != 0 {
		t.Errorf("Start built %d exporters with nowhere to send to", len(tel.shutdown))
	}
}

// collector is an OTLP/HTTP receiver that keeps what it was sent.
type collector struct {
	mu       sync.Mutex
	paths    []string
	headers  []http.Header
	spans    []string
	metrics  []string
	resource map[string]string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.URL.Path)
	c.headers = append(c.headers, r.Header.Clone())

	switch r.URL.Path {
	case "/v1/traces":
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, rs := range req.GetResourceSpans() {
			c.resource = map[string]string{}
			for _, kv := range rs.GetResource().GetAttributes() {
				c.resource[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					c.spans = append(c.spans, s.GetName())
				}
			}
		}
	case "/v1/metrics":
		var req colmetricpb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					c.metrics = append(c.metrics, m.GetName())
				}
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func TestTracesAndMetricsReachACollectorOverHTTP(t *testing.T) {
	cleanEnv(t)
	// Headers arrive the way the charts deliver them: from a Secret into the
	// environment, never through a flag or a manifest.
	const apiKey = "key-from-a-secret"
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key="+apiKey)

	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "cellcast_probe_total", Help: "A counter the test increments."})
	reg.MustRegister(counter)
	counter.Inc()

	var logged bytes.Buffer
	tel, err := Start(t.Context(), Config{Endpoint: srv.URL, Protocol: ProtocolHTTP, SampleRatio: 1}, "cellcast-test",
		slog.New(slog.NewJSONHandler(&logged, nil)),
		WithMetrics(promb.NewMetricProducer(promb.WithGatherer(reg))),
		WithResource(attribute.String("cellcast.cell", "prod-euw1")),
	)
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	_, span := tel.TracerProvider().Tracer("test").Start(t.Context(), "probe")
	span.End()
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.spans, "probe") {
		t.Errorf("the collector received spans %v at %v, want the probe span", c.spans, c.paths)
	}
	// The name a scrape of the same registry reports, unchanged.
	if !slices.Contains(c.metrics, "cellcast_probe_total") {
		t.Errorf("the collector received metrics %v, want cellcast_probe_total", c.metrics)
	}
	if c.resource["service.name"] != "cellcast-test" || c.resource["cellcast.cell"] != "prod-euw1" {
		t.Errorf("resource = %v, want the service name and the cell", c.resource)
	}
	for i, h := range c.headers {
		if h.Get("x-api-key") != apiKey {
			t.Errorf("export %d to %s carried no header from OTEL_EXPORTER_OTLP_HEADERS", i, c.paths[i])
		}
	}
	if strings.Contains(logged.String(), apiKey) {
		t.Errorf("the log carries the OTLP headers:\n%s", logged.String())
	}
}

// traceServer is an OTLP/gRPC trace receiver that keeps span names.
type traceServer struct {
	coltracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	spans []string
}

func (s *traceServer) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				s.spans = append(s.spans, sp.GetName())
			}
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func TestTracesReachACollectorOverGRPC(t *testing.T) {
	cleanEnv(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	ts := &traceServer{}
	gs := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(gs, ts)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	// http:// is how the scheme says plaintext for gRPC too.
	tel, err := Start(t.Context(), Config{Endpoint: "http://" + lis.Addr().String(), Protocol: ProtocolGRPC, SampleRatio: 1},
		"cellcast-test", discard())
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	_, span := tel.TracerProvider().Tracer("test").Start(t.Context(), "probe")
	span.End()
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !slices.Contains(ts.spans, "probe") {
		t.Errorf("the gRPC collector received %v, want the probe span", ts.spans)
	}
}

func TestADecisionMadeUpstreamOutranksTheRatio(t *testing.T) {
	// A report the agent traced must stay traced in the hub, or the trace stops
	// at the network edge and "slow in the agent or slow in the hub" has no
	// answer. The ratio governs only traces that start here.
	cleanEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	tel, err := Start(t.Context(), Config{Endpoint: srv.URL, Protocol: ProtocolHTTP, SampleRatio: 0}, "cellcast-test", discard())
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	defer func() { _ = tel.Shutdown(t.Context()) }()
	tracer := tel.TracerProvider().Tracer("test")

	_, root := tracer.Start(t.Context(), "root")
	root.End()
	if root.SpanContext().IsSampled() {
		t.Error("a trace started here was kept at a ratio of 0")
	}

	upstream := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	_, child := tracer.Start(trace.ContextWithRemoteSpanContext(t.Context(), upstream), "child")
	child.End()
	if !child.SpanContext().IsSampled() {
		t.Error("a span under a sampled upstream trace was dropped by the local ratio")
	}
}

func TestAnUnreachableCollectorIsLoggedOnceAMinute(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1_700_000_000, 0)
	e := &errorLog{log: slog.New(slog.NewJSONHandler(&buf, nil)), now: func() time.Time { return now }}

	for range 5 {
		e.Handle(errors.New("connection refused"))
	}
	if n := strings.Count(buf.String(), "telemetry export failed"); n != 1 {
		t.Fatalf("five failures inside a minute logged %d lines, want 1", n)
	}

	now = now.Add(errorLogEvery)
	e.Handle(errors.New("connection refused"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"suppressed":4`) {
		t.Errorf("after a minute, want a second line counting the 4 held back; got\n%s", buf.String())
	}
}

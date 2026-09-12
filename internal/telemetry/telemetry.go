// Package telemetry exports traces and metrics over OTLP.
//
// Off unless something configures it. With no endpoint on the command line and
// none in the environment, the tracer provider handed back is a no-op, so a
// process nobody pointed at a collector pays for neither the exporter nor the
// spans.
//
// The hub and the agent use it. The client does not and must not
// (TestTheClientDoesNotCarryTelemetry): a pipeline step has no collector to
// report to, and a binary downloaded on every run has no business carrying one.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ethan-kane-ops/cellcast/internal/version"
)

// Protocols the exporters speak, spelled as OTEL_EXPORTER_OTLP_PROTOCOL spells
// them so that the flag and the environment variable take the same values.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// flushTimeout bounds the export of whatever is still buffered at shutdown.
//
// Short on purpose. It runs after the hub's drain, inside the same termination
// grace period, and a SIGKILL that lands during it costs a few spans, where one
// that lands during the drain costs deploys.
const flushTimeout = 5 * time.Second

// errorLogEvery is the most often an export failure is logged.
const errorLogEvery = time.Minute

// Config says where telemetry goes.
type Config struct {
	// Endpoint is the collector's base URL, for example
	// http://otel-collector:4318. The scheme decides TLS. Empty defers to the
	// OTEL_EXPORTER_OTLP_* environment variables, and with none of those set
	// nothing is exported.
	Endpoint string
	// Protocol is grpc or http/protobuf. Empty defers to
	// OTEL_EXPORTER_OTLP_PROTOCOL, then to http/protobuf, which is the default
	// the OpenTelemetry specification sets for SDKs.
	Protocol string
	// SampleRatio is the share of traces that start in this process and are
	// kept. A trace that arrives with a sampling decision already made keeps
	// that decision, so a trace is never recorded in one process and dropped
	// in the next.
	SampleRatio float64
}

// DefaultConfig keeps every trace. The hub answers a request per deploy and the
// agent reports twice a minute, which is not a volume that needs sampling.
func DefaultConfig() Config {
	return Config{SampleRatio: 1}
}

// AddFlags registers the configuration on fs, under the same names in every
// binary that has it.
func (c *Config) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&c.Endpoint, "otlp-endpoint", c.Endpoint,
		"OTLP collector base URL, for example http://otel-collector:4318; the scheme decides TLS (default: the OTEL_EXPORTER_OTLP_* environment, else nothing is exported)")
	fs.StringVar(&c.Protocol, "otlp-protocol", c.Protocol,
		"OTLP protocol, grpc or http/protobuf (default: OTEL_EXPORTER_OTLP_PROTOCOL, else http/protobuf)")
	fs.Float64Var(&c.SampleRatio, "trace-sample-ratio", c.SampleRatio,
		"share of the traces started here that are kept, from 0 to 1")
}

// Validate reports whether the configuration is usable. It checks the protocol
// the environment supplies as well as the flag's, so a typo in either stops the
// process at startup rather than silently exporting nothing.
func (c Config) Validate() error {
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return fmt.Errorf("parsing otlp-endpoint: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("otlp-endpoint %q must be an http:// or https:// URL", c.Endpoint)
		}
		if u.Host == "" {
			return fmt.Errorf("otlp-endpoint %q must include a host", c.Endpoint)
		}
		if u.User != nil {
			// Refused rather than stripped. The exporters render the endpoint
			// into every failure they report, and those reach the log
			// (docs/threat-model.md T-05). A backend's credentials belong in
			// OTEL_EXPORTER_OTLP_HEADERS, which the charts fill from a Secret.
			return errors.New("otlp-endpoint must not embed credentials in the URL; put them in OTEL_EXPORTER_OTLP_HEADERS")
		}
	}
	if p := c.protocol(); p != ProtocolGRPC && p != ProtocolHTTP {
		return fmt.Errorf("otlp-protocol must be %s or %s, got %q", ProtocolGRPC, ProtocolHTTP, p)
	}
	if math.IsNaN(c.SampleRatio) || c.SampleRatio < 0 || c.SampleRatio > 1 {
		return fmt.Errorf("trace-sample-ratio must be between 0 and 1, got %v", c.SampleRatio)
	}
	return nil
}

// protocol is the protocol in force: the flag, then the environment, then the
// specification's default.
func (c Config) protocol() string {
	if c.Protocol != "" {
		return c.Protocol
	}
	if p := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"); p != "" {
		return p
	}
	return ProtocolHTTP
}

// exports reports whether a signal has somewhere to go. signal is TRACES or
// METRICS, as the per-signal environment variables spell it.
//
// OTEL_SDK_DISABLED and OTEL_<signal>_EXPORTER=none are honoured because they
// are how a collector-managed estate turns one signal off without knowing
// anything about cellcast: a Datadog agent that already scrapes the metrics
// endpoint wants the traces and not a second copy of the metrics.
func (c Config) exports(signal string) bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	if os.Getenv("OTEL_"+signal+"_EXPORTER") == "none" {
		return false
	}
	return c.Endpoint != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != ""
}

// Telemetry holds what a process reports through.
type Telemetry struct {
	tracerProvider trace.TracerProvider
	shutdown       []func(context.Context) error
}

// TracerProvider is the provider every span in the process starts from. A no-op
// when traces go nowhere.
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tracerProvider }

// Shutdown exports whatever is still buffered and stops, within flushTimeout.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	var errs []error
	for _, fn := range t.shutdown {
		errs = append(errs, fn(ctx))
	}
	return errors.Join(errs...)
}

// Option adds to what Start sets up.
type Option func(*options)

type options struct {
	producer sdkmetric.Producer
	attrs    []attribute.KeyValue
}

// WithMetrics exports what p yields on the OTLP metrics signal.
//
// A producer rather than a set of instruments. The hub's metrics already live
// in a Prometheus registry, and exporting that registry is what keeps a scrape
// and an OTLP push one surface with one set of names (docs/metrics.md).
func WithMetrics(p sdkmetric.Producer) Option {
	return func(o *options) { o.producer = p }
}

// WithResource describes this process on everything it exports, for example
// which cell an agent reports for.
func WithResource(attrs ...attribute.KeyValue) Option {
	return func(o *options) { o.attrs = append(o.attrs, attrs...) }
}

// Start builds the providers for service and starts exporting.
//
// An error means the configuration could not become an exporter. It never
// means the collector is unreachable: the exporters connect lazily, and a
// collector that is down is logged and retried rather than stopping the
// process. A hub in the deploy critical path must not refuse to start because
// its tracing backend is having a bad day.
func Start(ctx context.Context, cfg Config, service string, log *slog.Logger, opts ...Option) (*Telemetry, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	t := &Telemetry{tracerProvider: noop.NewTracerProvider()}
	traces := cfg.exports("TRACES")
	metrics := o.producer != nil && cfg.exports("METRICS")
	if !traces && !metrics {
		return t, nil
	}

	// The SDK's default handler is the standard library logger, which writes
	// plain text into the middle of a JSON log stream.
	otel.SetErrorHandler(&errorLog{log: log, now: time.Now})

	res, err := resource.New(ctx,
		resource.WithAttributes(append([]attribute.KeyValue{
			semconv.ServiceName(service),
			semconv.ServiceVersion(version.Get().Version),
		}, o.attrs...)...),
		// After the defaults, so that OTEL_SERVICE_NAME and
		// OTEL_RESOURCE_ATTRIBUTES override them. A Datadog estate tags by
		// service, env and version, and those names are the operator's.
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		if !errors.Is(err, resource.ErrPartialResource) {
			return nil, fmt.Errorf("building the telemetry resource: %w", err)
		}
		// A malformed OTEL_RESOURCE_ATTRIBUTES loses its own attributes and
		// nothing else; not a reason to export nothing.
		log.Warn("some telemetry resource attributes were not usable", slog.Any("error", err))
	}

	if traces {
		exp, err := traceExporter(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("building the OTLP trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		)
		t.tracerProvider = tp
		t.shutdown = append(t.shutdown, tp.Shutdown)
	}

	if metrics {
		exp, err := metricExporter(ctx, cfg)
		if err != nil {
			_ = t.Shutdown(ctx)
			return nil, fmt.Errorf("building the OTLP metric exporter: %w", err)
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithProducer(o.producer))),
		)
		t.shutdown = append(t.shutdown, mp.Shutdown)
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "from the environment"
	}
	log.Info("otlp export enabled",
		slog.Bool("traces", traces),
		slog.Bool("metrics", metrics),
		slog.String("protocol", cfg.protocol()),
		slog.String("endpoint", endpoint),
		slog.Float64("trace_sample_ratio", cfg.SampleRatio),
	)
	return t, nil
}

func traceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	if cfg.protocol() == ProtocolGRPC {
		var opts []otlptracegrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(signalURL(cfg.Endpoint, "traces")))
	}
	return otlptracehttp.New(ctx, opts...)
}

func metricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	if cfg.protocol() == ProtocolGRPC {
		var opts []otlpmetricgrpc.Option
		if cfg.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	var opts []otlpmetrichttp.Option
	if cfg.Endpoint != "" {
		opts = append(opts, otlpmetrichttp.WithEndpointURL(signalURL(cfg.Endpoint, "metrics")))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// signalURL is where one signal is posted over HTTP.
//
// The endpoint is a base URL, as OTEL_EXPORTER_OTLP_ENDPOINT is, and each
// signal's path goes on the end of it. Handing the exporter the base URL as it
// stands posts to the collector's root, which answers 404 and is
// indistinguishable in the log from a collector that is down.
func signalURL(base, signal string) string {
	u, err := url.Parse(base)
	if err != nil {
		// Unreachable after Validate; the exporter reports the bad URL itself.
		return base
	}
	return u.JoinPath("v1", signal).String()
}

// errorLog reports export failures without letting a dead collector fill the
// log.
//
// The span processor fails once per batch and the metric reader once per
// interval, so a collector that is down produces an error every few seconds for
// as long as it stays down. One line a minute with a count of what was held
// back says the same thing.
type errorLog struct {
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last time.Time
	held int
}

// Handle implements otel.ErrorHandler.
func (e *errorLog) Handle(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	if !e.last.IsZero() && now.Sub(e.last) < errorLogEvery {
		e.held++
		return
	}
	e.log.Warn("telemetry export failed", slog.Any("error", err), slog.Int("suppressed", e.held))
	e.last, e.held = now, 0
}

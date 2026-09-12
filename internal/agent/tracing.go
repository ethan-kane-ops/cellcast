package agent

import (
	"context"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// This file is the one place in the agent that decides what a span carries,
// for the same reason as the hub's tracing.go: every request the agent makes
// carries its projected token (docs/threat-model.md T-05).

// tracerName is the instrumentation scope of the spans the agent starts.
const tracerName = "github.com/ethan-kane-ops/cellcast/internal/agent"

// reportSpanName names the report after the hub route it calls, so that the
// agent's client span and the hub's server span read as the two ends of one
// call.
const reportSpanName = "POST /api/v1/clusters/{name}/capacity"

// startReport starts the client span for one heartbeat.
func startReport(ctx context.Context, tp trace.TracerProvider) (context.Context, trace.Span) {
	return tp.Tracer(tracerName).Start(ctx, reportSpanName, trace.WithSpanKind(trace.SpanKindClient))
}

// injectTrace writes the report span's context onto the request, so that the
// hub's span for the report joins the same trace. The W3C traceparent and
// tracestate headers and nothing else.
func injectTrace(ctx context.Context, h http.Header) {
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(h))
}

// endReport records how a report went and ends its span: an error status when
// it failed, and the hub's status code when the hub answered with a refusal.
// Nothing from the request or from either body, and never the error's text.
func endReport(span trace.Span, err error) {
	var refused *HubError
	if errors.As(err, &refused) {
		span.SetAttributes(semconv.HTTPResponseStatusCode(refused.Status))
	}
	if err != nil {
		span.SetStatus(codes.Error, "report failed")
	}
	span.End()
}

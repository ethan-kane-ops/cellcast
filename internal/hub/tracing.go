package hub

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// This file is the one place in the hub that decides what a span carries.
//
// Spans are timing. What is on them is either one of the facts that make an
// HTTP server span readable (method, route, status code, request id) or a field
// of the audit record, rendered by audit.Record.TraceAttrs from the same
// classification that keeps token material out of the trail
// (docs/threat-model.md T-05). No other file in the hub sets an attribute, a
// status or an event on a span, and TestOnlyTracingFilesDescribeSpans holds
// every file to that.

// tracerName is the instrumentation scope of the spans the hub starts.
const tracerName = "github.com/ethan-kane-ops/cellcast/internal/hub"

// traceContext is the only propagation format the hub reads. W3C baggage is
// deliberately not read: it is arbitrary caller-supplied data, and whatever the
// hub accepted it would pass on to every cell it minted in.
var traceContext = propagation.TraceContext{}

// traceRequests starts the server span for an API request, as a child of the
// caller's span when the request carries a traceparent header.
//
// Named after the method alone to begin with, because the route is not known
// until the mux has matched it. nameSpanByRoute renames it once it is.
func traceRequests(tp trace.TracerProvider) middleware {
	tracer := tp.Tracer(tracerName)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method := spanMethod(r.Method)
			ctx := traceContext.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := tracer.Start(ctx, method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					semconv.HTTPRequestMethodKey.String(method),
					attribute.String("cellcast.request_id", requestIDFrom(ctx)),
				),
			)
			defer span.End()

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			span.SetAttributes(semconv.HTTPResponseStatusCode(rec.status))
			if rec.status >= http.StatusInternalServerError {
				// A 5xx only. A refusal is the hub doing its job: a 403 for a
				// caller no policy permits is the correct answer, and marking it
				// an error would bury the real failures under expected ones.
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		})
	}
}

// spanMethod is the request method as a span may carry it: one the API could
// serve, or _OTHER. The method is whatever the caller sent, and a span name has
// to come from a small fixed set.
func spanMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "_OTHER"
}

// nameSpanByRoute renames the server span after the route that matched, for
// example "POST /api/v1/placement".
//
// It has to wrap the mux directly. ServeMux records the pattern it matched on
// the request it was handed, and each middleware above passes a copy down, so
// this is the only layer that can see it. The pattern rather than the path,
// because a path carries a cell name and a span name has to be the same for
// every call to one route.
func nameSpanByRoute(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if r.Pattern == "" {
			return
		}
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Pattern)
		if _, route, ok := strings.Cut(r.Pattern, " "); ok {
			span.SetAttributes(semconv.HTTPRoute(route))
		}
	})
}

// tracedAuthenticator times authentication as a span of its own. Issuer
// discovery and key fetches leave the process, so this is the one step besides
// the mint that can be slow for reasons the hub does not control.
type tracedAuthenticator struct{ Authenticator }

// Authenticate implements Authenticator.
func (a tracedAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*identity.Identity, error) {
	ctx, span := startSpan(ctx, "authenticate")
	defer span.End()

	id, err := a.Authenticator.Authenticate(ctx, r)
	if err != nil {
		// The classified reason and never err.Error(). An issuer's message is
		// unbounded, and a parse failure is the one place a message could quote
		// the token it failed on.
		span.SetStatus(codes.Error, rejectionReason(err))
	}
	return id, err
}

// startSpan starts a child of the span in ctx, from that span's own provider.
//
// Taking the provider from the parent rather than from a global is what lets a
// test hand the server one provider and see every span a request produced. It
// is also what makes tracing free when it is off: with no span in ctx, the
// no-op span's provider starts no-op spans.
func startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(tracerName).Start(ctx, name)
}

// spanNotifier puts each audit record on the span it happened in: the placement
// record on the request's server span, the mint record on the mint span. It
// satisfies audit.Notifier.
type spanNotifier struct{}

// Notify implements audit.Notifier.
func (spanNotifier) Notify(ctx context.Context, rec audit.Record) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(spanAttributes(rec)...)
	if rec.Event == audit.EventMint && rec.Outcome == audit.OutcomeRefused {
		span.SetStatus(codes.Error, string(rec.Reason))
	}
}

// spanAttributes renders a record's traced fields under the cellcast namespace,
// keyed as the audit trail keys them, so that a span and a trail line can be
// read side by side.
func spanAttributes(rec audit.Record) []attribute.KeyValue {
	attrs := rec.TraceAttrs()
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		key := attribute.Key("cellcast." + a.Key)
		switch a.Value.Kind() {
		case slog.KindBool:
			out = append(out, key.Bool(a.Value.Bool()))
		case slog.KindTime:
			out = append(out, key.String(a.Value.Time().UTC().Format(time.RFC3339)))
		default:
			out = append(out, key.String(a.Value.String()))
		}
	}
	return out
}

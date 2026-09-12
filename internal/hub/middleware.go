package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/ethan-kane-ops/cellcast/internal/hub/metrics"
)

// middleware wraps a handler.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first argument is the outermost wrapper.
func chain(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

type requestIDContextKey struct{}

// requestID assigns every request an identifier and echoes it back, so a
// pipeline failure can be correlated with a hub log line without a debug build.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8)
		// crypto/rand.Read never returns an error as of Go 1.24.
		_, _ = rand.Read(buf)
		id := hex.EncodeToString(buf)

		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestIDFrom returns the identifier assigned to this request, if any.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logging emits one structured line per request.
//
// It never logs request bodies, headers, or query strings. Caller tokens arrive
// in the Authorization header and minted tokens leave in response bodies; a
// well-meaning debug line is the most likely way this system leaks the thing it
// exists to protect (docs/threat-model.md T-05).
func logging(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			args := []any{
				slog.String("request_id", requestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
			}
			// The join from a log line to the trace that timed the request. An
			// unsampled trace was never exported, so its id leads nowhere.
			if sc := trace.SpanContextFromContext(r.Context()); sc.IsSampled() {
				args = append(args, slog.String("trace_id", sc.TraceID().String()))
			}
			log.InfoContext(r.Context(), "request", args...)
		})
	}
}

// recoverPanic keeps one bad request from taking the process down.
//
// The hub sits in the deploy critical path for an entire estate, so a panic
// serving one caller must not stop every other pipeline.
func recoverPanic(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.ErrorContext(r.Context(), "panic serving request",
						slog.String("request_id", requestIDFrom(r.Context())),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.Any("panic", v),
					)
					writeError(w, http.StatusInternalServerError, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// authRejection is implemented by an authenticator error that can name itself
// in one word.
//
// Declared here rather than imported so that the hub keeps exactly one seam to
// the authenticator and does not link a particular implementation in order to
// count its refusals. An error that does not implement it is counted as
// Unclassified, which is a visible bucket rather than a silent omission.
type authRejection interface {
	RejectionReason() string
}

// rejectionReason classifies an authentication failure for the metric label.
//
// The error string is not used: it is unbounded and would make the
// label cardinality a function of what an issuer put in a message.
func rejectionReason(err error) string {
	var reason authRejection
	if errors.As(err, &reason) {
		return reason.RejectionReason()
	}
	return "Unclassified"
}

// authenticate resolves the caller and attaches the identity to the request
// context. A request that cannot be authenticated is rejected here and never
// reaches a handler.
func authenticate(a Authenticator, log *slog.Logger, m *metrics.Metrics) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r.Context(), r)
			if err != nil {
				reason := rejectionReason(err)
				m.AuthRejected(reason)
				log.WarnContext(r.Context(), "authentication rejected",
					slog.String("request_id", requestIDFrom(r.Context())),
					slog.String("path", r.URL.Path),
					slog.String("rejection", reason),
					slog.String("reason", err.Error()),
				)
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
		})
	}
}

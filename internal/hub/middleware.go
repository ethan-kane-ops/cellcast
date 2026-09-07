package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
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

			log.InfoContext(r.Context(), "request",
				slog.String("request_id", requestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
			)
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

// authenticate resolves the caller and attaches the identity to the request
// context. A request that cannot be authenticated is rejected here and never
// reaches a handler.
func authenticate(a Authenticator, log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r.Context(), r)
			if err != nil {
				log.WarnContext(r.Context(), "authentication rejected",
					slog.String("request_id", requestIDFrom(r.Context())),
					slog.String("path", r.URL.Path),
					slog.String("reason", err.Error()),
				)
				writeError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
		})
	}
}

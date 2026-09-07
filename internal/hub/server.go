package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/version"
)

// Server is the hub's HTTP API.
type Server struct {
	cfg   Config
	log   *slog.Logger
	authn Authenticator

	// k8s is the registry. Reads are served from the manager's informer cache,
	// so listing the fleet on every placement costs no API server traffic.
	k8s client.Client

	// waitForSync blocks until the registry cache is usable. Nil means there is
	// nothing to wait for, which is the case in tests.
	waitForSync func(context.Context) bool

	// capacity is the in-memory utilisation index. Nil means the capacity
	// endpoints report 503 rather than panicking.
	capacity *capacity.Registry

	// ready gates the readiness probe. A replica that has not finished starting
	// must not accept traffic and answer placements it cannot score.
	ready atomic.Bool
}

// Option configures a Server.
type Option func(*Server)

// WithAuthenticator replaces the fail-closed default authenticator.
//
// ENG-172 supplies the OIDC implementation. Until then the default rejects
// every caller, which is the correct behaviour for a broker with no way to
// identify who is asking.
func WithAuthenticator(a Authenticator) Option {
	return func(s *Server) { s.authn = a }
}

// WithClusterClient supplies the Kubernetes client backing the registry.
//
// Without it the cluster endpoints report 503 rather than panicking, so a hub
// misconfigured with no cluster access fails visibly instead of at the first
// registration.
func WithClusterClient(c client.Client) Option {
	return func(s *Server) { s.k8s = c }
}

// WithCapacityRegistry supplies the in-memory capacity index.
//
// Separate from the cluster client because the two have opposite durability
// stories: the registry is etcd-backed and survives a restart, the capacity
// index is deliberately lost on one (docs/architecture.md ADR-002).
func WithCapacityRegistry(c *capacity.Registry) Option {
	return func(s *Server) { s.capacity = c }
}

// WithCacheSync defers readiness until the registry cache has synced.
//
// The listeners come up immediately either way, so probes answer from the first
// moment. Readiness is what gates traffic, and a replica that answered a list
// from an unsynced cache would report an empty fleet, which fails a placement
// that should have succeeded.
func WithCacheSync(fn func(context.Context) bool) Option {
	return func(s *Server) { s.waitForSync = fn }
}

// NewServer builds a hub API server.
func NewServer(cfg Config, log *slog.Logger, opts ...Option) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid hub config: %w", err)
	}
	s := &Server{cfg: cfg, log: log, authn: denyAll{}}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SetReady marks the server ready to serve traffic.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// apiHandler returns the authenticated API surface.
//
// Routes are registered by the tickets that own them: ENG-114 for placement.
// Everything mounted here sits behind the authentication middleware by
// construction, so a new route cannot accidentally be served anonymously.
func (s *Server) apiHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, version.Get())
	})

	mux.HandleFunc("POST /api/v1/clusters", s.handleRegisterCluster)
	mux.HandleFunc("GET /api/v1/clusters", s.handleListClusters)
	mux.HandleFunc("GET /api/v1/clusters/{name}", s.handleGetCluster)
	mux.HandleFunc("PATCH /api/v1/clusters/{name}/state", s.handleSetClusterState)
	mux.HandleFunc("POST /api/v1/clusters/{name}/capacity", s.handleReportCapacity)
	mux.HandleFunc("GET /api/v1/capacity", s.handleListCapacity)

	return chain(mux,
		requestID,
		recoverPanic(s.log),
		logging(s.log),
		authenticate(s.authn, s.log),
	)
}

// probeHandler returns the unauthenticated health surface.
//
// Deliberately a separate listener from the API: probes must work when the API
// is not exposed, and nothing here may reveal registry contents.
func (s *Server) probeHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return chain(mux, recoverPanic(s.log))
}

// Run serves until ctx is cancelled, then drains within the configured
// shutdown timeout.
//
// A deploy failing because cellcast was being upgraded is precisely the outage
// this project exists to prevent, so the drain is not optional.
func (s *Server) Run(ctx context.Context) error {
	api := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.apiHandler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
	}
	probe := &http.Server{
		Addr:              s.cfg.ProbeAddr,
		Handler:           s.probeHandler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
	}

	errc := make(chan error, 2)
	go func() { errc <- serve(probe, "probe", s.log) }()
	go func() { errc <- serve(api, "api", s.log) }()

	go s.markReadyWhenSynced(ctx)

	select {
	case err := <-errc:
		s.SetReady(false)
		shutdown(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout, s.log, api, probe)
		return err
	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining")
		s.SetReady(false)
		shutdown(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout, s.log, api, probe)
		return nil
	}
}

// markReadyWhenSynced flips the readiness probe once the registry is usable.
func (s *Server) markReadyWhenSynced(ctx context.Context) {
	if s.waitForSync != nil {
		if !s.waitForSync(ctx) {
			// Either the process is shutting down or the cache never synced. In
			// both cases staying unready is the answer, and the manager reports
			// the failure that caused it.
			s.log.Warn("registry cache did not sync, staying unready")
			return
		}
		s.log.Info("registry cache synced")
	}
	s.SetReady(true)
}

func serve(srv *http.Server, name string, log *slog.Logger) error {
	log.Info("listening", slog.String("server", name), slog.String("addr", srv.Addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}

func shutdown(ctx context.Context, timeout time.Duration, log *slog.Logger, servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("graceful shutdown failed", slog.String("addr", srv.Addr), slog.Any("error", err))
		}
	}
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already written, so this can only be logged.
		slog.Error("encoding response failed", slog.Any("error", err))
	}
}

// writeError writes a JSON error response.
//
// The message is deliberately coarse. A caller learns that it was rejected, not
// which cells exist or why a policy did not match; that detail goes to the hub
// log and the audit trail, which are readable by the operator rather than by
// the caller.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

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

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/metrics"
	"github.com/ethan-kane-ops/cellcast/internal/version"
)

// Server is the hub's HTTP API.
type Server struct {
	cfg   Config
	log   *slog.Logger
	authn Authenticator

	// limiter bounds placements per caller identity. Nil when the limit is
	// off, which allows everything.
	limiter *callerLimiter

	// k8s is the registry. Reads are served from the manager's informer cache,
	// so listing the fleet on every placement costs no API server traffic.
	k8s client.Client

	// waitForSync blocks until the registry cache is usable. Nil means there is
	// nothing to wait for, which is the case in tests.
	waitForSync func(context.Context) bool

	// capacity is the in-memory utilisation index. Nil means the capacity
	// endpoints report 503 rather than panicking.
	capacity *capacity.Registry

	// minter issues credentials for a chosen cell. Nil means the placement
	// route refuses rather than returning a cell with no way to reach it.
	minter Minter

	// placer decides which cell a caller may reach. Nil means the placement
	// route reports itself unavailable.
	placer Placer

	// audit is the trail of decisions and issued credentials. Never nil:
	// NewServer builds one if no option supplied it, because a broker that can
	// be run without an audit trail will be.
	audit *audit.Auditor

	// events publishes the audit trail a second way, as Kubernetes Events on
	// the cell each record concerns. Nil means the JSON trail is the only view,
	// which is the case wherever the hub has no manager behind it.
	events events.EventRecorder

	// metrics counts the same records for Prometheus. Nil is safe: every method
	// on it tolerates a nil receiver, so a hub with no metrics endpoint needs no
	// guard at each call site.
	metrics *metrics.Metrics

	// ready gates the readiness probe. A replica that has not finished starting
	// must not accept traffic and answer placements it cannot score.
	ready atomic.Bool

	// warmupCheck reports whether the capacity index covers the fleet. Nil
	// means there is nothing to warm up, which is the case for any hub built
	// without a capacity index.
	warmupCheck WarmupCheck

	// warm gates the placement route. Readiness is bounded by a deadline and
	// this is not, so a replica that went ready early still refuses to place
	// until it can actually score.
	warm atomic.Bool

	// lastMissing holds the cells the most recent warmup check did not see, so
	// a replica that goes ready cold can say what it was waiting for.
	lastMissing atomic.Pointer[[]string]
}

// Option configures a Server.
type Option func(*Server)

// WithAuthenticator replaces the fail-closed default authenticator.
//
// The default rejects every caller, which is the correct behaviour for a broker
// with no way to identify who is asking.
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
// index is lost on one (docs/architecture.md ADR-002).
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

// WithEventRecorder attaches the Kubernetes Events view of the audit trail.
//
// Without it the trail is written to the log only. The narrow events interface
// rather than controller-runtime's: the only thing the hub does
// with a recorder is call Eventf, and a one-method dependency is one a test can
// stand in for without a broadcaster.
func WithEventRecorder(rec events.EventRecorder) Option {
	return func(s *Server) { s.events = rec }
}

// WithMetrics attaches the Prometheus view of the audit trail.
//
// Without it the hub still decides and still mints; it just is not measured,
// which is the correct behaviour when no metrics endpoint is bound.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Server) { s.metrics = m }
}

// WithWarmupCheck makes readiness wait for the fleet to check in.
//
// Without it a replica is warm from the first instant, which is what a test
// with a hand-seeded index wants and what a deployed hub must not have: an
// empty capacity index scores nothing, so a fresh replica taking traffic
// refuses every placement it is given until the heartbeats arrive.
func WithWarmupCheck(check WarmupCheck) Option {
	return func(s *Server) { s.warmupCheck = check }
}

// WithAuditor replaces the audit sink.
//
// The default writes JSON to stdout, which is what a deployed hub wants and
// what a test suite does not, so tests supply their own. There is no option to
// disable auditing: an operator who does not want the records can filter them
// downstream, where the choice is visible.
func WithAuditor(a *audit.Auditor) Option {
	return func(s *Server) { s.audit = a }
}

// NewServer builds a hub API server.
func NewServer(cfg Config, log *slog.Logger, opts ...Option) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid hub config: %w", err)
	}
	// Built before the options, so a test can swap in a limiter of its own.
	s := &Server{cfg: cfg, log: log, authn: denyAll{}, limiter: newCallerLimiter(cfg.PlacementRateLimit, cfg.PlacementBurst)}
	for _, opt := range opts {
		opt(s)
	}
	// Nothing to be cold about without a check, so latch warm at construction
	// rather than making every call site guard on a nil check.
	s.warm.Store(s.warmupCheck == nil)
	s.metrics.SetWarm(s.warm.Load())
	if s.audit == nil {
		// Both views are optional and both see every record. Deciding which
		// records deserve an Event or a counter belongs to each view, not to
		// the auditor.
		s.audit = audit.New(NewAuditLogger(cfg), s.clusterNotifier(), s.metrics)
	}
	return s, nil
}

// clusterNotifier returns the Kubernetes Events view, or nil if the hub has no
// way to publish one.
//
// Returning an untyped nil matters: a nil *clusterEvents inside a non-nil
// interface would pass the auditor's nil check and then dereference.
func (s *Server) clusterNotifier() audit.Notifier {
	if s.events == nil || s.k8s == nil {
		return nil
	}
	return &clusterEvents{
		recorder:  s.events,
		reader:    s.k8s,
		namespace: s.cfg.Namespace,
		log:       s.log,
	}
}

// SetReady marks the server ready to serve traffic.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// apiHandler returns the authenticated API surface.
//
// Everything mounted here sits behind the authentication middleware by
// construction, so a new route cannot accidentally be served anonymously.
func (s *Server) apiHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, version.Get())
	})

	mux.HandleFunc("POST /api/v1/placement", s.handlePlacement)
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
		authenticate(s.authn, s.log, s.metrics),
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

	if s.capacity != nil && s.warmupCheck == nil {
		s.log.Warn("no warmup check configured; this replica will accept placements before any cell has reported")
	}

	errc := make(chan error, 2)
	go func() { errc <- serve(probe, "probe", s.log) }()
	go func() { errc <- serve(api, "api", s.log) }()

	go s.startup(ctx)

	select {
	case err := <-errc:
		// A listener failed, so there is nothing to drain towards: the address
		// is not serving and no traffic is being routed to it.
		s.SetReady(false)
		shutdown(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout, s.log, api, probe)
		return err
	case <-ctx.Done():
		s.drain(context.WithoutCancel(ctx), api, probe)
		return nil
	}
}

// drain takes the replica out of service and then closes it down.
//
// The order and the pause between the two halves are the whole point. Going
// unready first is what makes the kubelet's next probe fail and the endpoint
// controller remove this pod; the pause is what gives that removal time to
// reach every kube-proxy before the listener stops answering. Closing on the
// signal instead would refuse whatever was routed here in the meantime, which
// is a deploy failing because cellcast was being upgraded.
func (s *Server) drain(ctx context.Context, servers ...*http.Server) {
	s.SetReady(false)
	if s.cfg.DrainDelay > 0 {
		s.log.Info("shutdown signal received, reporting unready",
			slog.Duration("drain_delay", s.cfg.DrainDelay))
		timer := time.NewTimer(s.cfg.DrainDelay)
		defer timer.Stop()
		<-timer.C
	}
	s.log.Info("draining in-flight requests",
		slog.Duration("shutdown_timeout", s.cfg.ShutdownTimeout))
	shutdown(ctx, s.cfg.ShutdownTimeout, s.log, servers...)
}

// startup holds readiness until this replica can answer usefully.
//
// Two waits, not one, because they fail differently. A cache that never syncs
// means the replica cannot read the registry at all and must stay unready
// indefinitely; a fleet that has not checked in yet is a wait with a deadline,
// since agents heartbeat through the Service and cannot reach a hub that is
// waiting for them.
func (s *Server) startup(ctx context.Context) {
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
	s.awaitWarmth(ctx)
	if ctx.Err() != nil {
		return
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
// The message is coarse. A caller learns that it was rejected, not
// which cells exist or why a policy did not match; that detail goes to the hub
// log and the audit trail, which are readable by the operator rather than by
// the caller.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

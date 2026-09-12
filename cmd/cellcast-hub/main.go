// Command cellcast-hub runs the cellcast API and controllers.
//
// This is the only binary that can mint credentials. See
// docs/architecture.md ADR-007.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	promb "go.opentelemetry.io/contrib/bridges/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/ethan-kane-ops/cellcast/internal/hub"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/metrics"
	"github.com/ethan-kane-ops/cellcast/internal/hub/oidc"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
	"github.com/ethan-kane-ops/cellcast/internal/telemetry"
	"github.com/ethan-kane-ops/cellcast/internal/version"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cfg := hub.DefaultConfig()
	mgrOpts := hub.ManagerOptions{
		// On by default, on the port after the probes. A service in the deploy
		// critical path that cannot be scraped will not be adopted, and the
		// endpoint exposes cell names, policy names and refusal counts rather
		// than anything an authenticated caller could not already list through
		// the API. It is unauthenticated, so reaching it should be a
		// NetworkPolicy decision: see docs/metrics.md. "0" disables it.
		MetricsAddr: ":8082",
		// On by default. The chart runs more than one replica, and a fleet
		// where three hubs each write Cluster status and each emit the same
		// Event is worse than one where a single replica does. Turning it off
		// is a single-replica development choice, not a production one.
		LeaderElection: true,
	}
	authCfg := oidc.DefaultConfig()
	telCfg := telemetry.DefaultConfig()
	var issuerFlags []string

	cmd := &cobra.Command{
		Use:   "cellcast-hub",
		Short: "cellcast placement oracle and credential broker",
		Long: `cellcast-hub answers placement requests and mints short-lived, scoped
credentials for the chosen cell.

It holds trust configuration, never credentials. See docs/threat-model.md for
what a compromise of this process does and does not grant.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgrOpts.Namespace = cfg.Namespace
			if mgrOpts.LeaderElectionNamespace == "" {
				mgrOpts.LeaderElectionNamespace = cfg.Namespace
			}
			for _, raw := range issuerFlags {
				issuer, err := oidc.ParseIssuerFlag(raw)
				if err != nil {
					return err
				}
				authCfg.Issuers = append(authCfg.Issuers, issuer)
			}
			if err := mgrOpts.ValidateAgainst(cfg); err != nil {
				return err
			}
			if err := telCfg.Validate(); err != nil {
				return err
			}
			return run(cmd.Context(), cfg, mgrOpts, authCfg, telCfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address for the API server")
	f.StringVar(&cfg.ProbeAddr, "probe-addr", cfg.ProbeAddr, "listen address for health and readiness probes")
	f.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout, "maximum time to read request headers")
	f.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "maximum time to drain in-flight requests on shutdown")
	f.DurationVar(&cfg.DrainDelay, "drain-delay", cfg.DrainDelay, "how long to keep serving while reporting unready, so endpoint removal can propagate before the listener closes")
	f.DurationVar(&cfg.WarmupTimeout, "warmup-timeout", cfg.WarmupTimeout, "how long a starting replica waits for the fleet to report capacity before reporting itself ready anyway")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")
	f.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (json, text)")
	f.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "namespace holding the cellcast registry")
	f.DurationVar(&cfg.CapacityStaleness, "capacity-staleness", cfg.CapacityStaleness, "how long an agent capacity report stays usable before the cell is excluded from scoring")
	f.DurationVar(&cfg.CapacityRetention, "capacity-retention", cfg.CapacityRetention, "how long a stale capacity entry is kept before it is dropped entirely")
	f.IntVar(&cfg.CapacityMaxCells, "capacity-max-cells", cfg.CapacityMaxCells, "maximum number of cells held in the in-memory capacity index")
	f.DurationVar(&cfg.TokenTTLCeiling, "token-max-ttl", cfg.TokenTTLCeiling, "absolute ceiling on minted credential lifetime; no policy or request may exceed it")
	f.Float64Var(&cfg.PlacementRateLimit, "placement-rate-limit", cfg.PlacementRateLimit, "placements a second one caller may make, per replica; 0 turns the limit off")
	f.IntVar(&cfg.PlacementBurst, "placement-burst", cfg.PlacementBurst, "placements one caller may make at once before the rate limit applies")
	f.StringVar(&mgrOpts.MetricsAddr, "metrics-addr", mgrOpts.MetricsAddr, "listen address for the Prometheus metrics endpoint (0 disables)")
	f.BoolVar(&mgrOpts.LeaderElection, "leader-election", mgrOpts.LeaderElection, "elect a leader for the reconciler path")
	f.StringVar(&mgrOpts.LeaderElectionNamespace, "leader-election-namespace", mgrOpts.LeaderElectionNamespace, "namespace holding the leader election lease (defaults to --namespace)")
	f.StringArrayVar(&issuerFlags, "oidc-issuer", nil, "trusted OIDC issuer as url=provider (repeatable), for example https://token.actions.githubusercontent.com=github")
	f.StringVar(&authCfg.Audience, "oidc-audience", authCfg.Audience, "audience callers must request, identifying this cellcast instance")
	f.DurationVar(&authCfg.ClockSkew, "oidc-clock-skew", authCfg.ClockSkew, "tolerance applied to token exp, nbf and iat")
	f.DurationVar(&authCfg.RefreshInterval, "oidc-refresh-interval", authCfg.RefreshInterval, "how often issuer metadata is re-resolved")
	f.DurationVar(&authCfg.HTTPTimeout, "oidc-http-timeout", authCfg.HTTPTimeout, "timeout for issuer discovery and JWKS fetches")
	f.StringVar(&authCfg.CAFile, "oidc-ca-file", authCfg.CAFile, "PEM bundle trusted when fetching issuer metadata, on top of the system roots (default: system roots)")
	telCfg.AddFlags(f)

	cmd.AddCommand(version.NewCommand())
	return cmd
}

// buildAuthenticator returns the OIDC authenticator, or nil to leave the hub's
// fail-closed default in place.
//
// Starting with no issuers is allowed and leaves every API route returning 401.
// Refusing to start instead would take down a hub whose OIDC configuration was
// mistakenly cleared, when what it should do is stop authorizing deploys and
// keep answering probes so the operator can see why.
func buildAuthenticator(ctx context.Context, cfg oidc.Config, log *slog.Logger) (*oidc.Authenticator, error) {
	if len(cfg.Issuers) == 0 {
		log.Warn("no --oidc-issuer configured; every API request will be rejected as unauthenticated")
		return nil, nil
	}

	authn, err := oidc.New(ctx, cfg, log)
	if err != nil {
		return nil, err
	}

	log.Info("oidc authentication enabled",
		"issuers", strings.Join(issuerURLs(cfg), ","),
		"audience", cfg.Audience,
	)
	return authn, nil
}

// issuerURLs is the configured allowlist as plain URLs. The policy controller
// reports against the same list the authenticator verifies against, so there is
// one answer to "which issuers does this hub trust" rather than two.
func issuerURLs(cfg oidc.Config) []string {
	out := make([]string, 0, len(cfg.Issuers))
	for _, iss := range cfg.Issuers {
		out = append(out, iss.Issuer)
	}
	return out
}

func run(ctx context.Context, cfg hub.Config, mgrOpts hub.ManagerOptions, authCfg oidc.Config, telCfg telemetry.Config) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := hub.NewLogger(cfg)
	log.Info("starting cellcast-hub",
		"version", version.Get().Version,
		"namespace", cfg.Namespace,
	)

	// Before anything that could start a span, and flushed after the server has
	// drained, so the last requests a replica served are exported rather than
	// lost with the process.
	tel, err := telemetry.Start(ctx, telCfg, "cellcast-hub", log,
		// The registry the metrics endpoint serves, exported as it stands: one
		// surface and one set of names, whether it is scraped or pushed.
		telemetry.WithMetrics(promb.NewMetricProducer(promb.WithGatherer(ctrlmetrics.Registry))))
	if err != nil {
		return err
	}
	defer func() {
		if err := tel.Shutdown(ctx); err != nil {
			log.Warn("flushing telemetry failed", slog.Any("error", err))
		}
	}()

	mgr, err := hub.NewManager(mgrOpts, log)
	if err != nil {
		return err
	}

	index := capacity.New(capacity.Options{
		Staleness: cfg.CapacityStaleness,
		Retention: cfg.CapacityRetention,
		MaxCells:  cfg.CapacityMaxCells,
	})
	if err := hub.RegisterControllers(mgr, index, cfg.Namespace, issuerURLs(authCfg)); err != nil {
		return err
	}

	// GetAPIReader for credential Secrets, GetClient for everything else. Trust
	// configuration is cached because it is read on every mint and changes
	// rarely; the material that authenticates the hub to a spoke is fetched at
	// the moment it is used and not retained (docs/threat-model.md T-08).
	connector := broker.NewSecretConnector(mgr.GetAPIReader(), cfg.Namespace, mgr.GetConfig())
	minter := broker.New(
		mgr.GetClient(), cfg.Namespace, cfg.TokenTTLCeiling, log,
		broker.NewKubernetesProvider(connector.Connect),
	)

	engine := placement.NewEngine(mgr.GetClient(), index, cfg.Namespace, log)

	// Registered with controller-runtime's registry, which is what the
	// manager's metrics endpoint already serves. A second listener would mean a
	// fourth port on a process that keeps them to a minimum.
	hubMetrics, err := metrics.New(ctrlmetrics.Registry)
	if err != nil {
		return fmt.Errorf("registering metrics: %w", err)
	}
	if err := ctrlmetrics.Registry.Register(metrics.NewCapacityCollector(index)); err != nil {
		return fmt.Errorf("registering the capacity collector: %w", err)
	}
	if err := ctrlmetrics.Registry.Register(
		metrics.NewClusterStateCollector(mgr.GetClient(), cfg.Namespace, log)); err != nil {
		return fmt.Errorf("registering the cluster state collector: %w", err)
	}

	serverOpts := []hub.Option{
		hub.WithClusterClient(mgr.GetClient()),
		hub.WithCapacityRegistry(index),
		hub.WithPlacer(engine),
		hub.WithMinter(minter),
		hub.WithCacheSync(mgr.GetCache().WaitForCacheSync),
		// Readiness waits for the fleet, not just for the cache. A replica
		// whose cache has synced still knows nothing about how loaded anything
		// is, and taking traffic in that state means refusing every placement
		// for as long as it takes the agents to notice.
		hub.WithWarmupCheck(hub.FleetCoverage(mgr.GetClient(), index, cfg.Namespace)),
		// The audit trail's second view. Records go to stdout regardless; this
		// also hangs them on the Cluster they concern, so `kubectl describe
		// cluster` answers "who has been deploying here".
		hub.WithEventRecorder(mgr.GetEventRecorder("cellcast-hub")),
		hub.WithMetrics(hubMetrics),
		hub.WithTracerProvider(tel.TracerProvider()),
	}

	authn, err := buildAuthenticator(ctx, authCfg, log)
	if err != nil {
		return err
	}
	if authn != nil {
		serverOpts = append(serverOpts, hub.WithAuthenticator(authn))
	}

	srv, err := hub.NewServer(cfg, log, serverOpts...)
	if err != nil {
		return err
	}

	// The API and the controllers are one process and share a fate. An API
	// without controllers serves a status nobody is updating; controllers
	// without an API serve nobody at all. Whichever stops first stops the
	// other, so a partial failure surfaces as a restart rather than as a hub
	// that looks healthy and answers wrongly.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The capacity index is not in the fate-sharing set. It holds no listener
	// and nothing to fail: it stops when runCtx is cancelled by whichever of
	// the two below stops first.
	go index.Run(runCtx, log)

	errc := make(chan error, 2)
	go func() { errc <- mgr.Start(runCtx) }()
	go func() { errc <- srv.Run(runCtx) }()

	err = <-errc
	cancel()
	if second := <-errc; err == nil {
		err = second
	}
	return err
}

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

	"github.com/ethan-kane-ops/cellcast/internal/hub"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/oidc"
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
		// Metrics are off until ENG-178 owns what is exposed. Binding a third
		// port by default in a credential broker is not a free choice.
		MetricsAddr: "0",
	}
	authCfg := oidc.DefaultConfig()
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
			return run(cmd.Context(), cfg, mgrOpts, authCfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address for the API server")
	f.StringVar(&cfg.ProbeAddr, "probe-addr", cfg.ProbeAddr, "listen address for health and readiness probes")
	f.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout, "maximum time to read request headers")
	f.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "maximum time to drain in-flight requests on shutdown")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")
	f.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (json, text)")
	f.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "namespace holding the cellcast registry")
	f.DurationVar(&cfg.CapacityStaleness, "capacity-staleness", cfg.CapacityStaleness, "how long an agent capacity report stays usable before the cell is excluded from scoring")
	f.DurationVar(&cfg.CapacityRetention, "capacity-retention", cfg.CapacityRetention, "how long a stale capacity entry is kept before it is dropped entirely")
	f.IntVar(&cfg.CapacityMaxCells, "capacity-max-cells", cfg.CapacityMaxCells, "maximum number of cells held in the in-memory capacity index")
	f.DurationVar(&cfg.TokenTTLCeiling, "token-max-ttl", cfg.TokenTTLCeiling, "absolute ceiling on minted credential lifetime; no policy or request may exceed it")
	f.StringVar(&mgrOpts.MetricsAddr, "metrics-addr", mgrOpts.MetricsAddr, "listen address for controller metrics (0 disables)")
	f.BoolVar(&mgrOpts.LeaderElection, "leader-election", mgrOpts.LeaderElection, "elect a leader for the reconciler path")
	f.StringVar(&mgrOpts.LeaderElectionNamespace, "leader-election-namespace", mgrOpts.LeaderElectionNamespace, "namespace holding the leader election lease (defaults to --namespace)")
	f.StringArrayVar(&issuerFlags, "oidc-issuer", nil, "trusted OIDC issuer as url=provider (repeatable), for example https://token.actions.githubusercontent.com=github")
	f.StringVar(&authCfg.Audience, "oidc-audience", authCfg.Audience, "audience callers must request, identifying this cellcast instance")
	f.DurationVar(&authCfg.ClockSkew, "oidc-clock-skew", authCfg.ClockSkew, "tolerance applied to token exp, nbf and iat")
	f.DurationVar(&authCfg.RefreshInterval, "oidc-refresh-interval", authCfg.RefreshInterval, "how often issuer metadata is re-resolved")
	f.DurationVar(&authCfg.HTTPTimeout, "oidc-http-timeout", authCfg.HTTPTimeout, "timeout for issuer discovery and JWKS fetches")

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

	issuers := make([]string, 0, len(cfg.Issuers))
	for _, iss := range cfg.Issuers {
		issuers = append(issuers, iss.Issuer)
	}
	log.Info("oidc authentication enabled",
		"issuers", strings.Join(issuers, ","),
		"audience", cfg.Audience,
	)
	return authn, nil
}

func run(ctx context.Context, cfg hub.Config, mgrOpts hub.ManagerOptions, authCfg oidc.Config) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := hub.NewLogger(cfg)
	log.Info("starting cellcast-hub",
		"version", version.Get().Version,
		"namespace", cfg.Namespace,
	)

	mgr, err := hub.NewManager(mgrOpts, log)
	if err != nil {
		return err
	}

	index := capacity.New(capacity.Options{
		Staleness: cfg.CapacityStaleness,
		Retention: cfg.CapacityRetention,
		MaxCells:  cfg.CapacityMaxCells,
	})
	if err := hub.RegisterControllers(mgr, index, cfg.Namespace); err != nil {
		return err
	}

	// GetAPIReader for credential Secrets, GetClient for everything else. The
	// split is the point: trust configuration is cached because it is read on
	// every mint and changes rarely, while the material that authenticates the
	// hub to a spoke is fetched at the moment it is used and not retained
	// (docs/threat-model.md T-08).
	connector := broker.NewSecretConnector(mgr.GetAPIReader(), cfg.Namespace, mgr.GetConfig())
	minter := broker.New(
		mgr.GetClient(), cfg.Namespace, cfg.TokenTTLCeiling, log,
		broker.NewKubernetesProvider(connector.Connect),
	)

	serverOpts := []hub.Option{
		hub.WithClusterClient(mgr.GetClient()),
		hub.WithCapacityRegistry(index),
		hub.WithMinter(minter),
		hub.WithCacheSync(mgr.GetCache().WaitForCacheSync),
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

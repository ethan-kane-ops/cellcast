// Command cellcast-hub runs the cellcast API and controllers.
//
// This is the only binary that can mint credentials. See
// docs/architecture.md ADR-007.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/cellcast/internal/hub"
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
			return run(cmd.Context(), cfg, mgrOpts)
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
	f.StringVar(&mgrOpts.MetricsAddr, "metrics-addr", mgrOpts.MetricsAddr, "listen address for controller metrics (0 disables)")
	f.BoolVar(&mgrOpts.LeaderElection, "leader-election", mgrOpts.LeaderElection, "elect a leader for the reconciler path")
	f.StringVar(&mgrOpts.LeaderElectionNamespace, "leader-election-namespace", mgrOpts.LeaderElectionNamespace, "namespace holding the leader election lease (defaults to --namespace)")

	cmd.AddCommand(version.NewCommand())
	return cmd
}

func run(ctx context.Context, cfg hub.Config, mgrOpts hub.ManagerOptions) error {
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
	if err := hub.RegisterControllers(mgr); err != nil {
		return err
	}

	srv, err := hub.NewServer(cfg, log,
		hub.WithClusterClient(mgr.GetClient()),
		hub.WithCacheSync(mgr.GetCache().WaitForCacheSync),
	)
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

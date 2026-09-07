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

	cmd := &cobra.Command{
		Use:   "cellcast-hub",
		Short: "cellcast placement oracle and credential broker",
		Long: `cellcast-hub answers placement requests and mints short-lived, scoped
credentials for the chosen cell.

It holds trust configuration, never credentials. See docs/threat-model.md for
what a compromise of this process does and does not grant.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address for the API server")
	f.StringVar(&cfg.ProbeAddr, "probe-addr", cfg.ProbeAddr, "listen address for health and readiness probes")
	f.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout, "maximum time to read request headers")
	f.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "maximum time to drain in-flight requests on shutdown")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")
	f.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (json, text)")

	cmd.AddCommand(version.NewCommand())
	return cmd
}

func run(ctx context.Context, cfg hub.Config) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := hub.NewLogger(cfg)
	log.Info("starting cellcast-hub", "version", version.Get().Version)

	srv, err := hub.NewServer(cfg, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

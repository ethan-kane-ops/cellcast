// Command cellcast-agent reports cell capacity to the cellcast hub.
//
// It runs as a Deployment with leader election rather than a DaemonSet:
// capacity is a cluster-level fact, so one reporter per node would publish N
// copies of the same number at N times the API server cost.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"

	"github.com/ethan-kane-ops/cellcast/internal/agent"
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
	cfg := agent.DefaultConfig()
	telCfg := telemetry.DefaultConfig()

	cmd := &cobra.Command{
		Use:   "cellcast-agent",
		Short: "cellcast in-cluster capacity reporter",
		Long: `cellcast-agent publishes this cell's committed capacity to the cellcast
hub on a heartbeat.

It reports requested resources rather than momentary usage, because placement
cares about what is committed. It holds no credentials and cannot mint: the only
thing it presents to the hub is the ServiceAccount token the cluster projects
for it, and the hub accepts that token only for the cell whose registration
names it.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := telCfg.Validate(); err != nil {
				return err
			}
			return run(cmd.Context(), cfg, telCfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.HubEndpoint, "hub-endpoint", cfg.HubEndpoint, "cellcast hub URL to report to")
	f.StringVar(&cfg.HubCAFile, "hub-ca-file", cfg.HubCAFile, "PEM bundle verifying the hub's certificate (default: system roots)")
	f.StringVar(&cfg.CellName, "cell-name", cfg.CellName, "name of the Cluster resource this agent reports for")
	f.StringVar(&cfg.TokenPath, "token-path", cfg.TokenPath, "projected ServiceAccount token presented to the hub")
	f.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "how often to publish capacity")
	f.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "timeout for a single report to the hub")
	f.DurationVar(&cfg.MaxBackoff, "max-backoff", cfg.MaxBackoff, "ceiling on the retry interval while the hub is unreachable")
	f.StringVar(&cfg.Kubeconfig, "kubeconfig", cfg.Kubeconfig, "path to a kubeconfig (default: in-cluster, then the ambient one)")
	f.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "namespace holding the leader election lease")
	f.BoolVar(&cfg.LeaderElection, "leader-election", cfg.LeaderElection, "report from one replica at a time")
	f.StringVar(&cfg.ProbeAddr, "probe-addr", cfg.ProbeAddr, "listen address for health and readiness probes")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")
	f.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (json, text)")
	telCfg.AddFlags(f)

	cmd.AddCommand(version.NewCommand())
	return cmd
}

func run(ctx context.Context, cfg agent.Config, telCfg telemetry.Config) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := agent.NewLogger(cfg)
	log.Info("starting cellcast-agent",
		"version", version.Get().Version,
		"cell", cfg.CellName,
		"hub", cfg.HubEndpoint,
	)

	// The cell describes the process rather than any one report, so it is a
	// resource attribute on everything the agent exports and not a span
	// attribute on each span.
	tel, err := telemetry.Start(ctx, telCfg, "cellcast-agent", log,
		telemetry.WithResource(attribute.String("cellcast.cell", cfg.CellName)))
	if err != nil {
		return err
	}
	defer func() {
		if err := tel.Shutdown(ctx); err != nil {
			log.Warn("flushing telemetry failed", slog.Any("error", err))
		}
	}()

	return agent.Run(ctx, cfg, log, tel.TracerProvider())
}

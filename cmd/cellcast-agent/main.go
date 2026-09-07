// Command cellcast-agent reports cell capacity to the cellcast hub.
//
// It runs as a Deployment with leader election rather than a DaemonSet:
// capacity is a cluster-level fact, so one reporter per node would publish N
// copies of the same number at N times the API server cost.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/cellcast/internal/agent"
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

	cmd := &cobra.Command{
		Use:   "cellcast-agent",
		Short: "cellcast in-cluster capacity reporter",
		Long: `cellcast-agent publishes this cell's committed capacity to the cellcast
hub on a heartbeat.

It reports requested resources rather than momentary usage, because placement
cares about what is committed. It holds no credentials and cannot mint.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}
			// Reporting loop lands in ENG-174.
			return fmt.Errorf("capacity reporting is not implemented yet (ENG-174)")
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.HubEndpoint, "hub-endpoint", cfg.HubEndpoint, "cellcast hub URL to report to")
	f.StringVar(&cfg.CellName, "cell-name", cfg.CellName, "name of the Cluster resource this agent reports for")
	f.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", cfg.HeartbeatInterval, "how often to publish capacity")
	f.StringVar(&cfg.ProbeAddr, "probe-addr", cfg.ProbeAddr, "listen address for health and readiness probes")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")
	f.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format (json, text)")

	cmd.AddCommand(version.NewCommand())
	return cmd
}

// Package agent implements the in-cluster capacity reporter.
//
// The agent runs in every registered cell, which makes it the largest attack
// surface in the system by instance count. It deliberately contains no minting
// code: that lives only in the hub binary (docs/architecture.md ADR-007,
// docs/threat-model.md T-07).
package agent

import (
	"fmt"
	"time"
)

// Config is the agent's runtime configuration.
type Config struct {
	// HubEndpoint is the cellcast hub this agent reports to.
	HubEndpoint string
	// CellName is the Cluster resource this agent reports capacity for. An
	// agent may only report for its own cell.
	CellName string
	// HeartbeatInterval is how often capacity is published. Jitter is applied
	// so that a large fleet does not synchronise into a thundering herd.
	HeartbeatInterval time.Duration
	// ProbeAddr is the listen address for health and readiness probes.
	ProbeAddr string
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// LogFormat is one of json, text.
	LogFormat string
}

// DefaultConfig returns the configuration the agent runs with when nothing is
// overridden.
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval: 30 * time.Second,
		ProbeAddr:         ":8081",
		LogLevel:          "info",
		LogFormat:         "json",
	}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if c.HubEndpoint == "" {
		return fmt.Errorf("hub-endpoint must not be empty")
	}
	if c.CellName == "" {
		return fmt.Errorf("cell-name must not be empty")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("heartbeat-interval must be positive, got %s", c.HeartbeatInterval)
	}
	return nil
}

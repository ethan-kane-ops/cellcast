// Package agent implements the in-cluster capacity reporter.
//
// The agent runs in every registered cell, which makes it the largest attack
// surface in the system by instance count. It contains no minting
// code: that lives only in the hub binary (docs/architecture.md ADR-007,
// docs/threat-model.md T-07). It holds no credential of its own either. The
// only thing it presents to the hub is the ServiceAccount token the kubelet
// projects for it, which the cluster issues, rotates and expires without the
// agent's involvement.
package agent

import (
	"fmt"
	"time"
)

// Defaults for an agent reporting to a hub with the stock 90s staleness window.
const (
	// DefaultHeartbeatInterval is three heartbeats inside that window, so a
	// single dropped report never takes a healthy cell out of scoring.
	DefaultHeartbeatInterval = 30 * time.Second

	// DefaultTokenPath is where the projected ServiceAccount token is mounted.
	// Not the default `/var/run/secrets/kubernetes.io/serviceaccount/token`:
	// that one is minted for the API server's audience, and the hub must reject
	// a token minted for somebody else. The cellcast audience needs its own
	// projected volume, which is why this has its own path.
	DefaultTokenPath = "/var/run/secrets/cellcast/token" // #nosec G101 -- a mount path, not a credential

	// DefaultRequestTimeout bounds one heartbeat. A hub that hangs must not
	// hold the loop open past the next tick.
	DefaultRequestTimeout = 10 * time.Second

	// DefaultMaxBackoff caps the retry interval while the hub is unreachable.
	//
	// Deliberately short. Backing off further would be gentler on a struggling
	// hub, but a cell whose reports are not landing is Unknown and out of
	// scoring, so a long backoff turns a brief hub blip into minutes of a
	// healthy cell being unplaceable.
	DefaultMaxBackoff = 2 * time.Minute

	// DefaultNamespace is where the leader election lease lives when the
	// downward API has not supplied the real one.
	DefaultNamespace = "cellcast-system"
)

// Config is the agent's runtime configuration.
type Config struct {
	// HubEndpoint is the cellcast hub this agent reports to.
	HubEndpoint string
	// HubCAFile is a PEM bundle used to verify the hub's certificate. Empty
	// means the system roots, which is right for a hub behind a public CA and
	// wrong for one behind an internal one.
	HubCAFile string
	// CellName is the Cluster resource this agent reports capacity for. An
	// agent may only report for its own cell.
	CellName string
	// TokenPath is the projected ServiceAccount token presented to the hub. It
	// is read fresh on every heartbeat, never cached.
	TokenPath string
	// HeartbeatInterval is how often capacity is published. Jitter is applied
	// so that a large fleet does not synchronise into a thundering herd.
	HeartbeatInterval time.Duration
	// RequestTimeout bounds a single report to the hub.
	RequestTimeout time.Duration
	// MaxBackoff caps the retry interval while the hub is unreachable.
	MaxBackoff time.Duration
	// Kubeconfig is an explicit path to a kubeconfig. Empty uses the in-cluster
	// configuration, falling back to the ambient one, which is what makes the
	// agent runnable against a kind cluster from a laptop.
	Kubeconfig string
	// Namespace is where the leader election lease is held.
	Namespace string
	// LeaderElection runs the reporting loop on one replica at a time.
	LeaderElection bool
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
		TokenPath:         DefaultTokenPath,
		HeartbeatInterval: DefaultHeartbeatInterval,
		RequestTimeout:    DefaultRequestTimeout,
		MaxBackoff:        DefaultMaxBackoff,
		Namespace:         DefaultNamespace,
		LeaderElection:    true,
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
	if c.TokenPath == "" {
		return fmt.Errorf("token-path must not be empty; the agent has no other way to identify itself")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("heartbeat-interval must be positive, got %s", c.HeartbeatInterval)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request-timeout must be positive, got %s", c.RequestTimeout)
	}
	if c.RequestTimeout >= c.HeartbeatInterval {
		// Otherwise a hung hub leaves the previous attempt still running when
		// the next tick fires, and the agent's own backlog grows.
		return fmt.Errorf("request-timeout (%s) must be shorter than heartbeat-interval (%s)",
			c.RequestTimeout, c.HeartbeatInterval)
	}
	if c.MaxBackoff < c.HeartbeatInterval {
		return fmt.Errorf("max-backoff (%s) must not be shorter than heartbeat-interval (%s)",
			c.MaxBackoff, c.HeartbeatInterval)
	}
	if c.LeaderElection && c.Namespace == "" {
		return fmt.Errorf("namespace must not be empty when leader election is enabled; it is where the lease lives")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log-level must be one of debug, info, warn, error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("log-format must be one of json, text, got %q", c.LogFormat)
	}
	return nil
}

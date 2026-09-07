package hub

import (
	"fmt"
	"time"
)

// Config is the hub's runtime configuration.
type Config struct {
	// Addr is the listen address for the API server.
	Addr string
	// ProbeAddr is the listen address for health and readiness probes. Kept
	// separate from Addr so probes stay reachable when the API is not exposed.
	ProbeAddr string
	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout bounds the graceful drain on SIGTERM.
	ShutdownTimeout time.Duration
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// LogFormat is one of json, text.
	LogFormat string
	// Namespace is where the hub reads and writes its own resources.
	//
	// Cluster and PlacementPolicy are namespaced so that one hub cluster can
	// host more than one cellcast installation without either seeing the
	// other's fleet, and so the chart can grant a Role rather than a
	// ClusterRole.
	Namespace string
}

// DefaultConfig returns the configuration the hub runs with when nothing is
// overridden.
func DefaultConfig() Config {
	return Config{
		Addr:              ":8080",
		ProbeAddr:         ":8081",
		ReadHeaderTimeout: 10 * time.Second,
		ShutdownTimeout:   30 * time.Second,
		LogLevel:          "info",
		LogFormat:         "json",
		Namespace:         "cellcast-system",
	}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr must not be empty")
	}
	if c.ProbeAddr == "" {
		return fmt.Errorf("probe-addr must not be empty")
	}
	if c.Addr == c.ProbeAddr {
		return fmt.Errorf("addr and probe-addr must differ, both are %q", c.Addr)
	}
	if c.ReadHeaderTimeout <= 0 {
		return fmt.Errorf("read-header-timeout must be positive, got %s", c.ReadHeaderTimeout)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown-timeout must be positive, got %s", c.ShutdownTimeout)
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
	if c.Namespace == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	return nil
}

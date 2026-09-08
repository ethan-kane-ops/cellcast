package hub

import (
	"fmt"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
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
	// ShutdownTimeout bounds the graceful drain on SIGTERM, measured from the
	// end of DrainDelay.
	ShutdownTimeout time.Duration
	// DrainDelay is how long the hub keeps serving, already reporting itself
	// unready, before it closes its listeners.
	//
	// Kubernetes removes a pod from Service endpoints asynchronously, and it
	// sends SIGTERM at the same moment it starts. Closing the listener on the
	// signal therefore refuses the requests still being routed here while that
	// removal propagates through every kube-proxy and every client's connection
	// pool, which is a deploy failing because cellcast was being upgraded.
	//
	// DrainDelay plus ShutdownTimeout must fit inside the pod's
	// terminationGracePeriodSeconds, or the kubelet's SIGKILL arrives mid-drain
	// and undoes both.
	DrainDelay time.Duration
	// WarmupTimeout bounds how long a starting replica waits for capacity
	// before it reports itself ready anyway.
	//
	// Capacity is in-memory per replica and arrives only from agent heartbeats
	// (docs/architecture.md ADR-002), so a replica that has just started knows
	// nothing about the fleet and would refuse every placement it was given.
	// Readiness therefore waits for the fleet to check in. The wait has to be
	// bounded, because agents heartbeat through the Service and a Service
	// routes only to ready pods: without a deadline, a fleet whose hub replicas
	// all restarted at once would wait for heartbeats that nothing can deliver.
	WarmupTimeout time.Duration
	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// LogFormat is one of json, text.
	LogFormat string
	// CapacityStaleness is how long an agent report stays usable. Past it the
	// cell is Unknown and excluded from scoring rather than read as empty.
	CapacityStaleness time.Duration
	// CapacityRetention is how long an Unknown entry is kept before it is
	// dropped, so "went quiet" stays distinguishable from "never reported".
	CapacityRetention time.Duration
	// CapacityMaxCells bounds the in-memory capacity index.
	CapacityMaxCells int

	// TokenTTLCeiling is the absolute bound on minted credential lifetime. No
	// PlacementPolicy and no request may exceed it.
	//
	// This is a real control rather than defence in depth. A Kubernetes API
	// server applies no maximum of its own unless the operator configured
	// --service-account-max-token-expiration, and a default kind cluster will
	// issue a token lasting years if asked. This bound is the one that is
	// certain to exist (docs/threat-model.md T-01).
	TokenTTLCeiling time.Duration

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
		// Sized so that DrainDelay plus this one fits inside Kubernetes'
		// default terminationGracePeriodSeconds of 30, with headroom. A drain
		// the kubelet interrupts with SIGKILL is not a drain.
		ShutdownTimeout: 20 * time.Second,
		// Endpoint propagation is usually well under a second and is
		// occasionally much worse, so this is sized for the bad case rather
		// than the common one. It costs a rolling update five seconds per pod
		// and buys the property the rollout exists to preserve.
		DrainDelay: 5 * time.Second,
		// One staleness window. An agent heartbeating on the default interval
		// checks in three times inside it, so a replica that is still cold at
		// the deadline is not waiting on timing, it is waiting on something
		// that is broken.
		WarmupTimeout: capacity.DefaultStaleness,
		LogLevel:          "info",
		LogFormat:         "json",
		CapacityStaleness: capacity.DefaultStaleness,
		CapacityRetention: capacity.DefaultRetention,
		CapacityMaxCells:  capacity.DefaultMaxCells,
		TokenTTLCeiling:   time.Hour,
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
	if c.DrainDelay < 0 {
		return fmt.Errorf("drain-delay must not be negative, got %s", c.DrainDelay)
	}
	if c.WarmupTimeout < 0 {
		return fmt.Errorf("warmup-timeout must not be negative, got %s", c.WarmupTimeout)
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
	if c.CapacityStaleness <= 0 {
		return fmt.Errorf("capacity-staleness must be positive, got %s", c.CapacityStaleness)
	}
	if c.CapacityRetention < c.CapacityStaleness {
		return fmt.Errorf("capacity-retention (%s) must not be shorter than capacity-staleness (%s); "+
			"dropping entries while they are still fresh would hide working cells",
			c.CapacityRetention, c.CapacityStaleness)
	}
	if c.CapacityMaxCells <= 0 {
		return fmt.Errorf("capacity-max-cells must be positive, got %d", c.CapacityMaxCells)
	}
	if c.TokenTTLCeiling <= 0 {
		return fmt.Errorf("token-max-ttl must be positive, got %s", c.TokenTTLCeiling)
	}
	if c.TokenTTLCeiling < broker.KubernetesMinTTL {
		// Rejected at startup rather than at deploy time. A ceiling under the
		// provider floor is not a stricter policy, it is a hub that accepts
		// every placement and then refuses to mint for any of them.
		return fmt.Errorf("token-max-ttl (%s) is below the %s floor the kubernetes TokenRequest API enforces; "+
			"no credential could ever be minted", c.TokenTTLCeiling, broker.KubernetesMinTTL)
	}
	if c.Namespace == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	return nil
}

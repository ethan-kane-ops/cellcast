package hub

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "defaults are valid",
			mutate: func(*Config) {},
		},
		{
			name:    "empty addr",
			mutate:  func(c *Config) { c.Addr = "" },
			wantErr: "addr must not be empty",
		},
		{
			name:    "empty probe addr",
			mutate:  func(c *Config) { c.ProbeAddr = "" },
			wantErr: "probe-addr must not be empty",
		},
		{
			name:    "api and probe share a port",
			mutate:  func(c *Config) { c.ProbeAddr = c.Addr },
			wantErr: "must differ",
		},
		{
			name:    "zero read header timeout",
			mutate:  func(c *Config) { c.ReadHeaderTimeout = 0 },
			wantErr: "read-header-timeout must be positive",
		},
		{
			name:    "negative shutdown timeout",
			mutate:  func(c *Config) { c.ShutdownTimeout = -1 * time.Second },
			wantErr: "shutdown-timeout must be positive",
		},
		{
			name:    "negative drain delay",
			mutate:  func(c *Config) { c.DrainDelay = -1 * time.Second },
			wantErr: "drain-delay must not be negative",
		},
		{
			// Zero is disabled, not invalid. A single-replica development hub
			// has no endpoint propagation to wait for and no reason to pay for
			// it on every restart.
			name:   "zero drain delay is allowed",
			mutate: func(c *Config) { c.DrainDelay = 0 },
		},
		{
			name:    "negative warmup timeout",
			mutate:  func(c *Config) { c.WarmupTimeout = -1 * time.Second },
			wantErr: "warmup-timeout must not be negative",
		},
		{
			name:    "unknown log level",
			mutate:  func(c *Config) { c.LogLevel = "trace" },
			wantErr: "log-level must be one of",
		},
		{
			name:    "unknown log format",
			mutate:  func(c *Config) { c.LogFormat = "logfmt" },
			wantErr: "log-format must be one of",
		},
		{
			name:    "empty namespace",
			mutate:  func(c *Config) { c.Namespace = "" },
			wantErr: "namespace must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestManagerAddrsMustNotCollideWithTheServers is the guard for a bug that
// shipped once: the metrics endpoint defaulted to the probe port.
//
// The three listeners are configured in two structs and bound by two different
// pieces of machinery, so nothing but this comparison is in a position to
// notice. A collision otherwise surfaces as "address already in use" from
// inside a library, seconds after the process looked like it was starting.
func TestManagerAddrsMustNotCollideWithTheServers(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name    string
		metrics string
		wantErr bool
	}{
		{name: "the shipped default", metrics: ":8082"},
		{name: "disabled", metrics: "0"},
		{name: "unset leaves the manager to its own default", metrics: ""},
		{name: "collides with the api", metrics: cfg.Addr, wantErr: true},
		{name: "collides with the probes", metrics: cfg.ProbeAddr, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ManagerOptions{MetricsAddr: tt.metrics}.ValidateAgainst(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateAgainst() = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

// TestTheShippedDefaultsDoNotCollide is the assertion the case above cannot
// make: it checks the value the binary actually starts with, not one repeated
// in the test.
func TestTheShippedDefaultsDoNotCollide(t *testing.T) {
	out := hubHelp(t)
	if !strings.Contains(out, "--metrics-addr") {
		t.Fatalf("--metrics-addr is not a flag:\n%s", out)
	}

	defaults := regexp.MustCompile(`--(addr|probe-addr|metrics-addr) string\s+.*?\(default "([^"]+)"\)`)
	seen := map[string]string{}
	for _, m := range defaults.FindAllStringSubmatch(out, -1) {
		flag, addr := m[1], m[2]
		if other, dup := seen[addr]; dup {
			t.Errorf("--%s and --%s both default to %s", flag, other, addr)
		}
		seen[addr] = flag
	}
	if len(seen) != 3 {
		t.Errorf("found %d listen address defaults, want 3: %v", len(seen), seen)
	}
}

// hubHelp runs the hub binary's own --help.
//
// The point of going through the binary is that a flag's default lives in main
// and a test that restated it would agree with itself rather than with what
// ships.
func hubHelp(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "run", "../../cmd/cellcast-hub", "--help").CombinedOutput()
	if err != nil {
		t.Skipf("building the hub binary: %v", err)
	}
	return string(out)
}

// kubeletDefaultGracePeriod is Kubernetes' terminationGracePeriodSeconds when a
// pod spec does not set one.
const kubeletDefaultGracePeriod = 30 * time.Second

func TestTheShippedDrainFitsInsideTheDefaultGracePeriod(t *testing.T) {
	// The two halves of shutdown run one after the other, so what the kubelet
	// has to accommodate is their sum. Ship defaults that exceed it and the
	// SIGKILL lands mid-drain, which turns the graceful shutdown into the
	// ungraceful one it was added to replace, on a hub that never asked for a
	// longer grace period because it did not know it needed one.
	cfg := DefaultConfig()

	total := cfg.DrainDelay + cfg.ShutdownTimeout
	if total >= kubeletDefaultGracePeriod {
		t.Errorf("drain-delay (%s) plus shutdown-timeout (%s) is %s, which does not fit inside the default terminationGracePeriodSeconds of %s",
			cfg.DrainDelay, cfg.ShutdownTimeout, total, kubeletDefaultGracePeriod)
	}
}

func TestTheShippedWarmupOutlastsAHeartbeat(t *testing.T) {
	// A deadline shorter than the interval an agent reports on would expire
	// before the first heartbeat could possibly arrive, so every replica would
	// go ready cold every time and the wait would be decoration.
	cfg := DefaultConfig()

	if cfg.WarmupTimeout < cfg.CapacityStaleness {
		t.Errorf("warmup-timeout (%s) is shorter than capacity-staleness (%s); a replica would give up before a heartbeat could land",
			cfg.WarmupTimeout, cfg.CapacityStaleness)
	}
}

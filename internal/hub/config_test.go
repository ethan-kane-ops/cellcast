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
	out, err := exec.Command("go", "run", "../../cmd/cellcast-hub", "--help").CombinedOutput()
	if err != nil {
		t.Skipf("building the hub binary: %v", err)
	}
	if !strings.Contains(string(out), "--metrics-addr") {
		t.Fatalf("--metrics-addr is not a flag:\n%s", out)
	}

	defaults := regexp.MustCompile(`--(addr|probe-addr|metrics-addr) string\s+.*?\(default "([^"]+)"\)`)
	seen := map[string]string{}
	for _, m := range defaults.FindAllStringSubmatch(string(out), -1) {
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

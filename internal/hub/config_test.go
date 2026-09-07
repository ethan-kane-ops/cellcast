package hub

import (
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

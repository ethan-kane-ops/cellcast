package agent

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		c := DefaultConfig()
		c.HubEndpoint = "https://cellcast.example.test"
		c.CellName = "cell-01"
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "fully specified config is valid",
			mutate: func(*Config) {},
		},
		{
			name:    "missing hub endpoint",
			mutate:  func(c *Config) { c.HubEndpoint = "" },
			wantErr: "hub-endpoint must not be empty",
		},
		{
			name:    "missing cell name",
			mutate:  func(c *Config) { c.CellName = "" },
			wantErr: "cell-name must not be empty",
		},
		{
			name:    "zero heartbeat interval",
			mutate:  func(c *Config) { c.HeartbeatInterval = 0 },
			wantErr: "heartbeat-interval must be positive",
		},
		{
			name:    "negative heartbeat interval",
			mutate:  func(c *Config) { c.HeartbeatInterval = -5 * time.Second },
			wantErr: "heartbeat-interval must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
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

// TestDefaultConfigIsNotSelfSufficient documents that the agent refuses to
// start without being told which cell it reports for. An agent that guessed
// would be an agent that could report capacity for someone else's cell.
func TestDefaultConfigIsNotSelfSufficient(t *testing.T) {
	if err := DefaultConfig().Validate(); err == nil {
		t.Fatal("DefaultConfig().Validate() = nil, want an error: hub endpoint and cell name have no safe default")
	}
}

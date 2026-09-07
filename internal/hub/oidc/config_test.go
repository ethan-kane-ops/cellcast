package oidc

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		cfg := DefaultConfig()
		cfg.Audience = testAudience
		cfg.Issuers = []IssuerConfig{{
			Issuer:   "https://token.actions.githubusercontent.com",
			Provider: "github",
		}}
		return cfg
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "a configured issuer and audience is valid",
			mutate: func(*Config) {},
		},
		{
			name:    "no issuers",
			mutate:  func(c *Config) { c.Issuers = nil },
			wantErr: "at least one trusted issuer",
		},
		{
			name:    "no audience",
			mutate:  func(c *Config) { c.Audience = "" },
			wantErr: "audience must be set",
		},
		{
			name:    "plaintext issuer",
			mutate:  func(c *Config) { c.Issuers[0].Issuer = "http://token.actions.githubusercontent.com" },
			wantErr: "must use https",
		},
		{
			name: "issuer with a query string",
			mutate: func(c *Config) {
				c.Issuers[0].Issuer = "https://evil.test/?x=https://token.actions.githubusercontent.com"
			},
			wantErr: "query string",
		},
		{
			name:    "unknown provider",
			mutate:  func(c *Config) { c.Issuers[0].Provider = "jenkins" },
			wantErr: "unknown provider",
		},
		{
			name: "duplicate issuer",
			mutate: func(c *Config) {
				c.Issuers = append(c.Issuers, c.Issuers[0])
			},
			wantErr: "more than once",
		},
		{
			name:    "negative clock skew",
			mutate:  func(c *Config) { c.ClockSkew = -time.Second },
			wantErr: "must not be negative",
		},
		{
			name:    "clock skew wide enough to matter",
			mutate:  func(c *Config) { c.ClockSkew = time.Hour },
			wantErr: "must not exceed 5m",
		},
		{
			name:    "zero refresh interval",
			mutate:  func(c *Config) { c.RefreshInterval = 0 },
			wantErr: "refresh-interval must be positive",
		},
		{
			name:    "zero http timeout",
			mutate:  func(c *Config) { c.HTTPTimeout = 0 },
			wantErr: "http-timeout must be positive",
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
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseIssuerFlag(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    IssuerConfig
		wantErr bool
	}{
		{
			name: "url and provider",
			raw:  "https://token.actions.githubusercontent.com=github",
			want: IssuerConfig{Issuer: "https://token.actions.githubusercontent.com", Provider: "github"},
		},
		{
			name: "buildkite",
			raw:  "https://agent.buildkite.com=buildkite",
			want: IssuerConfig{Issuer: "https://agent.buildkite.com", Provider: "buildkite"},
		},
		{
			name:    "no provider",
			raw:     "https://token.actions.githubusercontent.com",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIssuerFlag(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseIssuerFlag(%q) = %+v, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIssuerFlag(%q) = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ParseIssuerFlag(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestSymmetricAlgorithmsAreNotSupported pins the allowlist against the
// algorithm-confusion attack, where a token is signed with HMAC using the
// issuer's public key as the shared secret.
func TestSymmetricAlgorithmsAreNotSupported(t *testing.T) {
	for _, alg := range []string{"HS256", "HS384", "HS512", "none", "None", ""} {
		if supportedAlgorithms[alg] {
			t.Errorf("supportedAlgorithms[%q] is true, want false", alg)
		}
	}
	for _, alg := range []string{"RS256", "ES256", "PS256"} {
		if !supportedAlgorithms[alg] {
			t.Errorf("supportedAlgorithms[%q] is false, want true", alg)
		}
	}
}

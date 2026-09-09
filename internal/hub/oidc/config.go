// Package oidc authenticates pipeline callers by their CI platform's workload
// identity token.
//
// The caller presents the OIDC JWT its platform already issues. cellcast
// verifies it against the issuer's published keys and maps the claims to an
// identity the policy engine can authorize. No shared secret exists anywhere in
// the chain: moving a credential closer to the pipeline does not solve the
// problem, and removing it does.
//
// This package is the trusted computing base for authentication. It is separate
// from internal/hub so that the code deciding who may ask for a credential is
// auditable on its own, without the HTTP server and controller wiring around
// it.
package oidc

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Default bounds. All are conservative: this is the code path an
// unauthenticated attacker reaches first.
const (
	// DefaultClockSkew is how far the hub's clock may disagree with the
	// issuer's before a valid token is rejected. Large enough for ordinary NTP
	// drift, small enough that it does not meaningfully extend a token's life.
	DefaultClockSkew = 60 * time.Second

	// DefaultRefreshInterval is how often a cached key set is refreshed in the
	// background, so a rotation is picked up before a caller trips over it.
	DefaultRefreshInterval = 15 * time.Minute

	// DefaultHTTPTimeout bounds a discovery or JWKS fetch. An issuer that hangs
	// must not hold a request open.
	DefaultHTTPTimeout = 10 * time.Second

	// MaxTokenBytes bounds the token accepted from the Authorization header.
	// Real workload identity tokens are one to three kilobytes; this is room to
	// spare and still refuses a payload sent to make the parser work.
	MaxTokenBytes = 8192
)

// supportedAlgorithms is the signing algorithm allowlist.
//
// Asymmetric only, and checked against the token header before any key is
// fetched. Symmetric algorithms are absent: accepting HS256 next to
// RS256 is the classic algorithm-confusion bug, where an attacker signs a token
// using the issuer's *public* key as an HMAC secret. "none" is absent for the
// obvious reason and is rejected explicitly rather than by omission.
var supportedAlgorithms = map[string]bool{
	"RS256": true,
	"RS384": true,
	"RS512": true,
	"ES256": true,
	"ES384": true,
	"ES512": true,
	"PS256": true,
	"PS384": true,
	"PS512": true,
}

// IssuerConfig is one entry in the issuer allowlist.
type IssuerConfig struct {
	// Issuer is the exact `iss` value the token must carry. It is compared
	// literally, never by prefix or suffix: "https://evil.test/?x=https://
	// token.actions.githubusercontent.com" must not match.
	Issuer string
	// Provider selects claim extraction. See ProviderFor.
	Provider string
}

// Config configures the authenticator.
type Config struct {
	// Issuers is the allowlist. An empty list means no caller can ever
	// authenticate, which is the correct reading of "no issuers configured".
	Issuers []IssuerConfig

	// Audience is the `aud` value bound to this cellcast instance. A token
	// minted for a different audience is a token minted for somebody else, and
	// accepting it is how one service's identity becomes another's.
	Audience string

	// ClockSkew is the tolerance applied to exp, nbf and iat.
	ClockSkew time.Duration

	// RefreshInterval is how often key sets refresh in the background.
	RefreshInterval time.Duration

	// HTTPTimeout bounds discovery and JWKS fetches.
	HTTPTimeout time.Duration

	// CAFile is a PEM bundle trusted when fetching issuer metadata, on top of
	// the system roots. Empty means the system roots alone.
	//
	// A managed platform's issuer chains to a public root and needs nothing
	// here. A self-hosted one usually does not: GitHub Enterprise Server, a
	// self-hosted GitLab and an internal Keycloak commonly present a
	// certificate from the organisation's own CA, and without this the hub
	// cannot fetch their keys and so cannot authenticate anyone they issue for.
	//
	// Additive rather than a replacement, because a hub may trust an internal
	// issuer and a public one at the same time, and narrowing the pool to the
	// private CA would break the public issuer the moment the private one was
	// configured.
	CAFile string
}

// DefaultConfig returns a configuration with the bounds filled in and no
// issuers. It cannot authenticate anyone until issuers and an audience are set.
func DefaultConfig() Config {
	return Config{
		ClockSkew:       DefaultClockSkew,
		RefreshInterval: DefaultRefreshInterval,
		HTTPTimeout:     DefaultHTTPTimeout,
	}
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if len(c.Issuers) == 0 {
		return fmt.Errorf("at least one trusted issuer must be configured")
	}
	if c.Audience == "" {
		return fmt.Errorf("audience must be set; it is what binds a token to this cellcast instance")
	}
	if c.ClockSkew < 0 {
		return fmt.Errorf("clock-skew must not be negative, got %s", c.ClockSkew)
	}
	if c.ClockSkew > 5*time.Minute {
		// A large skew extends the life of every token the hub accepts,
		// including a stolen one.
		return fmt.Errorf("clock-skew must not exceed 5m, got %s", c.ClockSkew)
	}
	if c.RefreshInterval <= 0 {
		return fmt.Errorf("refresh-interval must be positive, got %s", c.RefreshInterval)
	}
	if c.HTTPTimeout <= 0 {
		return fmt.Errorf("http-timeout must be positive, got %s", c.HTTPTimeout)
	}

	seen := make(map[string]bool, len(c.Issuers))
	for _, iss := range c.Issuers {
		if err := validateIssuerURL(iss.Issuer); err != nil {
			return err
		}
		if seen[iss.Issuer] {
			return fmt.Errorf("issuer %q is configured more than once", iss.Issuer)
		}
		seen[iss.Issuer] = true

		if _, ok := providers[iss.Provider]; !ok {
			return fmt.Errorf("issuer %q names unknown provider %q, want one of %s",
				iss.Issuer, iss.Provider, strings.Join(providerNames(), ", "))
		}
	}
	return nil
}

func validateIssuerURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("issuer must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("issuer %q is not a valid URL", raw)
	}
	// https only: discovery over plaintext lets whoever controls the network
	// choose the signing keys, which defeats the entire verification path.
	if u.Scheme != "https" {
		return fmt.Errorf("issuer %q must use https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("issuer %q must include a host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer %q must not carry a query string or fragment", raw)
	}
	return nil
}

// ParseIssuerFlag parses an `--oidc-issuer` value of the form `url=provider`.
func ParseIssuerFlag(raw string) (IssuerConfig, error) {
	issuerURL, provider, ok := strings.Cut(raw, "=")
	if !ok {
		return IssuerConfig{}, fmt.Errorf("issuer %q must be given as url=provider, for example https://token.actions.githubusercontent.com=github", raw)
	}
	return IssuerConfig{Issuer: issuerURL, Provider: provider}, nil
}

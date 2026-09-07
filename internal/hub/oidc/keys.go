package oidc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	coreosoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/sync/singleflight"
)

// keySource holds one allowlisted issuer's verification keys.
type keySource struct {
	issuer   string
	jwksURL  string
	provider Provider
	keys     *coreosoidc.RemoteKeySet
}

// keyRegistry resolves an allowlisted issuer to its verification keys.
//
// Discovery is lazy and cached. Resolving every issuer at startup would couple
// the hub's ability to boot to the availability of every CI platform it trusts:
// a restart during a GitHub outage would then fail every deploy, including the
// ones bound for an issuer that is up. Instead the first caller for an issuer
// pays the discovery cost and everyone after reuses it.
//
// Key rotation is handled by the underlying RemoteKeySet, which refetches the
// JWKS when it sees a key id it does not recognise, rate-limited so that a
// stream of bogus key ids cannot be turned into load on the issuer. A JWKS that
// cannot be fetched produces a verification error; it never degrades to
// accepting an unverified token.
type keyRegistry struct {
	// lifetime bounds the key sets and the refresh loop. It is the process
	// context, not a request context: a RemoteKeySet outlives the request that
	// first created it.
	lifetime context.Context
	// httpClient is kept alongside the context because go-oidc's ClientContext
	// stores it under an unexported key and offers no way to read it back.
	httpClient *http.Client
	log        *slog.Logger
	interval   time.Duration

	allowed map[string]IssuerConfig

	mu      sync.RWMutex
	sources map[string]*keySource

	// group collapses concurrent first-use discovery for the same issuer into
	// one request, so a burst of pipelines starting together does not turn into
	// a burst of identical discovery calls.
	group singleflight.Group
}

func newKeyRegistry(ctx context.Context, cfg Config, httpClient *http.Client, log *slog.Logger) *keyRegistry {
	allowed := make(map[string]IssuerConfig, len(cfg.Issuers))
	for _, iss := range cfg.Issuers {
		allowed[iss.Issuer] = iss
	}

	return &keyRegistry{
		lifetime:   coreosoidc.ClientContext(ctx, httpClient),
		httpClient: httpClient,
		log:        log,
		interval:   cfg.RefreshInterval,
		allowed:    allowed,
		sources:    make(map[string]*keySource, len(cfg.Issuers)),
	}
}

// permitted reports whether issuer is on the allowlist.
//
// Comparison is exact. Matching an issuer by prefix or suffix would let
// "https://token.actions.githubusercontent.com.evil.test" through.
func (r *keyRegistry) permitted(issuer string) (IssuerConfig, bool) {
	cfg, ok := r.allowed[issuer]
	return cfg, ok
}

// sourceFor returns the key source for an allowlisted issuer, discovering it on
// first use.
func (r *keyRegistry) sourceFor(ctx context.Context, issuer string) (*keySource, error) {
	r.mu.RLock()
	src, ok := r.sources[issuer]
	r.mu.RUnlock()
	if ok {
		return src, nil
	}

	// A failed discovery is not cached: a transient outage at the issuer must
	// not lock that issuer out until the hub restarts.
	result, err, _ := r.group.Do(issuer, func() (any, error) {
		r.mu.RLock()
		existing, ok := r.sources[issuer]
		r.mu.RUnlock()
		if ok {
			return existing, nil
		}

		discovered, err := r.discover(ctx, issuer)
		if err != nil {
			return nil, err
		}

		r.mu.Lock()
		r.sources[issuer] = discovered
		r.mu.Unlock()
		return discovered, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*keySource), nil
}

// discover resolves an issuer's OIDC metadata and builds its key set.
func (r *keyRegistry) discover(ctx context.Context, issuer string) (*keySource, error) {
	issCfg, ok := r.permitted(issuer)
	if !ok {
		// Unreachable through Authenticate, which checks the allowlist first.
		// Repeated here so this function cannot become a way around it.
		return nil, fmt.Errorf("issuer %q is not allowlisted", issuer)
	}

	provider, err := ProviderFor(issCfg.Provider)
	if err != nil {
		return nil, err
	}

	jwksURL, err := r.discoverJWKSURL(ctx, issuer)
	if err != nil {
		return nil, err
	}

	r.log.Info("oidc issuer discovered",
		slog.String("issuer", issuer),
		slog.String("jwks_url", jwksURL),
		slog.String("provider", issCfg.Provider),
	)

	return &keySource{
		issuer:   issuer,
		jwksURL:  jwksURL,
		provider: provider,
		keys:     coreosoidc.NewRemoteKeySet(r.lifetime, jwksURL),
	}, nil
}

// discoverJWKSURL fetches the issuer's OIDC metadata and returns its jwks_uri.
func (r *keyRegistry) discoverJWKSURL(ctx context.Context, issuer string) (string, error) {
	// NewProvider requires the `issuer` in the discovery document to equal the
	// URL it was fetched from, so a redirect to an attacker's metadata cannot
	// substitute a different signing authority.
	provider, err := coreosoidc.NewProvider(coreosoidc.ClientContext(ctx, r.httpClient), issuer)
	if err != nil {
		return "", fmt.Errorf("discovering issuer %q: %w", issuer, err)
	}

	var metadata struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return "", fmt.Errorf("reading metadata for issuer %q: %w", issuer, err)
	}
	if err := validateIssuerURL(metadata.JWKSURL); err != nil {
		return "", fmt.Errorf("issuer %q published an unusable jwks_uri: %w", issuer, err)
	}
	return metadata.JWKSURL, nil
}

// refreshLoop re-runs discovery periodically and swaps the key set if the
// issuer has moved its JWKS.
//
// The key set is replaced only when jwks_uri actually changes. Rebuilding it on
// every tick would throw away a warm key cache and turn a rotation the
// RemoteKeySet already handles into an extra fetch on the next request. What
// this loop adds is picking up an issuer that reconfigures its JWKS location,
// and surfacing an issuer that has become unreachable in the log before a
// pipeline discovers it.
func (r *keyRegistry) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshOnce(ctx)
		}
	}
}

func (r *keyRegistry) refreshOnce(ctx context.Context) {
	r.mu.RLock()
	issuers := make([]string, 0, len(r.sources))
	for issuer := range r.sources {
		issuers = append(issuers, issuer)
	}
	r.mu.RUnlock()

	for _, issuer := range issuers {
		jwksURL, err := r.discoverJWKSURL(ctx, issuer)
		if err != nil {
			// Keep serving from the existing key set. An issuer that cannot be
			// re-discovered is not grounds for rejecting tokens signed by keys
			// already known to be that issuer's.
			r.log.Warn("oidc issuer refresh failed, keeping cached keys",
				slog.String("issuer", issuer),
				slog.Any("error", err),
			)
			continue
		}

		r.mu.Lock()
		src, ok := r.sources[issuer]
		if ok && src.jwksURL != jwksURL {
			r.log.Info("oidc issuer moved its jwks",
				slog.String("issuer", issuer),
				slog.String("from", src.jwksURL),
				slog.String("to", jwksURL),
			)
			r.sources[issuer] = &keySource{
				issuer:   src.issuer,
				jwksURL:  jwksURL,
				provider: src.provider,
				keys:     coreosoidc.NewRemoteKeySet(r.lifetime, jwksURL),
			}
		}
		r.mu.Unlock()
	}
}

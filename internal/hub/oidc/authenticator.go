package oidc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// rejection is one authentication failure.
//
// It carries a one-word label alongside the prose because a metric counting
// refusals cannot be labelled by an error string: that string is unbounded and
// partly written by whatever the issuer returned, so the label cardinality
// would be a function of somebody else's error messages.
//
// The hub asserts a RejectionReason() interface rather than importing this
// package, which keeps the seam between the two exactly one interface wide.
type rejection struct {
	reason string
	msg    string
}

func (r *rejection) Error() string           { return r.msg }
func (r *rejection) RejectionReason() string { return r.reason }

func newRejection(reason, msg string) error { return &rejection{reason: reason, msg: msg} }

// Rejection reasons. They are a closed set so that a failing pipeline can be
// diagnosed from a log line or a metric label without a debug build, and so
// that the reason returned to the caller stays coarse while the reason recorded
// stays precise.
var (
	ErrNoToken          = newRejection("NoToken", "no bearer token presented")
	ErrMalformedToken   = newRejection("MalformedToken", "token is not a well-formed JWT")
	ErrTokenTooLarge    = newRejection("TokenTooLarge", "token exceeds the maximum accepted size")
	ErrIssuerNotAllowed = newRejection("IssuerNotAllowed", "token issuer is not allowlisted")
	ErrAlgNotAllowed    = newRejection("AlgNotAllowed", "token signing algorithm is not allowlisted")
	ErrSignature        = newRejection("Signature", "token signature could not be verified")
	ErrAudience         = newRejection("Audience", "token audience does not match this instance")
	ErrExpired          = newRejection("Expired", "token has expired")
	ErrNotYetValid      = newRejection("NotYetValid", "token is not valid yet")
	ErrIssuedInFuture   = newRejection("IssuedInFuture", "token was issued in the future")
	ErrNoSubject        = newRejection("NoSubject", "token carries no subject claim")
)

// Authenticator verifies a caller's workload identity token.
//
// It satisfies hub.Authenticator. Construction does no network I/O; issuer
// metadata is fetched on first use per issuer.
type Authenticator struct {
	cfg        Config
	keys       *keyRegistry
	log        *slog.Logger
	httpClient *http.Client

	// now is overridden in tests to exercise the expiry and clock-skew bounds
	// without sleeping.
	now func() time.Time
}

// compile-time check that this satisfies the seam it exists to fill.
// Option configures an Authenticator.
type Option func(*Authenticator)

// WithHTTPClient replaces the client used for discovery and JWKS fetches.
//
// Needed for an issuer behind a proxy, or one presenting a certificate from an
// internal CA, which is the normal shape of a self-hosted GitLab or Buildkite.
// The client's timeout should still bound the fetch; the configured
// HTTPTimeout is not applied to a client supplied here.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Authenticator) { a.httpClient = c }
}

// New builds an Authenticator.
//
// ctx bounds the lifetime of the cached key sets and the refresh loop; it is
// the process context, not a request context. The refresh loop stops when it is
// cancelled.
func New(ctx context.Context, cfg Config, log *slog.Logger, opts ...Option) (*Authenticator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid oidc config: %w", err)
	}

	a := &Authenticator{cfg: cfg, log: log, now: time.Now}
	for _, opt := range opts {
		opt(a)
	}
	if a.httpClient == nil {
		transport, err := issuerTransport(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		a.httpClient = &http.Client{Timeout: cfg.HTTPTimeout, Transport: transport}
	}

	registry := newKeyRegistry(ctx, cfg, a.httpClient, log)
	go registry.refreshLoop(ctx)

	a.keys = registry
	return a, nil
}

// issuerTransport builds the transport used to fetch issuer metadata.
//
// The returned error is fatal at startup, which is the point: a CA file that
// cannot be read is a hub that will refuse every caller from the issuer it was
// configured for, and finding that out on the first deploy of the day rather
// than at boot is the difference between a failed start and a silent outage.
//
// There is deliberately no path here that skips verification. A flag that
// degrades to InsecureSkipVerify when its file is missing hands issuer
// selection to whoever controls the network, which is exactly what verifying
// the issuer's certificate exists to prevent.
func issuerTransport(caFile string) (http.RoundTripper, error) {
	if caFile == "" {
		return http.DefaultTransport, nil
	}

	// Start from the system roots and add to them, so configuring an internal
	// issuer does not stop a public one alongside it from resolving.
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("loading system certificate pool: %w", err)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading oidc-ca-file: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("oidc-ca-file %s contains no usable certificate", caFile)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return transport, nil
}

// Authenticate verifies the caller's token and returns the identity policy
// authorizes against.
//
// The order of the steps is the security-relevant part. Cheap structural checks
// and the issuer allowlist come first, so an unknown caller is rejected before
// the hub does anything expensive or reaches out to the network on their
// behalf.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*identity.Identity, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return nil, a.reject(ctx, err)
	}

	// Read `iss` from the unverified token. This is the one piece of parsing
	// that necessarily precedes verification: the token names the authority
	// that can verify it, and there is no way to know which keys to use without
	// reading it. It is kept to the minimum that makes that possible, over a
	// length-bounded input, and nothing read here is trusted or retained beyond
	// selecting the issuer. Everything else is read again from the verified
	// payload below.
	unverified, err := peekIssuer(raw)
	if err != nil {
		return nil, a.reject(ctx, err)
	}

	issCfg, ok := a.keys.permitted(unverified.issuer)
	if !ok {
		return nil, a.reject(ctx, fmt.Errorf("%w: %q", ErrIssuerNotAllowed, unverified.issuer))
	}

	if !supportedAlgorithms[unverified.alg] {
		// Checked before any key is fetched, and independently of what the key
		// set will accept. This is what closes off algorithm confusion and
		// `alg: none` rather than relying on the verifier to refuse them.
		return nil, a.reject(ctx, fmt.Errorf("%w: %q", ErrAlgNotAllowed, unverified.alg))
	}

	source, err := a.keys.sourceFor(ctx, issCfg.Issuer)
	if err != nil {
		return nil, a.reject(ctx, err)
	}

	payload, err := source.keys.VerifySignature(ctx, raw)
	if err != nil {
		// Deliberately does not wrap the underlying error into the caller's
		// view: whether a key id was unknown or a fetch failed is operator
		// detail, not something to hand an unauthenticated caller.
		a.log.WarnContext(ctx, "oidc signature verification failed",
			slog.String("issuer", issCfg.Issuer),
			slog.Any("error", err),
		)
		return nil, a.reject(ctx, ErrSignature)
	}

	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, a.reject(ctx, ErrMalformedToken)
	}

	std, err := standardClaims(claims)
	if err != nil {
		return nil, a.reject(ctx, err)
	}

	// Re-check the issuer against the verified payload. The value used to pick
	// the key set came from unverified bytes; this is the one that counts.
	if std.Issuer != issCfg.Issuer {
		return nil, a.reject(ctx, fmt.Errorf("%w: signed payload names %q", ErrIssuerNotAllowed, std.Issuer))
	}
	if err := a.validateTimes(std); err != nil {
		return nil, a.reject(ctx, err)
	}
	if !containsAudience(std.Audience, a.cfg.Audience) {
		return nil, a.reject(ctx, ErrAudience)
	}
	if std.Subject == "" {
		return nil, a.reject(ctx, ErrNoSubject)
	}

	return &identity.Identity{
		Issuer:  std.Issuer,
		Subject: std.Subject,
		Claims:  source.provider.extract(claims),
	}, nil
}

// reject records a rejection and returns the error the middleware turns into a
// 401.
//
// The token itself is never logged, not even a prefix: a token fragment in a
// log is still credential material, and the hub's log is not the place it
// belongs (docs/threat-model.md T-05). The issuer and the reason are enough to
// diagnose a failing pipeline.
func (a *Authenticator) reject(ctx context.Context, reason error) error {
	a.log.WarnContext(ctx, "oidc authentication rejected", slog.String("reason", reason.Error()))
	// Both errors are wrapped. The middleware only asks whether this is
	// identity.ErrUnauthenticated, but a caller distinguishing an expired token from
	// a rejected issuer needs the specific reason to survive too, and that is
	// what makes the rejection table above assert anything.
	return fmt.Errorf("%w: %w", identity.ErrUnauthenticated, reason)
}

// validateTimes enforces exp, nbf and iat with the configured skew.
func (a *Authenticator) validateTimes(std standard) error {
	now := a.now()
	skew := a.cfg.ClockSkew

	if std.Expiry == 0 {
		// A token with no expiry is a bearer credential with no end. Treat the
		// absence as a failure rather than as "never expires".
		return fmt.Errorf("%w: no exp claim", ErrExpired)
	}
	if now.After(time.Unix(std.Expiry, 0).Add(skew)) {
		return ErrExpired
	}
	if std.NotBefore != 0 && now.Before(time.Unix(std.NotBefore, 0).Add(-skew)) {
		return ErrNotYetValid
	}
	if std.IssuedAt != 0 && now.Before(time.Unix(std.IssuedAt, 0).Add(-skew)) {
		return ErrIssuedInFuture
	}
	return nil
}

// standard is the set of registered claims this package enforces.
type standard struct {
	Issuer    string
	Subject   string
	Audience  []string
	Expiry    int64
	NotBefore int64
	IssuedAt  int64
}

func standardClaims(claims map[string]json.RawMessage) (standard, error) {
	var std standard

	if err := decodeClaim(claims, "iss", &std.Issuer); err != nil {
		return std, err
	}
	if err := decodeClaim(claims, "sub", &std.Subject); err != nil {
		return std, err
	}
	if err := decodeClaim(claims, "exp", &std.Expiry); err != nil {
		return std, err
	}
	if err := decodeClaim(claims, "nbf", &std.NotBefore); err != nil {
		return std, err
	}
	if err := decodeClaim(claims, "iat", &std.IssuedAt); err != nil {
		return std, err
	}

	// `aud` is a string or an array of strings per RFC 7519.
	if raw, ok := claims["aud"]; ok {
		var single string
		if err := json.Unmarshal(raw, &single); err == nil {
			std.Audience = []string{single}
		} else if err := json.Unmarshal(raw, &std.Audience); err != nil {
			return std, ErrMalformedToken
		}
	}
	return std, nil
}

func decodeClaim[T any](claims map[string]json.RawMessage, name string, out *T) error {
	raw, ok := claims[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: claim %q has the wrong type", ErrMalformedToken, name)
	}
	return nil
}

// containsAudience reports whether the token was minted for this instance.
func containsAudience(audiences []string, want string) bool {
	for _, got := range audiences {
		if got == want {
			return true
		}
	}
	return false
}

// bearerToken extracts the credential from the Authorization header.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", ErrNoToken
	}

	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", ErrNoToken
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrNoToken
	}
	if len(token) > MaxTokenBytes {
		// Bounded before any decoding, so an oversized body cannot be used to
		// make the parser do work.
		return "", ErrTokenTooLarge
	}
	return token, nil
}

// unverifiedHeader is what can be read from a token before its signature is
// checked. Nothing here is trusted; it only selects how to verify.
type unverifiedHeader struct {
	alg    string
	issuer string
}

// peekIssuer reads the signing algorithm and issuer from an unverified token.
func peekIssuer(raw string) (unverifiedHeader, error) {
	var out unverifiedHeader

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return out, ErrMalformedToken
	}

	var header struct {
		Alg string `json:"alg"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return out, err
	}
	if header.Alg == "" {
		return out, ErrMalformedToken
	}

	var payload struct {
		Issuer string `json:"iss"`
	}
	if err := decodeSegment(parts[1], &payload); err != nil {
		return out, err
	}
	if payload.Issuer == "" {
		return out, fmt.Errorf("%w: no iss claim", ErrIssuerNotAllowed)
	}

	out.alg = header.Alg
	out.issuer = payload.Issuer
	return out, nil
}

func decodeSegment(segment string, out any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return ErrMalformedToken
	}
	if err := json.Unmarshal(decoded, out); err != nil {
		return ErrMalformedToken
	}
	return nil
}

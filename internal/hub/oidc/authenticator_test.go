package oidc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/hub"
)

const testAudience = "https://cellcast.example.test"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testAuthenticator wires an Authenticator to a fixture issuer.
func testAuthenticator(t *testing.T, iss *testIssuer, provider string) *Authenticator {
	t.Helper()

	cfg := DefaultConfig()
	cfg.Audience = testAudience
	cfg.Issuers = []IssuerConfig{{Issuer: iss.URL(), Provider: provider}}

	a, err := New(t.Context(), cfg, discardLogger(), WithHTTPClient(iss.client()))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	return a
}

// validClaims is a token body that should authenticate cleanly.
func validClaims(iss *testIssuer) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":              iss.URL(),
		"sub":              "repo:example/app:ref:refs/heads/main",
		"aud":              testAudience,
		"iat":              now.Unix(),
		"nbf":              now.Unix(),
		"exp":              now.Add(10 * time.Minute).Unix(),
		"repository":       "example/app",
		"ref":              "refs/heads/main",
		"environment":      "production",
		"job_workflow_ref": "example/app/.github/workflows/deploy.yml@refs/heads/main",
	}
}

func requestWithToken(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAuthenticateAcceptsAValidToken(t *testing.T) {
	iss := newTestIssuer(t)
	a := testAuthenticator(t, iss, "github")

	id, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss))))
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil", err)
	}

	if id.Issuer != iss.URL() {
		t.Errorf("Issuer = %q, want %q", id.Issuer, iss.URL())
	}
	if want := "repo:example/app:ref:refs/heads/main"; id.Subject != want {
		t.Errorf("Subject = %q, want %q", id.Subject, want)
	}
	for claim, want := range map[string]string{
		"repository":       "example/app",
		"ref":              "refs/heads/main",
		"environment":      "production",
		"job_workflow_ref": "example/app/.github/workflows/deploy.yml@refs/heads/main",
	} {
		if got := id.Claims[claim]; got != want {
			t.Errorf("Claims[%q] = %q, want %q", claim, got, want)
		}
	}
}

// TestAuthenticateRejects is the adversarial table. Every row is a token the
// hub must refuse; a regression in any of them turns cellcast into a service
// that mints cluster credentials for whoever asks.
func TestAuthenticateRejects(t *testing.T) {
	tests := []struct {
		name    string
		token   func(t *testing.T, iss *testIssuer) string
		wantErr error
	}{
		{
			name:    "no token at all",
			token:   func(*testing.T, *testIssuer) string { return "" },
			wantErr: ErrNoToken,
		},
		{
			name:    "not a jwt",
			token:   func(*testing.T, *testIssuer) string { return "not-a-token" },
			wantErr: ErrMalformedToken,
		},
		{
			name: "oversized token",
			token: func(*testing.T, *testIssuer) string {
				return strings.Repeat("a", MaxTokenBytes+1)
			},
			wantErr: ErrTokenTooLarge,
		},
		{
			name: "alg none with no signature",
			token: func(t *testing.T, iss *testIssuer) string {
				return signToken(t, signingKey{kid: "key-1"}, "none", validClaims(iss))
			},
			wantErr: ErrAlgNotAllowed,
		},
		{
			name: "symmetric alg claimed in the header",
			token: func(t *testing.T, iss *testIssuer) string {
				key := newSigningKey(t, "key-1")
				return signToken(t, key, "HS256", validClaims(iss))
			},
			wantErr: ErrAlgNotAllowed,
		},
		{
			name: "issuer not on the allowlist",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				claims["iss"] = "https://evil.example.test"
				return iss.sign(t, claims)
			},
			wantErr: ErrIssuerNotAllowed,
		},
		{
			name: "allowlisted issuer as a prefix of an attacker host",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				claims["iss"] = iss.URL() + ".evil.example.test"
				return iss.sign(t, claims)
			},
			wantErr: ErrIssuerNotAllowed,
		},
		{
			name: "no issuer claim",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				delete(claims, "iss")
				return iss.sign(t, claims)
			},
			wantErr: ErrIssuerNotAllowed,
		},
		{
			name: "signed by a key the issuer does not publish",
			token: func(t *testing.T, iss *testIssuer) string {
				return iss.signWith(t, newSigningKey(t, "key-1"), validClaims(iss))
			},
			wantErr: ErrSignature,
		},
		{
			name: "signature does not match the payload",
			token: func(t *testing.T, iss *testIssuer) string {
				token := iss.sign(t, validClaims(iss))
				tampered := validClaims(iss)
				tampered["sub"] = "repo:attacker/app:ref:refs/heads/main"
				parts := strings.Split(token, ".")
				return parts[0] + "." + encodeTestSegment(t, tampered) + "." + parts[2]
			},
			wantErr: ErrSignature,
		},
		{
			name: "expired",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				claims["exp"] = time.Now().Add(-10 * time.Minute).Unix()
				return iss.sign(t, claims)
			},
			wantErr: ErrExpired,
		},
		{
			name: "no expiry claim",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				delete(claims, "exp")
				return iss.sign(t, claims)
			},
			wantErr: ErrExpired,
		},
		{
			name: "not valid yet",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				claims["nbf"] = time.Now().Add(10 * time.Minute).Unix()
				return iss.sign(t, claims)
			},
			wantErr: ErrNotYetValid,
		},
		{
			name: "issued in the future",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				claims["iat"] = time.Now().Add(10 * time.Minute).Unix()
				delete(claims, "nbf")
				return iss.sign(t, claims)
			},
			wantErr: ErrIssuedInFuture,
		},
		{
			name: "audience belongs to another service",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				claims["aud"] = "https://other.example.test"
				return iss.sign(t, claims)
			},
			wantErr: ErrAudience,
		},
		{
			name: "no audience claim",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				delete(claims, "aud")
				return iss.sign(t, claims)
			},
			wantErr: ErrAudience,
		},
		{
			name: "no subject claim",
			token: func(t *testing.T, iss *testIssuer) string {
				claims := validClaims(iss)
				delete(claims, "sub")
				return iss.sign(t, claims)
			},
			wantErr: ErrNoSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iss := newTestIssuer(t)
			a := testAuthenticator(t, iss, "github")

			id, err := a.Authenticate(t.Context(), requestWithToken(tt.token(t, iss)))
			if err == nil {
				t.Fatalf("Authenticate() = %+v, nil error; want %v", id, tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Authenticate() = %v, want %v", err, tt.wantErr)
			}
			// Every rejection must be unauthenticated as far as the middleware
			// is concerned, so no failure mode can be mistaken for anonymous
			// access.
			if !errors.Is(err, hub.ErrUnauthenticated) {
				t.Errorf("Authenticate() error does not wrap hub.ErrUnauthenticated: %v", err)
			}
		})
	}
}

// TestAuthenticateAcceptsRotatedKey covers the rotation the ticket calls out:
// the issuer swaps its signing key and publishes the new one, and a token
// signed with it verifies without restarting the hub.
func TestAuthenticateAcceptsRotatedKey(t *testing.T) {
	iss := newTestIssuer(t)
	a := testAuthenticator(t, iss, "github")

	if _, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss)))); err != nil {
		t.Fatalf("Authenticate() before rotation = %v, want nil", err)
	}

	iss.rotate(t, "key-2")

	if _, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss)))); err != nil {
		t.Fatalf("Authenticate() after rotation = %v, want nil", err)
	}
}

// TestAuthenticateRejectsAfterKeyIsWithdrawn asserts that rotation does not
// leave the old key valid forever. A key the issuer no longer publishes must
// stop working.
func TestAuthenticateRejectsAfterKeyIsWithdrawn(t *testing.T) {
	iss := newTestIssuer(t)
	a := testAuthenticator(t, iss, "github")

	retired := iss.signing
	if _, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss)))); err != nil {
		t.Fatalf("Authenticate() before rotation = %v, want nil", err)
	}

	iss.rotate(t, "key-2")
	// Warm the new key so the failure below cannot be a stale cache.
	if _, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss)))); err != nil {
		t.Fatalf("Authenticate() after rotation = %v, want nil", err)
	}

	token := iss.signWith(t, retired, validClaims(iss))
	if _, err := a.Authenticate(t.Context(), requestWithToken(token)); !errors.Is(err, ErrSignature) {
		t.Fatalf("Authenticate() with a withdrawn key = %v, want %v", err, ErrSignature)
	}
}

// TestJWKSOutageDoesNotAcceptUnverifiedTokens is the failure behaviour named in
// docs/architecture.md: serve from cache, never fall back to accepting a token
// that has not been verified.
func TestJWKSOutageDoesNotAcceptUnverifiedTokens(t *testing.T) {
	iss := newTestIssuer(t)
	a := testAuthenticator(t, iss, "github")

	// Warm the key set while the issuer is up.
	if _, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss)))); err != nil {
		t.Fatalf("Authenticate() while up = %v, want nil", err)
	}

	known := iss.signing
	iss.server.Close()

	t.Run("cached key still verifies", func(t *testing.T) {
		token := iss.signWith(t, known, validClaims(iss))
		if _, err := a.Authenticate(t.Context(), requestWithToken(token)); err != nil {
			t.Errorf("Authenticate() with a cached key = %v, want nil", err)
		}
	})

	t.Run("unknown key is refused, not waved through", func(t *testing.T) {
		token := iss.signWith(t, newSigningKey(t, "key-99"), validClaims(iss))
		if _, err := a.Authenticate(t.Context(), requestWithToken(token)); !errors.Is(err, ErrSignature) {
			t.Errorf("Authenticate() with an unknown key during an outage = %v, want %v", err, ErrSignature)
		}
	})
}

// TestUnreachableIssuerIsNotCachedAsAFailure asserts a transient outage at
// first use does not lock an issuer out until the process restarts.
func TestUnreachableIssuerIsNotCachedAsAFailure(t *testing.T) {
	iss := newTestIssuer(t)
	a := testAuthenticator(t, iss, "github")

	// Point the registry at a dead address for the first attempt by closing the
	// server before any discovery has happened.
	url := iss.URL()
	iss.server.Close()

	if _, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, validClaims(iss)))); err == nil {
		t.Fatal("Authenticate() against a dead issuer = nil error, want a failure")
	}

	a.keys.mu.RLock()
	_, cached := a.keys.sources[url]
	a.keys.mu.RUnlock()
	if cached {
		t.Error("a failed discovery was cached; a transient outage would lock the issuer out until restart")
	}
}

func TestClockSkewIsBounded(t *testing.T) {
	iss := newTestIssuer(t)

	tests := []struct {
		name    string
		expiry  time.Duration
		wantErr error
	}{
		{name: "just inside the skew window", expiry: -30 * time.Second},
		{name: "just outside the skew window", expiry: -90 * time.Second, wantErr: ErrExpired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAuthenticator(t, iss, "github")
			a.cfg.ClockSkew = time.Minute

			claims := validClaims(iss)
			claims["exp"] = time.Now().Add(tt.expiry).Unix()

			_, err := a.Authenticate(t.Context(), requestWithToken(iss.sign(t, claims)))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Authenticate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Authenticate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		want    string
		wantErr error
	}{
		{name: "standard", header: "Bearer abc.def.ghi", want: "abc.def.ghi"},
		{name: "scheme is case insensitive", header: "bearer abc.def.ghi", want: "abc.def.ghi"},
		{name: "missing header", header: "", wantErr: ErrNoToken},
		{name: "wrong scheme", header: "Basic dXNlcjpwYXNz", wantErr: ErrNoToken},
		{name: "scheme with no token", header: "Bearer", wantErr: ErrNoToken},
		{name: "empty token", header: "Bearer   ", wantErr: ErrNoToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}

			got, err := bearerToken(r)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("bearerToken() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("bearerToken() = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("bearerToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRejectionNeverLogsTokenMaterial pins docs/threat-model.md T-05: the hub
// log is not a place a credential may appear, not even truncated.
func TestRejectionNeverLogsTokenMaterial(t *testing.T) {
	iss := newTestIssuer(t)

	var sink strings.Builder
	cfg := DefaultConfig()
	cfg.Audience = testAudience
	cfg.Issuers = []IssuerConfig{{Issuer: iss.URL(), Provider: "github"}}

	a, err := New(t.Context(), cfg, slog.New(slog.NewTextHandler(&sink, nil)), WithHTTPClient(iss.client()))
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	claims := validClaims(iss)
	claims["aud"] = "https://other.example.test"
	token := iss.sign(t, claims)

	if _, err := a.Authenticate(t.Context(), requestWithToken(token)); err == nil {
		t.Fatal("Authenticate() = nil error, want a rejection")
	}

	logged := sink.String()
	if logged == "" {
		t.Fatal("nothing was logged; a rejection must be diagnosable")
	}
	for _, segment := range strings.Split(token, ".") {
		if len(segment) >= 16 && strings.Contains(logged, segment[:16]) {
			t.Errorf("log contains token material: %s", logged)
		}
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	if _, err := New(context.Background(), DefaultConfig(), discardLogger()); err == nil {
		t.Fatal("New() with no issuers = nil error, want a failure")
	}
}

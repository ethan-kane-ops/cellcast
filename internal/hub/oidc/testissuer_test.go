package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// testIssuer is a stand-in for a CI platform's OIDC issuer.
//
// It serves a real discovery document and a real JWKS over TLS, and signs real
// RS256 tokens, so the tests exercise the same code path a GitHub Actions token
// would, without needing a live CI job to produce one.
type testIssuer struct {
	server *httptest.Server

	mu      sync.Mutex
	signing signingKey
	// published is the key set the JWKS endpoint serves. It is separate from
	// the signing key so a rotation can be staged: sign with a key that has not
	// been published yet, or publish one that is no longer used to sign.
	published []signingKey
}

type signingKey struct {
	kid string
	key *rsa.PrivateKey
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()

	first := newSigningKey(t, "key-1")
	iss := &testIssuer{signing: first, published: []signingKey{first}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]string{
			"issuer":   iss.URL(),
			"jwks_uri": iss.URL() + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		iss.mu.Lock()
		defer iss.mu.Unlock()
		writeTestJSON(w, map[string]any{"keys": jwksFor(iss.published)})
	})

	iss.server = httptest.NewTLSServer(mux)
	t.Cleanup(iss.server.Close)
	return iss
}

func newSigningKey(t *testing.T, kid string) signingKey {
	t.Helper()
	// 2048 is the smallest size the verifier will accept and keeps the tests
	// fast; nothing here protects anything real.
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating signing key: %v", err)
	}
	return signingKey{kid: kid, key: k}
}

func (i *testIssuer) URL() string { return i.server.URL }

// client returns an HTTP client that trusts the fixture's certificate.
func (i *testIssuer) client() *http.Client { return i.server.Client() }

// rotate starts signing with a new key and publishes only that key, which is
// what a real issuer looks like after a rotation has fully rolled through.
func (i *testIssuer) rotate(t *testing.T, kid string) {
	t.Helper()
	i.mu.Lock()
	defer i.mu.Unlock()
	next := newSigningKey(t, kid)
	i.signing = next
	i.published = []signingKey{next}
}

// signWith signs claims using a key that is not in the published
// JWKS, standing in for a token signed by an authority the issuer disowns.
func (i *testIssuer) signWith(t *testing.T, key signingKey, claims map[string]any) string {
	t.Helper()
	return signToken(t, key, "RS256", claims)
}

// sign produces a token signed by the issuer's current key.
func (i *testIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	i.mu.Lock()
	key := i.signing
	i.mu.Unlock()
	return signToken(t, key, "RS256", claims)
}

// signToken builds a JWT by hand.
//
// Hand-rolled rather than built with a library so that the adversarial cases
// this ticket has to cover, `alg: none` and a detached or corrupted signature,
// are as easy to construct as a valid token.
func signToken(t *testing.T, key signingKey, alg string, claims map[string]any) string {
	t.Helper()

	header := map[string]string{"alg": alg, "typ": "JWT"}
	if key.kid != "" {
		header["kid"] = key.kid
	}
	signingInput := encodeTestSegment(t, header) + "." + encodeTestSegment(t, claims)

	if alg == "none" {
		return signingInput + "."
	}

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func encodeTestSegment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding token segment: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func jwksFor(keys []signingKey) []map[string]string {
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		exponent := make([]byte, 8)
		binary.BigEndian.PutUint64(exponent, uint64(k.key.E))
		out = append(out, map[string]string{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": k.kid,
			"n":   base64.RawURLEncoding.EncodeToString(k.key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(trimLeadingZeros(exponent)),
		})
	}
	return out
}

func trimLeadingZeros(b []byte) []byte {
	return new(big.Int).SetBytes(b).Bytes()
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

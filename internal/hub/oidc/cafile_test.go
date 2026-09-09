package oidc

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIssuerCA writes the fixture issuer's certificate out as a PEM bundle,
// which is what an operator running a self-hosted issuer would be handed.
func writeIssuerCA(t *testing.T, iss *testIssuer) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "issuer-ca")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: iss.server.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing issuer CA: %v", err)
	}
	return path
}

// TestCAFileTrustsAnIssuerTheSystemRootsReject is the whole point of the
// option, and it is written as a pair on purpose.
//
// The fixture issuer serves a certificate no public root signed, which is the
// shape of a self-hosted GitLab or a GitHub Enterprise Server. Without the
// bundle the hub cannot fetch its keys and authenticates nobody; with it the
// same token verifies. Asserting only the second half would pass just as well
// if the system roots had happened to accept the certificate all along.
func TestCAFileTrustsAnIssuerTheSystemRootsReject(t *testing.T) {
	t.Parallel()

	iss := newTestIssuer(t)
	token := iss.sign(t, validClaims(iss))

	base := DefaultConfig()
	base.Audience = testAudience
	base.Issuers = []IssuerConfig{{Issuer: iss.URL(), Provider: "github"}}

	t.Run("without the bundle the issuer cannot be reached", func(t *testing.T) {
		t.Parallel()

		a, err := New(t.Context(), base, discardLogger())
		if err != nil {
			t.Fatalf("New() = %v, want nil", err)
		}
		if _, err := a.Authenticate(t.Context(), requestWithToken(token)); err == nil {
			t.Fatal("Authenticate() = nil, want an error: the issuer certificate chains to no public root")
		}
	})

	t.Run("with the bundle the same token verifies", func(t *testing.T) {
		t.Parallel()

		cfg := base
		cfg.CAFile = writeIssuerCA(t, iss)

		a, err := New(t.Context(), cfg, discardLogger())
		if err != nil {
			t.Fatalf("New() = %v, want nil", err)
		}
		id, err := a.Authenticate(t.Context(), requestWithToken(token))
		if err != nil {
			t.Fatalf("Authenticate() = %v, want nil", err)
		}
		if id.Subject != "repo:example/app:ref:refs/heads/main" {
			t.Errorf("Subject = %q, want the token's sub claim", id.Subject)
		}
	})
}

// TestCAFileIsAddedToTheSystemRoots pins the pool being additive.
//
// An operator trusting an internal issuer must not lose the public issuer next
// to it, and replacing the pool rather than appending to it is the easy way to
// write that bug: every test covering the internal issuer still passes, and
// the public one breaks only in production.
//
// Compared by pool identity rather than by counting subjects, because
// x509.CertPool.Subjects() is deprecated and returns nothing for a
// system-derived pool on macOS.
func TestCAFileIsAddedToTheSystemRoots(t *testing.T) {
	t.Parallel()

	iss := newTestIssuer(t)
	caFile := writeIssuerCA(t, iss)

	system, err := x509.SystemCertPool()
	if err != nil {
		t.Skipf("system certificate pool unavailable: %v", err)
	}
	if system.Equal(x509.NewCertPool()) {
		t.Skip("no system roots on this machine; there is nothing to preserve")
	}

	bundle, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	certOnly := x509.NewCertPool()
	if !certOnly.AppendCertsFromPEM(bundle) {
		t.Fatal("fixture bundle holds no certificate")
	}

	transport, err := issuerTransport(caFile)
	if err != nil {
		t.Fatalf("issuerTransport() = %v, want nil", err)
	}
	got := transport.(*http.Transport).TLSClientConfig.RootCAs

	if got.Equal(certOnly) {
		t.Error("the pool holds only the configured bundle; it replaced the system roots instead of adding to them")
	}
	if got.Equal(system) {
		t.Error("the pool holds only the system roots; the configured bundle was not added")
	}
}

// TestCAFileIsRefusedWhenUnusable checks the failure lands at startup.
//
// A hub that starts with a broken bundle is a hub that refuses every caller
// from the issuer it was configured for, and discovers it on the first deploy
// rather than at boot.
func TestCAFileIsRefusedWhenUnusable(t *testing.T) {
	t.Parallel()

	notACert := filepath.Join(t.TempDir(), "garbage")
	if err := os.WriteFile(notACert, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	tests := []struct {
		name    string
		caFile  string
		wantErr string
	}{
		{
			name:    "missing file",
			caFile:  filepath.Join(t.TempDir(), "absent"),
			wantErr: "reading oidc-ca-file",
		},
		{
			name:    "no certificate in the file",
			caFile:  notACert,
			wantErr: "contains no usable certificate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			cfg.Audience = testAudience
			cfg.Issuers = []IssuerConfig{{Issuer: "https://issuer.example.test", Provider: "generic"}}
			cfg.CAFile = tt.caFile

			_, err := New(t.Context(), cfg, discardLogger())
			if err == nil {
				t.Fatal("New() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestNoCAFileLeavesTheDefaultTransport keeps the option free of cost when it
// is not set, which is the case for every managed CI platform.
func TestNoCAFileLeavesTheDefaultTransport(t *testing.T) {
	t.Parallel()

	transport, err := issuerTransport("")
	if err != nil {
		t.Fatalf("issuerTransport() = %v, want nil", err)
	}
	if transport != http.DefaultTransport {
		t.Error("issuerTransport(\"\") built a transport; an unset bundle must leave the default alone")
	}
}

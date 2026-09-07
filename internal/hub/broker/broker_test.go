package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

const testNamespace = "cellcast-system"

// theToken is a distinctive string so a leak into any output is unmistakable.
const theToken = "SENTINEL-TOKEN-VALUE-b7c1e9"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := cellcastv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("registering cellcast scheme: %v", err)
	}
	return s
}

func testCluster(name string, labels map[string]string) *cellcastv1alpha1.Cluster {
	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       "https://" + name + ".example.test",
			Provider:       cellcastv1alpha1.ProviderGeneric,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "cell-trust"},
		},
	}
}

func testTrust(name string) *cellcastv1alpha1.TrustConfig {
	return &cellcastv1alpha1.TrustConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: cellcastv1alpha1.TrustConfigSpec{
			Provider:         cellcastv1alpha1.TrustProviderKubernetes,
			CredentialSource: cellcastv1alpha1.CredentialSource{InCluster: true},
			Kubernetes: &cellcastv1alpha1.KubernetesTrust{
				ServiceAccountName: "deployer",
				Namespace:          "apps",
			},
		},
	}
}

// stubProvider mints without a cluster behind it.
type stubProvider struct {
	minTTL time.Duration
	// lifetime overrides how long the issued credential claims to last, so the
	// broker's own bound check can be exercised.
	lifetime time.Duration
	gotTTL   time.Duration
	err      error
}

func (p *stubProvider) Kind() cellcastv1alpha1.TrustProvider {
	return cellcastv1alpha1.TrustProviderKubernetes
}

func (p *stubProvider) MinTTL() time.Duration { return p.minTTL }

func (p *stubProvider) Mint(_ context.Context, req MintRequest) (*Credential, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.gotTTL = req.TTL

	lifetime := p.lifetime
	if lifetime == 0 {
		lifetime = req.TTL
	}
	return &Credential{
		Token:          theToken,
		ExpiresAt:      time.Now().Add(lifetime),
		Server:         req.Cluster.Spec.Endpoint,
		Namespace:      req.Trust.Spec.Kubernetes.Namespace,
		ServiceAccount: req.Trust.Spec.Kubernetes.ServiceAccountName,
	}, nil
}

func newBroker(t *testing.T, p Provider, objs ...client.Object) (*Broker, *bytes.Buffer) {
	t.Helper()
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return New(k8s, testNamespace, time.Hour, log, p), &logs
}

func TestMint(t *testing.T) {
	cluster := testCluster("prod-euw1", map[string]string{"env": "prod"})
	provider := &stubProvider{minTTL: KubernetesMinTTL}
	b, _ := newBroker(t, provider, cluster, testTrust("cell-trust"))

	cred, res, err := b.Mint(t.Context(), cluster, nil, 0, "repo:acme/app")
	if err != nil {
		t.Fatalf("Mint() = %v, want nil", err)
	}
	if cred.Token != theToken {
		t.Errorf("Token = %q, want the minted token", cred.Token)
	}
	if res.Granted != 10*time.Minute {
		t.Errorf("Granted = %s, want the production default of 10m", res.Granted)
	}
	if provider.gotTTL != res.Granted {
		t.Errorf("provider received %s, want the resolved %s", provider.gotTTL, res.Granted)
	}
	if cred.Server != cluster.Spec.Endpoint {
		t.Errorf("Server = %q, want the cell's registered endpoint %q", cred.Server, cluster.Spec.Endpoint)
	}
}

func TestMintRefusals(t *testing.T) {
	awsTrust := testTrust("cell-trust")
	awsTrust.Spec.Provider = cellcastv1alpha1.TrustProviderAWS

	brokenTrust := testTrust("cell-trust")
	brokenTrust.Spec.Kubernetes = nil

	tests := []struct {
		name    string
		objs    []client.Object
		want    error
		wantMsg string
	}{
		{
			name: "the cell names a trust config that does not exist",
			objs: nil,
			want: ErrTrustConfigMissing,
		},
		{
			name: "the trust config names a provider this build cannot use",
			objs: []client.Object{awsTrust},
			want: ErrProviderNotImplemented,
		},
		{
			name: "the trust config is internally inconsistent",
			objs: []client.Object{brokenTrust},
			want: ErrTrustConfigInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := testCluster("prod-euw1", map[string]string{"env": "prod"})
			objs := append([]client.Object{cluster}, tt.objs...)
			b, _ := newBroker(t, &stubProvider{minTTL: KubernetesMinTTL}, objs...)

			_, _, err := b.Mint(t.Context(), cluster, nil, 0, "repo:acme/app")
			if !errors.Is(err, tt.want) {
				t.Fatalf("Mint() = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestMintRefusesACredentialLongerThanTheCeiling covers the case where the
// issuing authority ignores what it was asked for.
//
// A Kubernetes API server applies no maximum of its own unless one is
// configured, so the ceiling has to be enforced against the token that came
// back, not only against the number that went out. A bound checked on the way
// in is not a bound.
func TestMintRefusesACredentialLongerThanTheCeiling(t *testing.T) {
	cluster := testCluster("prod-euw1", map[string]string{"env": "prod"})
	provider := &stubProvider{minTTL: KubernetesMinTTL, lifetime: 30 * 24 * time.Hour}
	b, _ := newBroker(t, provider, cluster, testTrust("cell-trust"))

	cred, _, err := b.Mint(t.Context(), cluster, nil, 0, "repo:acme/app")
	if err == nil {
		t.Fatalf("Mint() = %v, want an error for an over-long credential", cred)
	}
	if cred != nil {
		t.Errorf("Mint() returned a credential alongside the error; it must be discarded")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("error = %q, want it to name the ceiling", err)
	}
}

// TestCredentialNeverPrintsItsToken covers the most likely real-world leak in
// the system: credential material reaching build output by accident rather than
// by attack (docs/threat-model.md T-05).
//
// The rule "do not log tokens" cannot be enforced by review alone, so the type
// is built so that the obvious mistakes are safe. Every path here is one
// somebody actually writes while debugging.
func TestCredentialNeverPrintsItsToken(t *testing.T) {
	cred := Credential{
		Token:          theToken,
		ExpiresAt:      time.Now().Add(10 * time.Minute),
		Server:         "https://prod-euw1.example.test",
		Namespace:      "apps",
		ServiceAccount: "deployer",
	}

	renders := map[string]string{
		// %v on the value and on a pointer: a String method declared on the
		// wrong receiver would redact one and leak the other.
		"%v":            fmt.Sprintf("%v", cred),
		"%v on pointer": fmt.Sprintf("%v", &cred),
		"String()":      cred.String(),
		"error wrap":    fmt.Errorf("minting failed: %v", cred).Error(),
	}
	for name, got := range renders {
		if strings.Contains(got, theToken) {
			t.Errorf("%s leaked the token: %s", name, got)
		}
		if !strings.Contains(got, "redacted") {
			t.Errorf("%s = %q, want it to say the token was redacted", name, got)
		}
	}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("minted", slog.Any("credential", cred))
	if strings.Contains(logs.String(), theToken) {
		t.Errorf("slog.Any leaked the token: %s", logs.String())
	}
}

// TestMintLogsNoTokenMaterial asserts the broker's own success path stays
// quiet, since that is the one log line guaranteed to run on every mint.
func TestMintLogsNoTokenMaterial(t *testing.T) {
	cluster := testCluster("prod-euw1", map[string]string{"env": "prod"})
	b, logs := newBroker(t, &stubProvider{minTTL: KubernetesMinTTL}, cluster, testTrust("cell-trust"))

	if _, _, err := b.Mint(t.Context(), cluster, nil, 0, "repo:acme/app"); err != nil {
		t.Fatalf("Mint() = %v, want nil", err)
	}

	out := logs.String()
	if out == "" {
		t.Fatal("Mint() logged nothing; the mint record is what makes a compromise reconstructable")
	}
	if strings.Contains(out, theToken) {
		t.Errorf("mint log leaked the token: %s", out)
	}
	// A prefix is enough to correlate a leaked token back to a mint, which is
	// why the threat model bans prefixes too, not just whole tokens.
	if strings.Contains(out, theToken[:8]) {
		t.Errorf("mint log leaked a token prefix: %s", out)
	}
	for _, want := range []string{"prod-euw1", "repo:acme/app"} {
		if !strings.Contains(out, want) {
			t.Errorf("mint log = %s, want it to record %q", out, want)
		}
	}
}

func TestValidateTrust(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*cellcastv1alpha1.TrustConfig)
		wantErr error
	}{
		{
			name:   "in-cluster kubernetes trust is valid",
			mutate: func(*cellcastv1alpha1.TrustConfig) {},
		},
		{
			name: "a secret reference is valid",
			mutate: func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{
					SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig"},
				}
			},
		},
		{
			name: "no credential source at all",
			mutate: func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{}
			},
			wantErr: ErrTrustConfigInvalid,
		},
		{
			name: "both credential sources",
			mutate: func(tc *cellcastv1alpha1.TrustConfig) {
				tc.Spec.CredentialSource.SecretRef = &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig"}
			},
			wantErr: ErrTrustConfigInvalid,
		},
		{
			name:    "the aws provider is not built yet",
			mutate:  func(tc *cellcastv1alpha1.TrustConfig) { tc.Spec.Provider = cellcastv1alpha1.TrustProviderAWS },
			wantErr: ErrProviderNotImplemented,
		},
		{
			name:    "an unknown provider",
			mutate:  func(tc *cellcastv1alpha1.TrustConfig) { tc.Spec.Provider = "nomad" },
			wantErr: ErrTrustConfigInvalid,
		},
		{
			name:    "no service account name",
			mutate:  func(tc *cellcastv1alpha1.TrustConfig) { tc.Spec.Kubernetes.ServiceAccountName = "" },
			wantErr: ErrTrustConfigInvalid,
		},
		{
			name:    "no namespace",
			mutate:  func(tc *cellcastv1alpha1.TrustConfig) { tc.Spec.Kubernetes.Namespace = "" },
			wantErr: ErrTrustConfigInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trust := testTrust("cell-trust")
			tt.mutate(trust)

			err := ValidateTrust(trust)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateTrust() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateTrust() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

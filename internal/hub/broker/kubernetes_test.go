package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// tokenIssuer stands in for a spoke's TokenRequest endpoint.
type tokenIssuer struct {
	// grantedSeconds overrides the lifetime the API server reports, so the
	// difference between what was asked for and what was issued is testable.
	grantedSeconds int64
	saExists       bool

	gotSeconds   int64
	gotAudiences []string
	gotName      string
	gotNamespace string
}

func (ti *tokenIssuer) clientset() kubernetes.Interface {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		// CreateActionImpl rather than the CreateAction interface: the name of
		// the service account a token subresource targets lives on the concrete
		// type.
		ca, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			return false, nil, fmt.Errorf("unexpected action %T", action)
		}
		tr, ok := ca.GetObject().(*authenticationv1.TokenRequest)
		if !ok {
			return false, nil, fmt.Errorf("unexpected object %T", ca.GetObject())
		}

		ti.gotName = ca.Name
		ti.gotNamespace = ca.GetNamespace()
		ti.gotAudiences = tr.Spec.Audiences
		if tr.Spec.ExpirationSeconds != nil {
			ti.gotSeconds = *tr.Spec.ExpirationSeconds
		}

		if !ti.saExists {
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Resource: "serviceaccounts"}, ca.Name)
		}

		granted := ti.grantedSeconds
		if granted == 0 {
			granted = ti.gotSeconds
		}
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{
				Token:               theToken,
				ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(granted) * time.Second)),
			},
		}, nil
	})
	return cs
}

func TestKubernetesProviderMint(t *testing.T) {
	issuer := &tokenIssuer{saExists: true}
	provider := NewKubernetesProvider(
		func(context.Context, *cellcastv1alpha1.Cluster, *cellcastv1alpha1.TrustConfig) (kubernetes.Interface, error) {
			return issuer.clientset(), nil
		})

	trust := testTrust("cell-trust")
	trust.Spec.Kubernetes.Audiences = []string{"https://kubernetes.default.svc"}
	cluster := testCluster("prod-euw1", nil)
	cluster.Spec.CABundle = []byte("ca-material")

	cred, err := provider.Mint(t.Context(), MintRequest{
		Cluster: cluster, Trust: trust, TTL: 12 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Mint() = %v, want nil", err)
	}

	if issuer.gotSeconds != int64((12 * time.Minute).Seconds()) {
		t.Errorf("requested expirationSeconds = %d, want 720", issuer.gotSeconds)
	}
	if issuer.gotName != "deployer" || issuer.gotNamespace != "apps" {
		t.Errorf("minted for %s/%s, want apps/deployer", issuer.gotNamespace, issuer.gotName)
	}
	if len(issuer.gotAudiences) != 1 || issuer.gotAudiences[0] != "https://kubernetes.default.svc" {
		t.Errorf("audiences = %v, want the configured audience", issuer.gotAudiences)
	}
	if cred.Namespace != "apps" || cred.ServiceAccount != "deployer" {
		t.Errorf("credential scoped to %s/%s, want apps/deployer", cred.Namespace, cred.ServiceAccount)
	}
	if string(cred.CABundle) != "ca-material" {
		t.Errorf("CABundle = %q, want the cell's bundle", cred.CABundle)
	}
}

// TestMintReportsTheIssuedExpiryNotTheRequestedOne covers an API server
// configured with --service-account-max-token-expiration, which silently issues
// a shorter token than it was asked for. Reporting the requested lifetime would
// tell a pipeline it has longer than it does.
func TestMintReportsTheIssuedExpiryNotTheRequestedOne(t *testing.T) {
	issuer := &tokenIssuer{saExists: true, grantedSeconds: 600}
	provider := NewKubernetesProvider(
		func(context.Context, *cellcastv1alpha1.Cluster, *cellcastv1alpha1.TrustConfig) (kubernetes.Interface, error) {
			return issuer.clientset(), nil
		})

	cred, err := provider.Mint(t.Context(), MintRequest{
		Cluster: testCluster("prod-euw1", nil), Trust: testTrust("cell-trust"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Mint() = %v, want nil", err)
	}

	if lifetime := time.Until(cred.ExpiresAt); lifetime > 11*time.Minute {
		t.Errorf("ExpiresAt is %s away, want the ~10m the server actually issued", lifetime.Round(time.Second))
	}
}

func TestMintWhenTheServiceAccountIsMissing(t *testing.T) {
	issuer := &tokenIssuer{saExists: false}
	provider := NewKubernetesProvider(
		func(context.Context, *cellcastv1alpha1.Cluster, *cellcastv1alpha1.TrustConfig) (kubernetes.Interface, error) {
			return issuer.clientset(), nil
		})

	_, err := provider.Mint(t.Context(), MintRequest{
		Cluster: testCluster("prod-euw1", nil), Trust: testTrust("cell-trust"), TTL: 15 * time.Minute,
	})
	if err == nil {
		t.Fatal("Mint() = nil, want an error")
	}
	// The message has to name what is missing and where. "not found" alone
	// sends an operator to the wrong cluster.
	for _, want := range []string{"apps/deployer", "prod-euw1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

const spokeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: spoke
  cluster:
    server: https://spoke.example.test
contexts:
- name: spoke
  context:
    cluster: spoke
    user: hub
current-context: spoke
users:
- name: hub
  user:
    token: ` + theToken + `
`

func newConnector(t *testing.T, local *rest.Config, objs ...client.Object) *SecretConnector {
	t.Helper()

	secrets := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	c := NewSecretConnector(secrets, testNamespace, local)
	c.build = func(*rest.Config) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(), nil
	}
	return c
}

func TestSecretConnector(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke-kubeconfig", Namespace: testNamespace},
		Data:       map[string][]byte{"kubeconfig": []byte(spokeKubeconfig)},
	}

	t.Run("in-cluster uses the hub's own config", func(t *testing.T) {
		local := &rest.Config{Host: "https://hub.example.test"}
		c := newConnector(t, local)

		var got *rest.Config
		c.build = func(cfg *rest.Config) (kubernetes.Interface, error) {
			got = cfg
			return fake.NewSimpleClientset(), nil
		}

		if _, err := c.Connect(t.Context(), testCluster("hub-cell", nil), testTrust("cell-trust")); err != nil {
			t.Fatalf("Connect() = %v, want nil", err)
		}
		if got.Host != "https://hub.example.test" {
			t.Errorf("Host = %q, want the hub's own config", got.Host)
		}
		if got.Timeout != mintTimeout {
			t.Errorf("Timeout = %s, want %s", got.Timeout, mintTimeout)
		}
		// Mutating a copy, not the shared config every other client is built
		// from.
		if local.Timeout != 0 {
			t.Errorf("the hub's shared config was mutated; Timeout = %s", local.Timeout)
		}
	})

	t.Run("a secret reference is parsed into a config for the spoke", func(t *testing.T) {
		c := newConnector(t, nil, secret)
		trust := testTrust("cell-trust")
		trust.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{
			SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig"},
		}

		var got *rest.Config
		c.build = func(cfg *rest.Config) (kubernetes.Interface, error) {
			got = cfg
			return fake.NewSimpleClientset(), nil
		}

		if _, err := c.Connect(t.Context(), testCluster("spoke", nil), trust); err != nil {
			t.Fatalf("Connect() = %v, want nil", err)
		}
		if got.Host != "https://spoke.example.test" {
			t.Errorf("Host = %q, want the spoke's server", got.Host)
		}
	})

	refusals := []struct {
		name   string
		objs   []client.Object
		ref    *cellcastv1alpha1.SecretKeyReference
		want   error
		reject string
	}{
		{
			name: "the secret does not exist",
			ref:  &cellcastv1alpha1.SecretKeyReference{Name: "absent"},
			want: ErrCredentialMissing,
		},
		{
			name: "the secret has no such key",
			objs: []client.Object{secret},
			ref:  &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig", Key: "other"},
			want: ErrCredentialMissing,
		},
	}

	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			c := newConnector(t, nil, tt.objs...)
			trust := testTrust("cell-trust")
			trust.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{SecretRef: tt.ref}

			_, err := c.Connect(t.Context(), testCluster("spoke", nil), trust)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Connect() = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestConnectErrorsCarryNoCredentialMaterial covers the path where a spoke
// kubeconfig is malformed.
//
// A parse failure is exactly when somebody is tempted to put the offending
// input in the error, and the offending input here is a credential.
func TestConnectErrorsCarryNoCredentialMaterial(t *testing.T) {
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke-kubeconfig", Namespace: testNamespace},
		Data:       map[string][]byte{"kubeconfig": []byte("this is not: [valid: yaml\ntoken: " + theToken)},
	}

	c := newConnector(t, nil, broken)
	trust := testTrust("cell-trust")
	trust.Spec.CredentialSource = cellcastv1alpha1.CredentialSource{
		SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "spoke-kubeconfig"},
	}

	_, err := c.Connect(t.Context(), testCluster("spoke", nil), trust)
	if err == nil {
		t.Fatal("Connect() = nil, want a parse error")
	}
	if strings.Contains(err.Error(), theToken) {
		t.Errorf("error leaked credential material: %v", err)
	}
}

// TestTheKubernetesProviderRegistersUnderTheKindTheCRDNames covers the wiring
// between the provider and the registry that looks it up.
//
// New keys the registry on Kind(), and Mint resolves a provider from the
// TrustConfig's spec.provider. A Kind() that did not match the enum the CRD
// validates would leave every mint failing with "provider not implemented",
// and nothing outside a live cluster would notice.
func TestTheKubernetesProviderRegistersUnderTheKindTheCRDNames(t *testing.T) {
	provider := NewKubernetesProvider(nil)

	if got := provider.Kind(); got != cellcastv1alpha1.TrustProviderKubernetes {
		t.Errorf("Kind() = %q, want %q", got, cellcastv1alpha1.TrustProviderKubernetes)
	}

	b := New(nil, "cellcast-system", time.Hour, nil, provider)
	if _, ok := b.providers[cellcastv1alpha1.TrustProviderKubernetes]; !ok {
		t.Errorf("a broker built with the Kubernetes provider has no provider for %q",
			cellcastv1alpha1.TrustProviderKubernetes)
	}
}

// TestTheMintingFloorIsTheOneTheAPIServerEnforces keeps the number the broker
// reasons about equal to the one the cluster will actually accept.
//
// Verified against a real API server rather than read from documentation: a
// TokenRequest under 600 seconds is refused outright. A floor set lower here
// would be discovered in the deploy path, by a pipeline asking for five minutes
// and getting an error instead of a credential.
func TestTheMintingFloorIsTheOneTheAPIServerEnforces(t *testing.T) {
	if KubernetesMinTTL != 10*time.Minute {
		t.Errorf("KubernetesMinTTL = %s, want the ten minutes TokenRequest enforces", KubernetesMinTTL)
	}
	if got := NewKubernetesProvider(nil).MinTTL(); got != KubernetesMinTTL {
		t.Errorf("MinTTL() = %s, want %s", got, KubernetesMinTTL)
	}
}

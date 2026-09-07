package broker

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// KubernetesMinTTL is the shortest credential the TokenRequest API will issue.
//
// Verified against a real API server, not read from documentation: anything
// below 600 seconds is rejected outright with "may not specify a duration less
// than 10 minutes". This is why ADR-004's original five-minute production
// default does not exist and could not have.
const KubernetesMinTTL = 10 * time.Minute

// mintTimeout bounds a single minting call.
//
// Minting sits in the deploy critical path, so an unreachable spoke has to fail
// quickly and let the caller act on its declared fallback stance (ENG-175)
// rather than holding the pipeline open until something else times out.
const mintTimeout = 15 * time.Second

// KubernetesProvider mints through a cell's TokenRequest API.
//
// The credential it produces is a service account token, so the permissions the
// caller receives are exactly the permissions of the service account named in
// the trust configuration. cellcast grants nothing of its own here; it asks the
// target cluster to issue an identity that cluster already defined.
type KubernetesProvider struct {
	connect Connector
}

// Connector builds a client for a cell from its trust configuration.
type Connector func(ctx context.Context, cluster *cellcastv1alpha1.Cluster, trust *cellcastv1alpha1.TrustConfig) (kubernetes.Interface, error)

// NewKubernetesProvider builds the TokenRequest provider.
func NewKubernetesProvider(connect Connector) *KubernetesProvider {
	return &KubernetesProvider{connect: connect}
}

// Kind implements Provider.
func (p *KubernetesProvider) Kind() cellcastv1alpha1.TrustProvider {
	return cellcastv1alpha1.TrustProviderKubernetes
}

// MinTTL implements Provider.
func (p *KubernetesProvider) MinTTL() time.Duration { return KubernetesMinTTL }

// Mint issues a service account token scoped to the trust configuration's
// namespace and service account.
func (p *KubernetesProvider) Mint(ctx context.Context, req MintRequest) (*Credential, error) {
	k := req.Trust.Spec.Kubernetes
	if k == nil {
		return nil, fmt.Errorf("%w: %s has no kubernetes block", ErrTrustConfigInvalid, req.Trust.Name)
	}

	cs, err := p.connect(ctx, req.Cluster, req.Trust)
	if err != nil {
		return nil, fmt.Errorf("connecting to cell %s: %w", req.Cluster.Name, err)
	}

	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()

	seconds := int64(req.TTL.Seconds())
	out, err := cs.CoreV1().ServiceAccounts(k.Namespace).CreateToken(ctx, k.ServiceAccountName,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				Audiences:         k.Audiences,
				ExpirationSeconds: &seconds,
			},
		}, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("service account %s/%s does not exist in cell %s: %w",
				k.Namespace, k.ServiceAccountName, req.Cluster.Name, err)
		}
		return nil, fmt.Errorf("requesting a token for %s/%s in cell %s: %w",
			k.Namespace, k.ServiceAccountName, req.Cluster.Name, err)
	}

	// Server is the cell's registered endpoint rather than the address this
	// mint went through. A hub may reach a spoke on a private address the
	// caller cannot resolve, and the caller is the one who has to connect.
	return &Credential{
		Token:          out.Status.Token,
		ExpiresAt:      out.Status.ExpirationTimestamp.Time,
		Server:         req.Cluster.Spec.Endpoint,
		CABundle:       req.Cluster.Spec.CABundle,
		Namespace:      k.Namespace,
		ServiceAccount: k.ServiceAccountName,
	}, nil
}

// SecretConnector builds cell clients from trust configuration.
//
// secrets must be an uncached reader. The hub reads a spoke credential at the
// moment it mints and forgets it again, so a compromised process yields what it
// can fetch while it is running rather than a resident map of every way into
// the estate. A cached client would also require watch on Secrets, which is a
// permission the hub has no reason to hold.
type SecretConnector struct {
	secrets client.Reader
	ns      string
	local   *rest.Config

	// build is injected so tests exercise the configuration path without a
	// cluster behind it.
	build func(*rest.Config) (kubernetes.Interface, error)
}

// NewSecretConnector builds a Connector over the hub's own namespace.
//
// local is the hub's own rest config, used for cells configured inCluster.
func NewSecretConnector(secrets client.Reader, namespace string, local *rest.Config) *SecretConnector {
	return &SecretConnector{
		secrets: secrets,
		ns:      namespace,
		local:   local,
		build:   func(c *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(c) },
	}
}

// Connect implements Connector.
func (c *SecretConnector) Connect(
	ctx context.Context,
	cluster *cellcastv1alpha1.Cluster,
	trust *cellcastv1alpha1.TrustConfig,
) (kubernetes.Interface, error) {
	cfg, err := c.restConfigFor(ctx, trust)
	if err != nil {
		return nil, err
	}

	// Copied before mutation: the hub's own config is shared, and setting a
	// timeout on it would apply to every other client built from it.
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = mintTimeout

	return c.build(cfg)
}

func (c *SecretConnector) restConfigFor(ctx context.Context, trust *cellcastv1alpha1.TrustConfig) (*rest.Config, error) {
	src := trust.Spec.CredentialSource

	if src.InCluster {
		if c.local == nil {
			return nil, fmt.Errorf("%w: %s is configured inCluster but the hub has no local cluster config",
				ErrTrustConfigInvalid, trust.Name)
		}
		return c.local, nil
	}

	if src.SecretRef == nil {
		return nil, fmt.Errorf("%w: %s has neither inCluster nor secretRef", ErrTrustConfigInvalid, trust.Name)
	}

	key := src.SecretRef.Key
	if key == "" {
		key = "kubeconfig"
	}

	var secret corev1.Secret
	ref := client.ObjectKey{Namespace: c.ns, Name: src.SecretRef.Name}
	if err := c.secrets.Get(ctx, ref, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s references secret %q, which does not exist",
				ErrCredentialMissing, trust.Name, src.SecretRef.Name)
		}
		return nil, fmt.Errorf("reading secret %q for trust configuration %s: %w", src.SecretRef.Name, trust.Name, err)
	}

	raw, ok := secret.Data[key]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("%w: secret %q has no key %q", ErrCredentialMissing, src.SecretRef.Name, key)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		// The error from clientcmd is not wrapped with the secret contents on
		// purpose; a parse failure on credential material must not put any of
		// it into a log line.
		return nil, fmt.Errorf("parsing kubeconfig from secret %q for trust configuration %s: %w",
			src.SecretRef.Name, trust.Name, err)
	}
	return cfg, nil
}

// Package broker mints short-lived, scoped credentials for a chosen cell.
//
// This is the only package in cellcast that produces credential material, and
// it is reachable only from the hub binary (docs/architecture.md ADR-007). It
// holds trust configuration, never credentials: nothing here writes a token to
// disk, to a cache, or to a log, and a minted credential is forgotten as soon
// as it has been returned.
package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// Minting outcomes callers distinguish.
var (
	// ErrTrustConfigMissing means the cell names a trust configuration that
	// does not exist. A registration-time mistake that only surfaces at deploy
	// time, which is why the TrustConfig controller reports readiness up front.
	ErrTrustConfigMissing = errors.New("trust configuration not found")

	// ErrProviderNotImplemented means the trust configuration names a provider
	// this build cannot use.
	ErrProviderNotImplemented = errors.New("trust provider is not implemented in this build")

	// ErrTrustConfigInvalid means the configuration is internally inconsistent,
	// for example a kubernetes provider with no kubernetes block.
	ErrTrustConfigInvalid = errors.New("trust configuration is invalid")

	// ErrCredentialMissing means the hub's own way in to the cell is absent:
	// the referenced Secret or the key inside it does not exist. Distinct from
	// an invalid configuration because the fix is different, and because the
	// TrustConfig controller can detect it before a deploy needs it.
	ErrCredentialMissing = errors.New("trust configuration credential is missing")
)

// Credential is a minted, short-lived credential for one cell.
//
// It exists only in flight. The zero value of Token is never written anywhere
// persistent, and the redacting String and LogValue methods below mean an
// accidental log line prints the shape of this struct rather than its contents.
type Credential struct {
	// Token is the bearer token. Never log this field.
	Token string
	// ExpiresAt is when the token stops working, as reported by the issuing
	// authority rather than as requested.
	ExpiresAt time.Time
	// Server is the API endpoint the caller should use. Not necessarily the
	// address the hub minted through: a hub may reach a spoke privately while
	// the caller reaches it publicly.
	Server string
	// CABundle verifies Server, empty when it presents a publicly trusted
	// certificate.
	CABundle []byte
	// Namespace is the namespace boundary the credential is scoped to.
	Namespace string
	// ServiceAccount is the identity the credential acts as.
	ServiceAccount string
}

// String redacts the token.
//
// Implemented so that the obvious mistakes, a %v in an error or a fmt.Println
// while debugging, cannot leak credential material. The threat model calls
// accidental exposure through build output the most likely real-world leak in
// the system (docs/threat-model.md T-05), and a type that cannot be printed
// unsafely is worth more than a rule asking people not to.
func (c Credential) String() string {
	return fmt.Sprintf("Credential{ServiceAccount:%s/%s Server:%s ExpiresAt:%s Token:[redacted]}",
		c.Namespace, c.ServiceAccount, c.Server, c.ExpiresAt.Format(time.RFC3339))
}

// LogValue redacts the token for structured logging.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("service_account", c.Namespace+"/"+c.ServiceAccount),
		slog.String("server", c.Server),
		slog.Time("expires_at", c.ExpiresAt),
	)
}

// MintRequest is everything a provider needs to issue one credential.
type MintRequest struct {
	// Cluster is the chosen cell.
	Cluster *cellcastv1alpha1.Cluster
	// Trust is the configuration governing how to mint for it.
	Trust *cellcastv1alpha1.TrustConfig
	// TTL is the already-resolved, already-bounded lifetime.
	TTL time.Duration
}

// Provider mints credentials through one mechanism.
//
// The interface carries both implementations' shapes from the first release
// even though only Kubernetes ships, so that adding AWS STS in v0.2 does not
// reopen the broker's semantics (docs/architecture.md ADR-004).
type Provider interface {
	// Kind is the trust provider this implements.
	Kind() cellcastv1alpha1.TrustProvider

	// MinTTL is the shortest credential this mechanism can issue. Kubernetes
	// TokenRequest refuses anything under ten minutes; AWS STS AssumeRole
	// refuses anything under fifteen. Neither is negotiable, so the broker has
	// to reason about the floor rather than discover it in the deploy path.
	MinTTL() time.Duration

	// Mint issues the credential. Implementations must return the expiry the
	// issuing authority reported, not the one that was requested.
	Mint(ctx context.Context, req MintRequest) (*Credential, error)
}

// Broker resolves trust configuration and mints through the right provider.
type Broker struct {
	// reader serves TrustConfig from the manager's cache.
	reader client.Reader
	// ns is the hub namespace holding trust configuration.
	ns string
	// ceiling is the operator's absolute bound on credential lifetime.
	ceiling time.Duration
	log     *slog.Logger

	providers map[cellcastv1alpha1.TrustProvider]Provider
}

// New builds a broker over the given providers.
func New(reader client.Reader, namespace string, ceiling time.Duration, log *slog.Logger, providers ...Provider) *Broker {
	b := &Broker{
		reader:    reader,
		ns:        namespace,
		ceiling:   ceiling,
		log:       log,
		providers: make(map[cellcastv1alpha1.TrustProvider]Provider, len(providers)),
	}
	for _, p := range providers {
		b.providers[p.Kind()] = p
	}
	return b
}

// Mint issues a credential for a chosen cell.
//
// The cell must already have been chosen by the placement engine. There is no
// route that reaches this without going through placement first, which is what
// stops a caller from minting into a cell its policy would have refused
// (docs/threat-model.md T-03).
func (b *Broker) Mint(
	ctx context.Context,
	cluster *cellcastv1alpha1.Cluster,
	ttlPolicy *cellcastv1alpha1.TokenTTLPolicy,
	requested time.Duration,
	subject string,
) (*Credential, Resolution, error) {
	var zero Resolution

	trust, err := b.trustFor(ctx, cluster)
	if err != nil {
		return nil, zero, err
	}

	provider, ok := b.providers[trust.Spec.Provider]
	if !ok {
		return nil, zero, fmt.Errorf("%w: %s", ErrProviderNotImplemented, trust.Spec.Provider)
	}

	res, err := ResolveTTL(cluster.Labels, ttlPolicy, requested, b.ceiling, provider.MinTTL())
	if err != nil {
		return nil, res, fmt.Errorf("resolving credential lifetime for cell %s: %w", cluster.Name, err)
	}

	cred, err := provider.Mint(ctx, MintRequest{Cluster: cluster, Trust: trust, TTL: res.Granted})
	if err != nil {
		return nil, res, fmt.Errorf("minting for cell %s: %w", cluster.Name, err)
	}

	// The issuing authority is authoritative on expiry, and a Kubernetes API
	// server with no configured maximum will issue whatever it is asked for. If
	// it somehow returns more than the operator's ceiling permits, the
	// credential is discarded rather than returned: a bound that is checked
	// only on the way in is not a bound.
	if lifetime := time.Until(cred.ExpiresAt); lifetime > res.Max+time.Minute {
		return nil, res, fmt.Errorf("cell %s issued a credential lasting %s, exceeding the %s ceiling",
			cluster.Name, lifetime.Round(time.Second), res.Max)
	}

	b.log.InfoContext(ctx, "credential minted",
		slog.String("cell", cluster.Name),
		slog.String("subject", subject),
		slog.String("provider", string(trust.Spec.Provider)),
		slog.Duration("ttl_granted", res.Granted),
		slog.Bool("ttl_clamped", res.Clamped),
		slog.Any("credential", cred),
	)

	return cred, res, nil
}

// trustFor resolves the trust configuration a cell references.
func (b *Broker) trustFor(ctx context.Context, cluster *cellcastv1alpha1.Cluster) (*cellcastv1alpha1.TrustConfig, error) {
	name := cluster.Spec.TrustConfigRef.Name
	var trust cellcastv1alpha1.TrustConfig
	key := client.ObjectKey{Namespace: b.ns, Name: name}

	if err := b.reader.Get(ctx, key, &trust); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: cell %s references %q", ErrTrustConfigMissing, cluster.Name, name)
		}
		return nil, fmt.Errorf("reading trust configuration %q: %w", name, err)
	}

	if err := ValidateTrust(&trust); err != nil {
		return nil, err
	}
	return &trust, nil
}

// ValidateTrust reports whether a trust configuration is internally consistent.
//
// The CRD's own validation rules cover this for anything applied through the
// API server. This exists because the broker must not depend on that being the
// only way a resource got there, and because the TrustConfig controller reports
// the same answer as a condition before a deploy needs it.
func ValidateTrust(trust *cellcastv1alpha1.TrustConfig) error {
	switch trust.Spec.Provider {
	case cellcastv1alpha1.TrustProviderKubernetes:
		k := trust.Spec.Kubernetes
		switch {
		case k == nil:
			return fmt.Errorf("%w: %s names the kubernetes provider with no kubernetes block", ErrTrustConfigInvalid, trust.Name)
		case k.ServiceAccountName == "":
			return fmt.Errorf("%w: %s has no serviceAccountName", ErrTrustConfigInvalid, trust.Name)
		case k.Namespace == "":
			return fmt.Errorf("%w: %s has no namespace", ErrTrustConfigInvalid, trust.Name)
		}
	case cellcastv1alpha1.TrustProviderAWS:
		return fmt.Errorf("%w: %s", ErrProviderNotImplemented, trust.Spec.Provider)
	default:
		return fmt.Errorf("%w: %s names an unknown provider %q", ErrTrustConfigInvalid, trust.Name, trust.Spec.Provider)
	}

	src := trust.Spec.CredentialSource
	if src.InCluster == (src.SecretRef != nil) {
		return fmt.Errorf("%w: %s must set exactly one of inCluster or secretRef", ErrTrustConfigInvalid, trust.Name)
	}
	if src.SecretRef != nil && src.SecretRef.Name == "" {
		return fmt.Errorf("%w: %s has a secretRef with no name", ErrTrustConfigInvalid, trust.Name)
	}
	return nil
}

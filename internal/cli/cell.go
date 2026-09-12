package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// stockIssuer is the service account issuer of every kubeadm and kind cluster
// whose API server was not told otherwise. Two cells sharing it cannot be told
// apart by the hub (docs/architecture.md ADR-009).
const stockIssuer = "https://kubernetes.default.svc.cluster.local"

// maxDiscoveryBytes bounds the discovery document. A real one is a few hundred
// bytes, and the body comes from whatever answers at the kubeconfig's address.
const maxDiscoveryBytes = 64 << 10

// credentialKey is the key the TrustConfig reads the cell's kubeconfig from,
// and the key the Secret command on stderr stores it under.
const credentialKey = "kubeconfig"

var knownProviders = []cellcastv1alpha1.Provider{
	cellcastv1alpha1.ProviderEKS, cellcastv1alpha1.ProviderGKE,
	cellcastv1alpha1.ProviderAKS, cellcastv1alpha1.ProviderGeneric,
}

type cellAddOptions struct {
	kubeconfig          string
	kubeContext         string
	name                string
	hubNamespace        string
	labels              map[string]string
	provider            string
	state               string
	issuer              string
	mintServiceAccount  string
	mintNamespace       string
	agentNamespace      string
	agentServiceAccount string
	timeout             time.Duration
}

// resolvedCell is what cell add learned from the cell's kubeconfig and its
// discovery document.
type resolvedCell struct {
	endpoint string
	ca       []byte
	provider cellcastv1alpha1.Provider
	issuer   string
}

func newCellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cell",
		Short: "Enrol cells with the hub",
	}
	cmd.AddCommand(newCellAddCmd())
	return cmd
}

func newCellAddCmd() *cobra.Command {
	var opts cellAddOptions

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Print the objects that register a cell with the hub",
		Long: `Print the TrustConfig and the Cluster that register a cell with the hub.

The part that is easy to get wrong by hand is the reporter identity: the exact
issuer the cell's service account tokens carry. Wrong, it fails silently, and
the cell never wins a placement. This reads it from the cell's own discovery
document, through the kubeconfig you pass, so it is what the cell actually
issues rather than what anybody remembers it issuing.

Nothing is applied. The objects go to stdout, to be read and then piped:

    cellcast cell add --kubeconfig euw1.yaml --name euw1 \
        --mint-service-account deployer --mint-namespace apps \
        | kubectl apply -f -

The Secret the TrustConfig names is not printed. It holds the cell's credential,
and cellcast does not put credential material on stdout. What to run instead,
and the hub flag this cell's agent needs, go to stderr.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCellAdd(cmd, &opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "kubeconfig for the cell being enrolled (required)")
	f.StringVar(&opts.kubeContext, "context", "", "context to use from that kubeconfig (default: its current context)")
	f.StringVar(&opts.name, "name", "", "the cell's name, which the agent's cellName must match (required)")
	f.StringVar(&opts.hubNamespace, "hub-namespace", "cellcast-system", "namespace the hub runs in, where both objects are created")
	f.StringToStringVar(&opts.labels, "labels", nil, "labels policies select the cell by, for example env=prod,region=euw1")
	f.StringVar(&opts.provider, "provider", "", "eks, gke, aks or generic (default: inferred from the API server address, else generic)")
	f.StringVar(&opts.state, "state", string(cellcastv1alpha1.ClusterStateLive), "LIVE, DARK or DRAINING")
	f.StringVar(&opts.issuer, "issuer", "", "the cell's service account issuer, instead of reading it from the cell")
	// No defaults for what gets minted. A default here would decide, without
	// anybody choosing it, what every pipeline deploying to this cell may do.
	f.StringVar(&opts.mintServiceAccount, "mint-service-account", "", "service account in the cell that deploy credentials are minted for (required)")
	f.StringVar(&opts.mintNamespace, "mint-namespace", "", "namespace of that service account (required)")
	f.StringVar(&opts.agentNamespace, "agent-namespace", "cellcast-system", "namespace the agent runs in, in the cell")
	f.StringVar(&opts.agentServiceAccount, "agent-service-account", "cellcast-agent", "service account the agent runs as")
	f.DurationVar(&opts.timeout, "timeout", 15*time.Second, "how long to wait for the cell's discovery document")

	for _, name := range []string{"kubeconfig", "name", "mint-service-account", "mint-namespace"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(err)
		}
	}
	return cmd
}

func runCellAdd(cmd *cobra.Command, opts *cellAddOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}

	cfg, err := cellRESTConfig(opts.kubeconfig, opts.kubeContext)
	if err != nil {
		return err
	}
	cell, err := opts.resolve(cmd.Context(), cfg)
	if err != nil {
		return err
	}

	trust, cluster := opts.objects(cell)
	if err := writeManifests(cmd.OutOrStdout(), trust, cluster); err != nil {
		return err
	}
	return opts.writeNextSteps(cmd.ErrOrStderr(), cell.issuer)
}

// validate refuses, before anything is printed, what the API server would
// refuse after it, so a mistake costs one rerun rather than a half-created cell.
func (o *cellAddOptions) validate() error {
	if errs := validation.IsDNS1123Subdomain(o.name); len(errs) > 0 {
		return fmt.Errorf("--name %q cannot name a Kubernetes object: %s", o.name, strings.Join(errs, "; "))
	}
	states := []cellcastv1alpha1.ClusterState{
		cellcastv1alpha1.ClusterStateLive, cellcastv1alpha1.ClusterStateDark, cellcastv1alpha1.ClusterStateDraining,
	}
	if !slices.Contains(states, cellcastv1alpha1.ClusterState(o.state)) {
		return fmt.Errorf("--state must be LIVE, DARK or DRAINING, not %q", o.state)
	}
	if o.provider != "" && !slices.Contains(knownProviders, cellcastv1alpha1.Provider(o.provider)) {
		return fmt.Errorf("--provider must be eks, gke, aks or generic, not %q", o.provider)
	}
	return nil
}

// cellRESTConfig loads one kubeconfig file the way kubectl would: relative
// certificate paths resolved against the file, the chosen context or the
// file's own current one, and whatever authentication it names.
func cellRESTConfig(path, kubeContext string) (*rest.Config, error) {
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		// clientcmd reports the file and what was wrong with it, never its
		// contents: the same judgement the broker makes about the Secrets it
		// parses (docs/threat-model.md T-08).
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return cfg, nil
}

func (o *cellAddOptions) resolve(ctx context.Context, cfg *rest.Config) (resolvedCell, error) {
	cell := resolvedCell{endpoint: strings.TrimRight(cfg.Host, "/"), issuer: o.issuer}
	if !strings.HasPrefix(cell.endpoint, "https://") {
		return cell, fmt.Errorf("the kubeconfig's server is %s, and the hub reaches cells only over https", cfg.Host)
	}

	ca, err := caBundle(cfg)
	if err != nil {
		return cell, err
	}
	cell.ca = ca

	cell.provider = cellcastv1alpha1.Provider(o.provider)
	if cell.provider == "" {
		cell.provider = inferProvider(cell.endpoint)
	}

	if cell.issuer == "" {
		if cell.issuer, err = discoverIssuer(ctx, cfg, o.timeout); err != nil {
			return cell, fmt.Errorf("%w; pass --issuer to give the cell's service account issuer yourself", err)
		}
	}
	if !strings.HasPrefix(cell.issuer, "https://") {
		return cell, fmt.Errorf("issuer %q is not https, and the hub verifies tokens only from https issuers", cell.issuer)
	}
	return cell, nil
}

// caBundle is the certificate authority the hub verifies the cell's API server
// against: the kubeconfig's own, inline or from the file it names. Empty means
// a publicly trusted certificate, which is how the Cluster field is defined.
func caBundle(cfg *rest.Config) ([]byte, error) {
	if len(cfg.CAData) > 0 || cfg.CAFile == "" {
		return cfg.CAData, nil
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("reading the cell's certificate authority: %w", err)
	}
	return ca, nil
}

// inferProvider names the platform where the API server's address says it.
// EKS and AKS endpoints carry the provider's domain. A GKE endpoint is
// usually a bare IP with nothing to infer from, so a GKE cell needs
// --provider gke.
func inferProvider(endpoint string) cellcastv1alpha1.Provider {
	u, err := url.Parse(endpoint)
	if err != nil {
		return cellcastv1alpha1.ProviderGeneric
	}
	switch host := u.Hostname(); {
	case strings.HasSuffix(host, ".eks.amazonaws.com"):
		return cellcastv1alpha1.ProviderEKS
	case strings.HasSuffix(host, ".azmk8s.io"):
		return cellcastv1alpha1.ProviderAKS
	}
	return cellcastv1alpha1.ProviderGeneric
}

// discoverIssuer reads the cell's service account issuer from its OIDC
// discovery document, authenticated exactly as the kubeconfig authenticates
// kubectl.
//
// rest.TransportFor rather than a typed client. It is already linked, because
// the client writes kubeconfigs, and it handles bearer tokens, client
// certificates and exec plugins alike. The typed clientset would do nothing
// more here and costs the binary 2.8x (TestTheClientDoesNotCarryTheTypedClientset).
func discoverIssuer(ctx context.Context, cfg *rest.Config, timeout time.Duration) (string, error) {
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return "", fmt.Errorf("building a transport for the cell: %w", err)
	}
	client := &http.Client{Transport: transport, Timeout: timeout}

	target := strings.TrimRight(cfg.Host, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("building the discovery request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http puts the URL in this error and never the headers, so
		// neither a bearer token nor a client certificate reaches it.
		return "", fmt.Errorf("reading the cell's discovery document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("the cell refused its discovery document to this kubeconfig's identity (%s)", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("the cell's discovery document returned %s", resp.Status)
	}

	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBytes)).Decode(&doc); err != nil {
		return "", fmt.Errorf("decoding the cell's discovery document: %w", err)
	}
	if doc.Issuer == "" {
		return "", errors.New("the cell's discovery document names no issuer")
	}
	return doc.Issuer, nil
}

func (o *cellAddOptions) secretName() string { return o.name + "-kubeconfig" }

func (o *cellAddOptions) objects(cell resolvedCell) (*cellcastv1alpha1.TrustConfig, *cellcastv1alpha1.Cluster) {
	typeMeta := func(kind string) metav1.TypeMeta {
		return metav1.TypeMeta{APIVersion: cellcastv1alpha1.GroupVersion.String(), Kind: kind}
	}

	trust := &cellcastv1alpha1.TrustConfig{
		TypeMeta:   typeMeta("TrustConfig"),
		ObjectMeta: metav1.ObjectMeta{Name: o.name, Namespace: o.hubNamespace},
		Spec: cellcastv1alpha1.TrustConfigSpec{
			Provider: cellcastv1alpha1.TrustProviderKubernetes,
			CredentialSource: cellcastv1alpha1.CredentialSource{
				SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: o.secretName(), Key: credentialKey},
			},
			Kubernetes: &cellcastv1alpha1.KubernetesTrust{
				ServiceAccountName: o.mintServiceAccount,
				Namespace:          o.mintNamespace,
			},
		},
	}

	cluster := &cellcastv1alpha1.Cluster{
		TypeMeta:   typeMeta("Cluster"),
		ObjectMeta: metav1.ObjectMeta{Name: o.name, Namespace: o.hubNamespace, Labels: o.labels},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       cell.endpoint,
			CABundle:       cell.ca,
			Provider:       cell.provider,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: o.name},
			Reporter: &cellcastv1alpha1.ReporterIdentity{
				Issuer:  cell.issuer,
				Subject: fmt.Sprintf("system:serviceaccount:%s:%s", o.agentNamespace, o.agentServiceAccount),
			},
			State: cellcastv1alpha1.ClusterState(o.state),
		},
	}
	return trust, cluster
}

func writeManifests(w io.Writer, objs ...any) error {
	for _, obj := range objs {
		doc, err := manifest(obj)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "---\n%s", doc); err != nil {
			return err
		}
	}
	return nil
}

// manifest renders an object as the YAML a person would write, without the
// empty status block and the null creation timestamp that marshalling a typed
// object produces. Neither means anything in a manifest, and both invite the
// question of whether they should be edited.
func manifest(obj any) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encoding %T: %w", obj, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decoding %T: %w", obj, err)
	}
	delete(fields, "status")
	if meta, ok := fields["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	return yaml.Marshal(fields)
}

// writeNextSteps says what cell add deliberately did not do. On stderr, so
// stdout stays a manifest that pipes straight into kubectl.
func (o *cellAddOptions) writeNextSteps(w io.Writer, issuer string) error {
	out := &lineWriter{w: w}
	if issuer == stockIssuer {
		out.printf("warning: %s is the issuer of every kubeadm and kind cluster not configured\n"+
			"otherwise. The hub tells one cell's agent from another's by issuer, so this cell's capacity\n"+
			"reports would be indistinguishable from any other such cluster's. Set\n"+
			"--service-account-issuer on its API server first (docs/architecture.md ADR-009).\n\n", issuer)
	}
	out.printf("Not printed: the Secret the TrustConfig names, because it holds the cell's credential.\n"+
		"Create it from a kubeconfig that can create tokens for %s in %s and nothing more:\n\n"+
		"  kubectl -n %s create secret generic %s --from-file=%s=%s\n\n",
		o.mintServiceAccount, o.mintNamespace, o.hubNamespace, o.secretName(), credentialKey, o.kubeconfig)
	out.printf("The hub must trust this cell's issuer, or its agent's capacity reports are refused:\n\n"+
		"  --oidc-issuer %s=generic\n\n", issuer)
	out.printf("Then install the agent in the cell with cellName=%s.\n", o.name)
	return out.err
}

package cli

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// cellToken stands in for the operator's credential in the cell. It has to
// reach the cell's discovery endpoint and nowhere else.
const cellToken = "SENTINEL-CELL-TOKEN-4f2a90"

const eksIssuer = "https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE"

// fakeCell is a cell's API server as far as enrolment is concerned: one
// discovery document behind TLS.
type fakeCell struct {
	*httptest.Server
	issuer string
	status int

	mu       sync.Mutex
	calls    int
	sawToken string
}

func newFakeCell(t *testing.T, issuer string, status int) *fakeCell {
	t.Helper()

	c := &fakeCell{issuer: issuer, status: status}
	c.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls++
		c.sawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		c.mu.Unlock()

		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		if c.status != http.StatusOK {
			w.WriteHeader(c.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": c.issuer})
	}))
	t.Cleanup(c.Close)
	return c
}

// observed is how many times the cell was asked, and with what bearer token.
func (c *fakeCell) observed() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.sawToken
}

// cellKubeconfig writes a kubeconfig for the fake cell that trusts its
// certificate and authenticates with cellToken.
func cellKubeconfig(t *testing.T, cell *fakeCell) string {
	t.Helper()

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["euw1"] = &clientcmdapi.Cluster{
		Server:                   cell.URL,
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cell.Certificate().Raw}),
	}
	cfg.AuthInfos["operator"] = &clientcmdapi.AuthInfo{Token: cellToken}
	cfg.Contexts["euw1"] = &clientcmdapi.Context{Cluster: "euw1", AuthInfo: "operator"}
	cfg.CurrentContext = "euw1"

	path := filepath.Join(t.TempDir(), "euw1.kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("writing the cell's kubeconfig: %v", err)
	}
	return path
}

// runCellAddCmd runs `cellcast cell add` for a cell named euw1 that mints for
// deployer in apps, with any further arguments appended.
func runCellAddCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	cmd := NewRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"cell", "add", "--name", "euw1",
		"--mint-service-account", "deployer", "--mint-namespace", "apps"}, args...))

	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// decodeEnrolment parses what cell add printed into the real API types,
// strictly. Strict is the point: a field name that drifted from the CRD would
// otherwise decode as nothing, and the assertions would check an empty value.
func decodeEnrolment(t *testing.T, stdout string) (cellcastv1alpha1.TrustConfig, cellcastv1alpha1.Cluster) {
	t.Helper()

	var trust cellcastv1alpha1.TrustConfig
	var cluster cellcastv1alpha1.Cluster
	var kinds []string
	for _, doc := range strings.Split(stdout, "---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("cell add printed something that is not YAML: %v\n%s", err, stdout)
		}
		kinds = append(kinds, head.Kind)

		var target any
		switch head.Kind {
		case "TrustConfig":
			target = &trust
		case "Cluster":
			target = &cluster
		default:
			t.Fatalf("cell add printed a %s; it prints a TrustConfig and a Cluster and nothing else", head.Kind)
		}
		if err := yaml.UnmarshalStrict([]byte(doc), target); err != nil {
			t.Fatalf("the %s does not decode into the API type: %v", head.Kind, err)
		}
	}

	if len(kinds) != 2 {
		t.Fatalf("cell add printed %v, want a TrustConfig and a Cluster", kinds)
	}
	return trust, cluster
}

func TestCellAddReadsTheIssuerFromTheCellItself(t *testing.T) {
	// The reporter issuer is the value that is easy to get wrong by hand and
	// silent when wrong: the agent's reports are refused and the cell never
	// wins a placement. Read from the cell, it is what the cell actually
	// issues rather than what somebody remembered.
	cell := newFakeCell(t, eksIssuer, http.StatusOK)
	stdout, _, err := runCellAddCmd(t, "--kubeconfig", cellKubeconfig(t, cell), "--labels", "env=prod,region=euw1")
	if err != nil {
		t.Fatalf("cell add failed: %v", err)
	}

	trust, cluster := decodeEnrolment(t, stdout)
	if cluster.Spec.Reporter == nil {
		t.Fatal("the Cluster declares no reporter, so no agent could ever report for it")
	}
	if got := cluster.Spec.Reporter.Issuer; got != eksIssuer {
		t.Errorf("reporter issuer = %q, want the %q the cell's discovery document names", got, eksIssuer)
	}
	if got, want := cluster.Spec.Reporter.Subject, "system:serviceaccount:cellcast-system:cellcast-agent"; got != want {
		t.Errorf("reporter subject = %q, want %q", got, want)
	}
	if cluster.Spec.Endpoint != cell.URL {
		t.Errorf("endpoint = %q, want the kubeconfig's server %q", cluster.Spec.Endpoint, cell.URL)
	}
	if len(cluster.Spec.CABundle) == 0 {
		t.Error("the Cluster carries no CA bundle, so the hub could not verify the cell's API server")
	}
	if cluster.Labels["env"] != "prod" || cluster.Labels["region"] != "euw1" {
		t.Errorf("labels = %v, want the ones policies will select on", cluster.Labels)
	}
	if cluster.Spec.TrustConfigRef.Name != trust.Name {
		t.Errorf("the Cluster references trust configuration %q and the one printed is %q", cluster.Spec.TrustConfigRef.Name, trust.Name)
	}

	ref := trust.Spec.CredentialSource.SecretRef
	if ref == nil || ref.Name != "euw1-kubeconfig" || ref.Key != "kubeconfig" {
		t.Errorf("secretRef = %+v, want euw1-kubeconfig/kubeconfig, which is what stderr says to create", ref)
	}
	if k := trust.Spec.Kubernetes; k == nil || k.ServiceAccountName != "deployer" || k.Namespace != "apps" {
		t.Errorf("mints for %+v, want deployer in apps", k)
	}

	if _, token := cell.observed(); token != cellToken {
		t.Error("the discovery request did not carry the kubeconfig's credential")
	}
}

func TestCellAddNeverPrintsTheCellsCredential(t *testing.T) {
	// T-05. The kubeconfig is read in order to reach the cell, and its
	// credential goes to the cell and nowhere else: not into the manifests,
	// which get piped, pasted and committed, and not into the guidance on
	// stderr. decodeEnrolment separately fails on a printed Secret.
	cell := newFakeCell(t, eksIssuer, http.StatusOK)
	stdout, stderr, err := runCellAddCmd(t, "--kubeconfig", cellKubeconfig(t, cell))
	if err != nil {
		t.Fatalf("cell add failed: %v", err)
	}
	decodeEnrolment(t, stdout)

	for name, out := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(out, cellToken) {
			t.Errorf("%s carries the cell's credential", name)
		}
	}
	if !strings.Contains(stderr, "create secret generic euw1-kubeconfig") {
		t.Errorf("stderr does not say how to create the Secret that was not printed:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--oidc-issuer "+eksIssuer+"=generic") {
		t.Errorf("stderr does not name the issuer the hub has to trust for this cell's agent:\n%s", stderr)
	}
}

func TestCellAddNamesTheFlagWhenTheCellRefusesDiscovery(t *testing.T) {
	// The fallback is only useful if the error says it exists.
	cell := newFakeCell(t, eksIssuer, http.StatusForbidden)
	_, _, err := runCellAddCmd(t, "--kubeconfig", cellKubeconfig(t, cell))
	if err == nil {
		t.Fatal("cell add succeeded with no issuer to put in the Cluster")
	}
	if !strings.Contains(err.Error(), "--issuer") {
		t.Errorf("error %q does not name the flag that gets past it", err)
	}
}

func TestCellAddTakesAnIssuerWithoutAskingTheCell(t *testing.T) {
	cell := newFakeCell(t, "https://wrong.example.test", http.StatusOK)
	stdout, _, err := runCellAddCmd(t, "--kubeconfig", cellKubeconfig(t, cell), "--issuer", eksIssuer)
	if err != nil {
		t.Fatalf("cell add failed: %v", err)
	}

	if _, cluster := decodeEnrolment(t, stdout); cluster.Spec.Reporter.Issuer != eksIssuer {
		t.Errorf("reporter issuer = %q, want the one passed with --issuer", cluster.Spec.Reporter.Issuer)
	}
	if calls, _ := cell.observed(); calls != 0 {
		t.Errorf("--issuer was given and the cell was still asked %d time(s)", calls)
	}
}

func TestCellAddWarnsAboutTheIssuerEveryKindClusterShares(t *testing.T) {
	// Every kubeadm and kind cluster issues as this unless told otherwise, and
	// the hub cannot tell two such cells' agents apart. The command cannot see
	// the rest of the fleet, but it can see a value known to collide.
	cell := newFakeCell(t, stockIssuer, http.StatusOK)
	_, stderr, err := runCellAddCmd(t, "--kubeconfig", cellKubeconfig(t, cell))
	if err != nil {
		t.Fatalf("cell add failed: %v", err)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "ADR-009") {
		t.Errorf("no warning about the shared issuer:\n%s", stderr)
	}
}

func TestCellAddRefusesWhatTheAPIServerWould(t *testing.T) {
	cell := newFakeCell(t, eksIssuer, http.StatusOK)
	kubeconfig := cellKubeconfig(t, cell)

	for name, args := range map[string][]string{
		"a name that cannot be an object": {"--name", "EUW1_prod"},
		"an unknown state":                {"--state", "PAUSED"},
		"an unknown provider":             {"--provider", "digitalocean"},
		"an issuer that is not https":     {"--issuer", "http://oidc.example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runCellAddCmd(t, append([]string{"--kubeconfig", kubeconfig}, args...)...)
			if err == nil {
				t.Error("cell add accepted it")
			}
			if stdout != "" {
				t.Errorf("cell add refused and still printed manifests:\n%s", stdout)
			}
		})
	}
}

func TestProviderIsInferredOnlyWhereTheAddressSaysSo(t *testing.T) {
	for endpoint, want := range map[string]cellcastv1alpha1.Provider{
		"https://ABCDEF1234.gr7.eu-west-1.eks.amazonaws.com":   cellcastv1alpha1.ProviderEKS,
		"https://prod-dns-1a2b3c.hcp.westeurope.azmk8s.io:443": cellcastv1alpha1.ProviderAKS,
		// GKE's usual shape. There is nothing in it to infer from, and guessing
		// would be worse than saying generic.
		"https://34.77.12.9":     cellcastv1alpha1.ProviderGeneric,
		"https://127.0.0.1:6451": cellcastv1alpha1.ProviderGeneric,
	} {
		if got := inferProvider(endpoint); got != want {
			t.Errorf("inferProvider(%q) = %s, want %s", endpoint, got, want)
		}
	}
}

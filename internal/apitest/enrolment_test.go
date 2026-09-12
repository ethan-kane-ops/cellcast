package apitest

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ethan-kane-ops/cellcast/internal/cli"
)

// TestWhatCellAddPrintsIsAcceptedByTheAPIServer holds the enrolment command to
// the CRDs it writes for.
//
// The command's unit tests decode its output strictly into the Go types, which
// proves the field names. Only an API server proves the rest: the https
// patterns on the endpoint and the issuer, the provider and state enums, and
// the CEL rules on TrustConfig. The output is what an operator pipes straight
// into kubectl, so a rule it breaks is a failed enrolment.
func TestWhatCellAddPrintsIsAcceptedByTheAPIServer(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	// The kubeconfig supplies the endpoint and nothing else here. --issuer
	// means the command never dials it, so the address only has to be well
	// formed, and the CA is left out as it would be for a public certificate.
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["euw1"] = &clientcmdapi.Cluster{Server: "https://euw1.example.test"}
	cfg.AuthInfos["operator"] = &clientcmdapi.AuthInfo{Token: "unused"}
	cfg.Contexts["euw1"] = &clientcmdapi.Context{Cluster: "euw1", AuthInfo: "operator"}
	cfg.CurrentContext = "euw1"
	kubeconfig := filepath.Join(t.TempDir(), "euw1.kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, kubeconfig); err != nil {
		t.Fatalf("writing the cell's kubeconfig: %v", err)
	}

	cmd := cli.NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"cell", "add",
		"--kubeconfig", kubeconfig, "--name", "euw1", "--hub-namespace", ns,
		"--issuer", "https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE",
		"--labels", "env=prod,region=euw1",
		"--mint-service-account", "deployer", "--mint-namespace", "apps",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cell add failed: %v", err)
	}

	var kinds []string
	for _, doc := range splitDocs(out.String()) {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
			t.Fatalf("cell add printed something that is not YAML: %v\n%s", err, doc)
		}
		if obj.GetKind() == "" {
			continue
		}
		kinds = append(kinds, obj.GetKind())

		// DryRunAll runs validation, defaulting and CEL and persists nothing,
		// so the TrustConfig can name a Secret that is not here.
		if err := c.Create(context.Background(), obj, client.DryRunAll); err != nil {
			t.Errorf("the API server refused the %s that cell add printed: %v", obj.GetKind(), err)
		}
	}
	if len(kinds) != 2 {
		t.Fatalf("cell add printed %v, want a TrustConfig and a Cluster", kinds)
	}
}

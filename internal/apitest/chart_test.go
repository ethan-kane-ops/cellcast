package apitest

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// TestTheChartsProduceManifestsTheAPIServerAccepts is the check `helm lint`
// cannot make.
//
// Lint reads the templates and template rendering produces YAML; neither knows
// whether Kubernetes will take it. A misspelled field inside a pod spec, a
// probe with the wrong shape, a topology constraint with a typo: all of them
// render cleanly and fail at `helm install`, which is the worst moment to find
// out because the release is already half applied.
//
// Server-side dry run against a real API server answers it, with no cluster to
// arrange: the same control plane the rest of this package uses.
func TestTheChartsProduceManifestsTheAPIServerAccepts(t *testing.T) {
	tests := []struct {
		name  string
		chart string
		args  []string
	}{
		{
			name:  "hub",
			chart: "cellcast",
			// Every optional block that does not need the Prometheus Operator
			// CRDs, because a block nobody renders is a block nobody checked.
			args: []string{
				"--set", "hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com",
				"--set", "hub.oidc.issuers[0].provider=github",
				"--set", "networkPolicy.enabled=true",
				"--set", "networkPolicy.allowedIngress[0].podSelector.matchLabels.app=ci",
				"--set", "priorityClassName=system-cluster-critical",
			},
		},
		{
			name:  "agent",
			chart: "cellcast-agent",
			args: []string{
				"--set", "cellName=prod-euw1",
				"--set", "hub.endpoint=https://hub.example.test",
				"--set", "hub.caConfigMap=hub-ca",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t)
			ns := newNamespace(t, c)

			objects := renderChart(t, tt.chart, append(tt.args, "--namespace", ns)...)
			if len(objects) == 0 {
				t.Fatal("the chart rendered no objects, so this test checked nothing")
			}

			var kinds []string
			for _, obj := range objects {
				kinds = append(kinds, obj.GetKind())
				// DryRunAll runs validation and admission and persists nothing,
				// so this leaves the control plane exactly as it found it.
				err := c.Create(context.Background(), obj, client.DryRunAll)
				if err != nil && !strings.Contains(err.Error(), "already exists") {
					t.Errorf("the API server refused %s %s: %v",
						obj.GetKind(), obj.GetName(), err)
				}
			}
			t.Logf("accepted: %s", strings.Join(kinds, ", "))
		})
	}
}

// renderChart runs `helm template` and parses the result into objects, with the
// namespace-scoped ones pinned to ns.
func renderChart(t *testing.T, chart string, args ...string) []*unstructured.Unstructured {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	full := append([]string{"template", "test", filepath.Join("..", "..", "charts", chart)}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", chart, err, out)
	}

	var objects []*unstructured.Unstructured
	for _, doc := range splitDocs(string(out)) {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
			t.Fatalf("parsing a rendered document: %v\n%s", err, doc)
		}
		if obj.GetKind() == "" {
			continue
		}
		objects = append(objects, obj)
	}
	return objects
}

// splitDocs breaks a multi-document YAML stream on its separators.
//
// Line-based rather than a naive split on "---", because that string also
// appears inside the CRD descriptions the chart installs.
func splitDocs(in string) []string {
	var docs []string
	var current strings.Builder

	flush := func() {
		if strings.TrimSpace(current.String()) != "" {
			docs = append(docs, current.String())
		}
		current.Reset()
	}

	scanner := bufio.NewScanner(strings.NewReader(in))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimRight(line, " \t") == "---" {
			flush()
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	flush()
	return docs
}

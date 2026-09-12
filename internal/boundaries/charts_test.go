package boundaries_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/ethan-kane-ops/cellcast/internal/cli"
)

// chartFlags are the flags a chart hands to a binary, and binaryFlags are the
// ones that binary accepts. A chart that ships a flag its image does not
// understand installs cleanly and then crash-loops, which is the worst place to
// find out: the manifests are correct, the rollout is stuck, and nothing in the
// chart says why.
var (
	renderedFlag = regexp.MustCompile(`^\s*- (--[a-z0-9-]+)`)
	helpFlag     = regexp.MustCompile(`(--[a-z0-9-]+)`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving the repo root: %v", err)
	}
	return root
}

// render runs `helm template` and returns the manifests.
func render(t *testing.T, chart string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	full := append([]string{"template", "test", filepath.Join(repoRoot(t), "charts", chart)}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", chart, err, out)
	}
	return string(out)
}

// binaryFlags returns the flags a command accepts, read off its own help.
//
// sub names a subcommand to descend into. A cobra root command lists only its
// own flags, so asking `cellcast --help` about `--workload` answers no, which is
// true of the root and false of the tool.
func binaryFlags(t *testing.T, cmd string, sub ...string) []string {
	t.Helper()

	args := append([]string{"run", filepath.Join(repoRoot(t), "cmd", cmd)}, sub...)
	out, err := exec.Command("go", append(args, "--help")...).CombinedOutput()
	if err != nil {
		t.Skipf("building %s: %v\n%s", cmd, err, out)
	}

	var flags []string
	for _, m := range helpFlag.FindAllStringSubmatch(string(out), -1) {
		if !slices.Contains(flags, m[1]) {
			flags = append(flags, m[1])
		}
	}
	if len(flags) == 0 {
		t.Fatalf("no flags found in %s --help:\n%s", cmd, out)
	}
	return flags
}

// clientFlags is every flag the client accepts, root and subcommands together.
//
// The subcommands are walked from the command tree rather than listed, so a new
// one's flags are covered the day it is added. With a list, a command whose
// flags all happen to be place's passes, and one with flags of its own fails
// for a reason that has nothing to do with the page being checked.
func clientFlags(t *testing.T) []string {
	t.Helper()

	flags := binaryFlags(t, "cellcast")
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, m := range helpFlag.FindAllStringSubmatch(cmd.Flags().FlagUsages(), -1) {
			if !slices.Contains(flags, m[1]) {
				flags = append(flags, m[1])
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(cli.NewRootCmd())
	return flags
}

// flagsIn returns the distinct flags a rendered manifest passes as container
// args.
func flagsIn(manifests string) []string {
	var flags []string
	for _, line := range strings.Split(manifests, "\n") {
		m := renderedFlag.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if !slices.Contains(flags, m[1]) {
			flags = append(flags, m[1])
		}
	}
	return flags
}

func TestChartsPassOnlyFlagsTheBinariesAccept(t *testing.T) {
	tests := []struct {
		name   string
		chart  string
		binary string
		args   []string
	}{
		{
			name:   "hub",
			chart:  "cellcast",
			binary: "cellcast-hub",
			// Every optional block on, so a flag that only appears under a
			// values combination is still checked.
			args: []string{
				"--set", "hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com",
				"--set", "hub.oidc.issuers[0].provider=github",
				"--set", "observability.otlp.endpoint=http://otel-collector:4318",
				"--set", "observability.otlp.headersSecret.name=otlp-headers",
			},
		},
		{
			name:   "agent",
			chart:  "cellcast-agent",
			binary: "cellcast-agent",
			args: []string{
				"--set", "cellName=prod-euw1",
				"--set", "hub.endpoint=https://hub.example.test",
				"--set", "hub.caConfigMap=hub-ca",
				"--set", "observability.otlp.endpoint=http://otel-collector:4318",
				"--set", "observability.otlp.headersSecret.name=otlp-headers",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accepted := binaryFlags(t, tt.binary)

			shipped := flagsIn(render(t, tt.chart, tt.args...))
			if len(shipped) == 0 {
				t.Fatal("the chart passed no flags at all, so this test checked nothing")
			}

			for _, flag := range shipped {
				if !slices.Contains(accepted, flag) {
					t.Errorf("the chart passes %s, which %s does not accept; the pod would crash-loop on install", flag, tt.binary)
				}
			}
		})
	}
}

func TestTheChartsReadOTLPHeadersFromASecret(t *testing.T) {
	// A backend's API key belongs in a Secret and reaches the process through
	// the environment. As an argument it would sit in the pod spec, readable by
	// anyone who can get the Deployment (docs/threat-model.md T-05).
	for _, tt := range []struct {
		chart string
		args  []string
	}{
		{chart: "cellcast"},
		{chart: "cellcast-agent", args: []string{"--set", "cellName=prod-euw1", "--set", "hub.endpoint=https://hub.example.test"}},
	} {
		t.Run(tt.chart, func(t *testing.T) {
			out := render(t, tt.chart, append(tt.args,
				"--set", "observability.otlp.endpoint=http://$(HOST_IP):4318",
				"--set", "observability.otlp.headersSecret.name=otlp-headers",
			)...)
			for _, want := range []string{
				"name: OTEL_EXPORTER_OTLP_HEADERS", "name: otlp-headers", "key: headers",
				"name: HOST_IP", "fieldPath: status.hostIP", "--otlp-endpoint=http://$(HOST_IP):4318",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("the rendered %s chart has no %q", tt.chart, want)
				}
			}
		})
	}
}

// crd is the part of a CustomResourceDefinition worth comparing. The name
// identifies it and the spec is the schema the API server will enforce;
// everything else is chart-owned decoration.
type crd struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Kind string         `json:"kind"`
	Spec map[string]any `json:"spec"`
}

// crdsIn parses every CustomResourceDefinition out of a multi-document stream.
func crdsIn(t *testing.T, manifests string) map[string]crd {
	t.Helper()

	out := map[string]crd{}
	for _, doc := range strings.Split(manifests, "\n---\n") {
		var parsed crd
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			// Not every document is a CRD, and the ones that are not are not
			// this test's business.
			continue
		}
		if parsed.Kind != "CustomResourceDefinition" {
			continue
		}
		out[parsed.Metadata.Name] = parsed
	}
	return out
}

func TestTheHubChartInstallsTheGeneratedSchemaUnaltered(t *testing.T) {
	// The chart holds a copy because Helm's .Files cannot reach outside the
	// chart directory, and the template reparses it to attach an annotation.
	// That round trip is where a CEL validation rule or a default would go
	// missing, and the failure would be an API server accepting a Cluster it
	// should have rejected. Comparing the whole spec is the only version of
	// this check that would notice.
	root := repoRoot(t)

	files, err := filepath.Glob(filepath.Join(root, "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated CRDs found (%v); run `just manifests`", err)
	}

	installed := crdsIn(t, render(t, "cellcast"))
	if len(installed) != len(files) {
		t.Fatalf("the chart installs %d CRDs, want %d; run `just manifests`", len(installed), len(files))
	}

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var want crd
		if err := yaml.Unmarshal(raw, &want); err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}

		got, ok := installed[want.Metadata.Name]
		if !ok {
			t.Errorf("the chart does not install %s; run `just manifests`", want.Metadata.Name)
			continue
		}
		if diff := cmp.Diff(want.Spec, got.Spec); diff != "" {
			t.Errorf("%s: the installed schema is not the generated one (-generated +installed):\n%s",
				want.Metadata.Name, diff)
		}
	}
}

func TestTheHubChartCanLeaveTheCRDsAlone(t *testing.T) {
	// Plenty of shops manage CRDs out of band, and a chart that cannot be
	// installed without them is a chart those shops cannot install.
	manifests := render(t, "cellcast", "--set", "crds.install=false")

	if strings.Contains(manifests, "kind: CustomResourceDefinition") {
		t.Error("crds.install=false still rendered a CustomResourceDefinition")
	}
	if !strings.Contains(manifests, "kind: Deployment") {
		t.Error("crds.install=false rendered no Deployment; the rest of the chart must still install")
	}
}

// alertNames returns every alert name in a rules document.
var alertPattern = regexp.MustCompile(`alert: (\w+)`)

func alertNames(in string) []string {
	var names []string
	for _, m := range alertPattern.FindAllStringSubmatch(in, -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	return names
}

func TestTheHubChartShipsTheSameAlertsAsTheRepo(t *testing.T) {
	// The alert expressions are tied to the metrics the code emits by a
	// contract test over config/prometheus. A second copy maintained by hand
	// would quietly stop agreeing with it, and the first sign would be an alert
	// that never fires.
	//
	// Compared as whole names in both directions. Asking whether each expected
	// name appears somewhere in the rendered text passes on a renamed alert,
	// because the old name is a prefix of the new one.
	root := repoRoot(t)

	shipped, err := os.ReadFile(filepath.Join(root, "config", "prometheus", "prometheusrule.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped rules: %v", err)
	}

	want := alertNames(string(shipped))
	if len(want) == 0 {
		t.Fatal("no alerts in config/prometheus/prometheusrule.yaml")
	}

	got := alertNames(render(t, "cellcast", "--set", "metrics.prometheusRule.enabled=true"))
	if !slices.Equal(got, want) {
		t.Errorf("the chart ships %v, want %v; run `just manifests`", got, want)
	}
}

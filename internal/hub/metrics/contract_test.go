package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promb "go.opentelemetry.io/contrib/bridges/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// seeded returns a registry holding every cellcast metric the hub can emit.
//
// Seeded rather than described, because Gather returns only metrics that have
// values, and a name that is declared and never emitted is not one a dashboard
// can plot.
func seeded(t *testing.T) *prometheus.Registry {
	t.Helper()

	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	m.Notify(t.Context(), audit.Record{
		Event: audit.EventPlacement, Outcome: audit.OutcomeGranted, Policy: "p", Cell: "c",
	})
	m.Notify(t.Context(), audit.Record{
		Event: audit.EventMint, Outcome: audit.OutcomeGranted,
		Provider: "eks", Env: "prod", GrantedTTL: 15 * time.Minute,
	})
	m.Notify(t.Context(), audit.Record{
		Event: audit.EventMint, Outcome: audit.OutcomeRefused, Provider: "eks", Reason: refusal.MintFailed,
	})
	m.ObservePlacement("granted", time.Second)
	m.AuthRejected("Expired")

	index := capacity.New(capacity.Options{Staleness: time.Hour})
	report(t, index, "c", 3000, 10000)
	if err := reg.Register(NewCapacityCollector(index)); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}

	scheme := runtime.NewScheme()
	if err := cellcastv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cell("c", cellcastv1alpha1.ClusterStateLive)).Build()
	if err := reg.Register(NewClusterStateCollector(k8s, testNamespace, discardLogger())); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}
	return reg
}

// exported returns every cellcast metric name the hub can emit.
func exported(t *testing.T) []string {
	t.Helper()

	families, err := seeded(t).Gather()
	if err != nil {
		t.Fatalf("Gather() = %v, want nil", err)
	}

	var names []string
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "cellcast_") {
			names = append(names, f.GetName())
		}
	}
	slices.Sort(names)
	return names
}

// referenced pulls every cellcast metric name out of a shipped artefact.
//
// Histogram suffixes are stripped so that a dashboard querying
// cellcast_token_ttl_seconds_bucket is checked against the family this package
// registers.
var metricRef = regexp.MustCompile(`cellcast_[a-z0-9_]+`)

func referenced(t *testing.T, path string) []string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "..", "..", path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	seen := map[string]struct{}{}
	for _, name := range metricRef.FindAllString(string(body), -1) {
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			name = strings.TrimSuffix(name, suffix)
		}
		seen[name] = struct{}{}
	}

	var out []string
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// TestShippedArtefactsOnlyReferenceMetricsWeExport is the join between the code
// and the two files an operator actually installs.
//
// Renaming a metric is a one-line change that leaves the dashboard rendering
// empty panels and, worse, leaves an alert that can never fire. Neither failure
// is visible until the day it was supposed to be.
func TestShippedArtefactsOnlyReferenceMetricsWeExport(t *testing.T) {
	have := exported(t)

	for _, artefact := range []string{
		"dashboards/cellcast.json",
		"config/prometheus/prometheusrule.yaml",
	} {
		t.Run(artefact, func(t *testing.T) {
			for _, want := range referenced(t, artefact) {
				if !slices.Contains(have, want) {
					t.Errorf("%s queries %s, which the hub does not export.\nExported: %v",
						artefact, want, have)
				}
			}
		})
	}
}

// TestEveryExportedMetricIsOnTheDashboardOrInAnAlert is the other direction. A
// metric nobody plots and nobody alerts on is one that was added for a reason
// that has since been forgotten, and it still costs a series per cell forever.
func TestEveryExportedMetricIsOnTheDashboardOrInAnAlert(t *testing.T) {
	used := append(referenced(t, "dashboards/cellcast.json"),
		referenced(t, "config/prometheus/prometheusrule.yaml")...)

	for _, name := range exported(t) {
		if !slices.Contains(used, name) {
			t.Errorf("%s is exported but neither plotted nor alerted on; plot it or drop it", name)
		}
	}
}

// TestTheOTLPExportCarriesTheScrapedNames holds the OTLP push to the names a
// scrape reports.
//
// The hub exports this registry through the Prometheus bridge rather than
// defining a second set of instruments, and this is what makes that a
// guarantee instead of an intention: a dashboard or an alert written against
// docs/metrics.md works on either path.
func TestTheOTLPExportCarriesTheScrapedNames(t *testing.T) {
	scopes, err := promb.NewMetricProducer(promb.WithGatherer(seeded(t))).Produce(t.Context())
	if err != nil {
		t.Fatalf("Produce() = %v, want nil", err)
	}

	var pushed []string
	for _, sm := range scopes {
		for _, m := range sm.Metrics {
			if strings.HasPrefix(m.Name, "cellcast_") {
				pushed = append(pushed, m.Name)
			}
		}
	}
	slices.Sort(pushed)
	if scraped := exported(t); !slices.Equal(pushed, scraped) {
		t.Errorf("OTLP carries %v, a scrape reports %v", pushed, scraped)
	}
}

// TestTheDocumentedSurfaceIsTheRealOne keeps docs/metrics.md honest. It is the
// page an adopter reads before deciding whether cellcast can be operated, so a
// metric listed there and never emitted is worse than one left undocumented.
func TestTheDocumentedSurfaceIsTheRealOne(t *testing.T) {
	documented := referenced(t, "docs/metrics.md")
	have := exported(t)

	for _, name := range documented {
		if !slices.Contains(have, name) {
			t.Errorf("docs/metrics.md documents %s, which is not exported", name)
		}
	}
	for _, name := range have {
		if !slices.Contains(documented, name) {
			t.Errorf("%s is exported and undocumented; add it to docs/metrics.md", name)
		}
	}
}

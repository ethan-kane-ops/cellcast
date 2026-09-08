package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

const testNamespace = "cellcast-system"

var errUnreadable = errors.New("informer cache is not synced")

func newMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := New(reg)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	return m, reg
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewRefusesToRegisterTwice pins that a duplicate registration is an error
// the caller sees rather than a panic inside a package initialiser. It is why
// the collectors are fields on a value rather than package-level variables.
func TestNewRefusesToRegisterTwice(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := New(reg); err != nil {
		t.Fatalf("first New() = %v, want nil", err)
	}
	if _, err := New(reg); err == nil {
		t.Error("second New() = nil, want a duplicate registration error")
	}
}

// TestEveryMetricIsIndependentPerRegistry is the other half of that decision. A
// package-level counter would carry counts between tests, and between a test
// and whatever ran before it.
func TestEveryMetricIsIndependentPerRegistry(t *testing.T) {
	first, _ := newMetrics(t)
	second, _ := newMetrics(t)

	first.AuthRejected("Expired")

	if got := testutil.ToFloat64(first.authRejections.WithLabelValues("Expired")); got != 1 {
		t.Errorf("first registry = %v, want 1", got)
	}
	if got := testutil.ToFloat64(second.authRejections.WithLabelValues("Expired")); got != 0 {
		t.Errorf("second registry = %v, want 0; the counters are shared", got)
	}
}

// TestARefusedPlacementIsCountedLikeAGrantedOne is the point of driving these
// from the audit trail. A rate that only counts what worked hides the change
// worth alerting on.
func TestARefusedPlacementIsCountedLikeAGrantedOne(t *testing.T) {
	m, reg := newMetrics(t)

	m.Notify(t.Context(), audit.Record{
		Event: audit.EventPlacement, Outcome: audit.OutcomeGranted,
		Policy: "app-prod", Cell: "prod-euw1",
	})
	m.Notify(t.Context(), audit.Record{
		Event: audit.EventPlacement, Outcome: audit.OutcomeRefused,
		Reason: refusal.NoPolicy,
	})
	m.Notify(t.Context(), audit.Record{
		Event: audit.EventPlacement, Outcome: audit.OutcomeDryRun,
		Policy: "app-prod", Cell: "prod-euw1",
	})

	want := `
# HELP cellcast_placement_requests_total Placement decisions, by outcome, refusal reason, governing policy and chosen cell.
# TYPE cellcast_placement_requests_total counter
cellcast_placement_requests_total{cell="",outcome="refused",policy="",reason="NoPolicy"} 1
cellcast_placement_requests_total{cell="prod-euw1",outcome="dry-run",policy="app-prod",reason=""} 1
cellcast_placement_requests_total{cell="prod-euw1",outcome="granted",policy="app-prod",reason=""} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "cellcast_placement_requests_total"); err != nil {
		t.Error(err)
	}
}

func TestMintsAreCountedByProviderAndFailuresByReason(t *testing.T) {
	m, reg := newMetrics(t)

	m.Notify(t.Context(), audit.Record{
		Event: audit.EventMint, Outcome: audit.OutcomeGranted,
		Provider: "eks", Env: "prod", GrantedTTL: 15 * time.Minute,
	})
	m.Notify(t.Context(), audit.Record{
		Event: audit.EventMint, Outcome: audit.OutcomeRefused,
		Provider: "eks", Reason: refusal.MintFailed,
	})

	want := `
# HELP cellcast_tokens_minted_total Credentials issued, by trust provider and the cell's env label.
# TYPE cellcast_tokens_minted_total counter
cellcast_tokens_minted_total{env="prod",provider="eks"} 1
# HELP cellcast_mint_failures_total Mints that failed after placement had already chosen a cell.
# TYPE cellcast_mint_failures_total counter
cellcast_mint_failures_total{provider="eks",reason="MintFailed"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"cellcast_tokens_minted_total", "cellcast_mint_failures_total"); err != nil {
		t.Error(err)
	}

	// A failed mint issued nothing, so it must not appear in the lifetime
	// histogram: an observation of zero would drag the distribution down and
	// make a broken provider look like it was issuing very short credentials.
	// One observation summing to 900 is the granted 15m and nothing else.
	ttl := `
# HELP cellcast_token_ttl_seconds Granted credential lifetime, which is not necessarily the requested one.
# TYPE cellcast_token_ttl_seconds histogram
cellcast_token_ttl_seconds_bucket{le="600"} 0
cellcast_token_ttl_seconds_bucket{le="900"} 1
cellcast_token_ttl_seconds_bucket{le="1200"} 1
cellcast_token_ttl_seconds_bucket{le="1800"} 1
cellcast_token_ttl_seconds_bucket{le="2700"} 1
cellcast_token_ttl_seconds_bucket{le="3600"} 1
cellcast_token_ttl_seconds_bucket{le="7200"} 1
cellcast_token_ttl_seconds_bucket{le="14400"} 1
cellcast_token_ttl_seconds_bucket{le="28800"} 1
cellcast_token_ttl_seconds_bucket{le="+Inf"} 1
cellcast_token_ttl_seconds_sum 900
cellcast_token_ttl_seconds_count 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(ttl), "cellcast_token_ttl_seconds"); err != nil {
		t.Error(err)
	}
}

// TestNilMetricsIsSafe pins the reason no call site guards. A hub with no
// metrics endpoint still decides and still mints.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics

	m.AuthRejected("Expired")
	m.ObservePlacement("granted", time.Second)
	m.Notify(t.Context(), audit.Record{Event: audit.EventMint, Outcome: audit.OutcomeGranted})
}

func TestCapacityCollector(t *testing.T) {
	index := capacity.New(capacity.Options{Staleness: time.Hour})
	report(t, index, "prod-euw1", 3000, 10000)

	reg := prometheus.NewRegistry()
	if err := reg.Register(NewCapacityCollector(index)); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}

	want := `
# HELP cellcast_cluster_capacity_ratio How full a cell is, as the larger of its CPU and memory commitment, in [0,1]. Absent for a cell whose capacity is stale.
# TYPE cellcast_cluster_capacity_ratio gauge
cellcast_cluster_capacity_ratio{cell="prod-euw1"} 0.3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "cellcast_cluster_capacity_ratio"); err != nil {
		t.Error(err)
	}
	if got := testutil.CollectAndCount(reg, "cellcast_capacity_staleness_seconds"); got != 1 {
		t.Errorf("staleness series = %d, want one per known cell", got)
	}

	// The configured window is published so that an alert derives its threshold
	// instead of hardcoding one. CellcastCapacityStale is written against it, so
	// this series disappearing silently disarms that alert.
	window := `
# HELP cellcast_capacity_staleness_window_seconds The configured window after which a cell's capacity stops being usable for scoring.
# TYPE cellcast_capacity_staleness_window_seconds gauge
cellcast_capacity_staleness_window_seconds 3600
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(window), "cellcast_capacity_staleness_window_seconds"); err != nil {
		t.Error(err)
	}
}

// TestAStaleCellReportsStalenessAndNoRatio is the failure mode this metric
// exists for. A cell whose agent died must not keep publishing the utilisation
// it had when it died, because a dashboard reads that as current and the
// placement engine does not.
func TestAStaleCellReportsStalenessAndNoRatio(t *testing.T) {
	// One clock behind both the index and the collector, so "two hours later"
	// means the same thing to the health check and to the reported age.
	now := time.Now()
	clock := func() time.Time { return now }

	index := capacity.New(capacity.Options{
		Staleness: time.Hour, Retention: 24 * time.Hour, Now: clock,
	})
	report(t, index, "prod-euw1", 3000, 10000)

	collector := NewCapacityCollector(index).(*capacityCollector)
	collector.now = clock
	now = now.Add(2 * time.Hour)

	reg := prometheus.NewRegistry()
	if err := reg.Register(collector); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}

	if got := testutil.CollectAndCount(reg, "cellcast_cluster_capacity_ratio"); got != 0 {
		t.Errorf("a stale cell still publishes a utilisation ratio (%d series)", got)
	}
	if got := testutil.CollectAndCount(reg, "cellcast_capacity_staleness_seconds"); got != 1 {
		t.Errorf("staleness series = %d, want the series the alert fires on", got)
	}
}

func TestClusterStateCollector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cellcastv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		cell("prod-euw1", cellcastv1alpha1.ClusterStateDraining),
		// State unset, so it normalizes to LIVE the same way placement does.
		cell("prod-euw2", ""),
	).Build()

	reg := prometheus.NewRegistry()
	if err := reg.Register(NewClusterStateCollector(k8s, testNamespace, discardLogger())); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}

	want := `
# HELP cellcast_cluster_state 1 for the cell's current placement state, 0 for the others.
# TYPE cellcast_cluster_state gauge
cellcast_cluster_state{cell="prod-euw1",state="DARK"} 0
cellcast_cluster_state{cell="prod-euw1",state="DRAINING"} 1
cellcast_cluster_state{cell="prod-euw1",state="LIVE"} 0
cellcast_cluster_state{cell="prod-euw2",state="DARK"} 0
cellcast_cluster_state{cell="prod-euw2",state="DRAINING"} 0
cellcast_cluster_state{cell="prod-euw2",state="LIVE"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "cellcast_cluster_state"); err != nil {
		t.Error(err)
	}
}

// TestAnUnreadableRegistryEmitsNothing checks the collector does not publish
// zeroes it cannot back up. Every cell reporting 0 for every state reads as a
// fleet-wide event, and a cache that will not list is not one.
func TestAnUnreadableRegistryEmitsNothing(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(NewClusterStateCollector(failingReader{}, testNamespace, discardLogger())); err != nil {
		t.Fatalf("Register() = %v, want nil", err)
	}
	if got := testutil.CollectAndCount(reg, "cellcast_cluster_state"); got != 0 {
		t.Errorf("collected %d series from an unreadable registry, want 0", got)
	}
}

type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errUnreadable
}

func report(t *testing.T, index *capacity.Registry, name string, committed, allocatable int64) {
	t.Helper()
	err := index.Report(capacity.Report{
		Cell:                   name,
		Nodes:                  3,
		CPUMilliAllocatable:    allocatable,
		CPUMilliCommitted:      committed,
		MemoryBytesAllocatable: allocatable,
		MemoryBytesCommitted:   0,
	})
	if err != nil {
		t.Fatalf("Report(%s) = %v, want nil", name, err)
	}
}

func cell(name string, state cellcastv1alpha1.ClusterState) *cellcastv1alpha1.Cluster {
	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       "https://" + name + ".example.test",
			Provider:       cellcastv1alpha1.ProviderEKS,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "t1"},
			State:          state,
		},
	}
}

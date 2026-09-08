package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
)

// Fleet state is collected at scrape time rather than written as gauges when it
// changes.
//
// A gauge set on write is wrong twice over here. It keeps reporting a cell that
// has been decommissioned, because nothing deletes the series; and it reports
// the last utilisation of a cell whose agent died, which is precisely the
// number that must not steer anything (docs/architecture.md ADR-002). Deriving
// both from the live index and the informer cache on each scrape cannot go
// stale, and costs a map walk.
var (
	capacityRatioDesc = prometheus.NewDesc(
		"cellcast_cluster_capacity_ratio",
		"How full a cell is, as the larger of its CPU and memory commitment, in [0,1]. Absent for a cell whose capacity is stale.",
		[]string{"cell"}, nil,
	)

	stalenessDesc = prometheus.NewDesc(
		"cellcast_capacity_staleness_seconds",
		"Seconds since the hub last accepted a capacity report for a cell.",
		[]string{"cell"}, nil,
	)

	stalenessWindowDesc = prometheus.NewDesc(
		"cellcast_capacity_staleness_window_seconds",
		"The configured window after which a cell's capacity stops being usable for scoring.",
		nil, nil,
	)

	clusterStateDesc = prometheus.NewDesc(
		"cellcast_cluster_state",
		"1 for the cell's current placement state, 0 for the others.",
		[]string{"cell", "state"}, nil,
	)
)

// capacityCollector reports the in-memory capacity index.
type capacityCollector struct {
	index *capacity.Registry
	now   func() time.Time
}

// NewCapacityCollector reports utilisation and staleness per cell.
func NewCapacityCollector(index *capacity.Registry) prometheus.Collector {
	return &capacityCollector{index: index, now: time.Now}
}

func (c *capacityCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- capacityRatioDesc
	ch <- stalenessDesc
	ch <- stalenessWindowDesc
}

func (c *capacityCollector) Collect(ch chan<- prometheus.Metric) {
	if c.index == nil {
		return
	}
	now := c.now()

	// The window is published so an alert can be written against it rather than
	// against a number copied from a flag default. A rule saying "stale for
	// twice as long as this hub considers usable" retunes itself when the
	// operator changes --capacity-staleness; one saying "> 180" does not, and
	// nobody remembers to.
	ch <- prometheus.MustNewConstMetric(
		stalenessWindowDesc, prometheus.GaugeValue, c.index.Staleness().Seconds())

	for cell, entry := range c.index.Snapshot() {
		// Staleness is emitted for every cell the index knows about, including
		// the stale ones. It is the series the alert fires on, so it must exist
		// exactly when there is something to alert about.
		ch <- prometheus.MustNewConstMetric(
			stalenessDesc, prometheus.GaugeValue, now.Sub(entry.ObservedAt).Seconds(), cell)

		// Utilisation is emitted only while it is usable. A stale ratio is a
		// number a dashboard will read as current and a placement will not,
		// and the two disagreeing is worse than a gap in the graph.
		if entry.Health == capacity.HealthFresh {
			ch <- prometheus.MustNewConstMetric(
				capacityRatioDesc, prometheus.GaugeValue, entry.Utilisation, cell)
		}
	}
}

// clusterStateCollector reports the declared state of every registered cell.
type clusterStateCollector struct {
	reader    client.Reader
	namespace string
	log       *slog.Logger
}

// NewClusterStateCollector reports each cell's placement state.
//
// reader is the manager's cached client, so a scrape costs no API server
// traffic no matter how often Prometheus asks.
func NewClusterStateCollector(reader client.Reader, namespace string, log *slog.Logger) prometheus.Collector {
	return &clusterStateCollector{reader: reader, namespace: namespace, log: log}
}

func (c *clusterStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- clusterStateDesc
}

func (c *clusterStateCollector) Collect(ch chan<- prometheus.Metric) {
	if c.reader == nil {
		return
	}

	var cells cellcastv1alpha1.ClusterList
	if err := c.reader.List(context.Background(), &cells, client.InNamespace(c.namespace)); err != nil {
		// A scrape that cannot read the registry emits nothing rather than
		// zeroes. Zeroes would read as "every cell left this state", which is a
		// fleet-wide event, and an unreadable cache is not one.
		c.log.Warn("cluster state metrics unavailable", slog.Any("error", err))
		return
	}

	// Every state gets a series, so `cellcast_cluster_state == 1` selects the
	// current one and a transition moves a 1 to a 0 rather than leaving the old
	// series to be read as still true.
	states := []cellcastv1alpha1.ClusterState{
		cellcastv1alpha1.ClusterStateLive,
		cellcastv1alpha1.ClusterStateDark,
		cellcastv1alpha1.ClusterStateDraining,
	}

	for i := range cells.Items {
		cell := &cells.Items[i]
		// The declared state, normalized the same way placement normalizes it.
		// Reporting status.observedState instead would make the metric lag a
		// reconcile behind the decision the placement engine is already making.
		observed := cell.Spec.State.Normalize()
		for _, state := range states {
			value := 0.0
			if state == observed {
				value = 1
			}
			ch <- prometheus.MustNewConstMetric(
				clusterStateDesc, prometheus.GaugeValue, value, cell.Name, string(state))
		}
	}
}

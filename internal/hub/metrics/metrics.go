// Package metrics exposes the hub's Prometheus surface.
//
// A service in the deploy critical path that cannot be monitored will not be
// adopted, and the number an adopter cares about first is placement latency,
// because it is added to every deploy in the estate.
//
// Nothing here is a package-level variable. Collectors are fields on a value
// built against a registry, so a test gets its own set instead of inheriting
// whatever counts an earlier test left behind, and so registering twice is a
// returned error rather than a panic inside init.
//
// The counters are driven by the audit trail rather than by a second pass over
// the placement handler: every fact they need is already on an audit.Record,
// and one instrumentation point cannot disagree with itself
// (docs/architecture.md ADR-010).
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
)

// Metrics holds the hub's collectors.
//
// The zero value is not usable; every method is safe on a nil pointer instead,
// so a hub built without metrics does not need a guard at each call site.
type Metrics struct {
	placementRequests *prometheus.CounterVec
	placementDuration *prometheus.HistogramVec
	tokensMinted      *prometheus.CounterVec
	mintFailures      *prometheus.CounterVec
	tokenTTL          prometheus.Histogram
	authRejections    *prometheus.CounterVec
	warm              prometheus.Gauge
}

// New builds the hub's collectors and registers them with reg.
//
// reg is a parameter rather than controller-runtime's global registry so that a
// test can gather from a registry it owns. The hub binary passes the global one,
// which is what the manager's metrics endpoint serves.
func New(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		placementRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cellcast_placement_requests_total",
			Help: "Placement decisions, by outcome, refusal reason, governing policy and chosen cell.",
		}, []string{"outcome", "reason", "policy", "cell"}),

		placementDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cellcast_placement_duration_seconds",
			Help: "Wall time to answer a placement request, including the mint.",
			// Bucketed for a request that should take milliseconds and is
			// bounded by one API call to a spoke. The top bucket is where a
			// pipeline starts noticing cellcast in its build time.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"outcome"}),

		tokensMinted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cellcast_tokens_minted_total",
			Help: "Credentials issued, by trust provider and the cell's env label.",
		}, []string{"provider", "env"}),

		mintFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cellcast_mint_failures_total",
			Help: "Mints that failed after placement had already chosen a cell.",
		}, []string{"provider", "reason"}),

		tokenTTL: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cellcast_token_ttl_seconds",
			Help: "Granted credential lifetime, which is not necessarily the requested one.",
			// The Kubernetes TokenRequest floor is ten minutes, so nothing
			// lands below 600. The spread above it is what shows a policy
			// quietly granting hours.
			Buckets: []float64{600, 900, 1200, 1800, 2700, 3600, 7200, 14400, 28800},
		}),

		authRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cellcast_auth_rejections_total",
			Help: "Requests rejected before reaching a handler, by why the token was refused.",
		}, []string{"reason"}),

		warm: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cellcast_hub_warm",
			Help: "1 when this replica's capacity index covers the fleet, 0 while it is still cold and refusing placements.",
		}),
	}

	for _, c := range []prometheus.Collector{
		m.placementRequests, m.placementDuration, m.tokensMinted,
		m.mintFailures, m.tokenTTL, m.authRejections, m.warm,
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Notify records one audit record. It satisfies audit.Notifier.
//
// Refusals are counted with the same fields as successes. A rate that only
// counts what worked hides exactly the change worth alerting on.
func (m *Metrics) Notify(_ context.Context, rec audit.Record) {
	if m == nil {
		return
	}

	switch rec.Event {
	case audit.EventPlacement:
		m.placementRequests.WithLabelValues(
			string(rec.Outcome), string(rec.Reason), rec.Policy, rec.Cell).Inc()

	case audit.EventMint:
		if rec.Outcome != audit.OutcomeGranted {
			m.mintFailures.WithLabelValues(rec.Provider, string(rec.Reason)).Inc()
			return
		}
		m.tokensMinted.WithLabelValues(rec.Provider, rec.Env).Inc()
		m.tokenTTL.Observe(rec.GrantedTTL.Seconds())
	}
}

// ObservePlacement records how long a placement request took.
//
// Separate from Notify because a duration is a property of the request rather
// than of anything the hub decided, and putting it on the audit record would
// make the trail's shape depend on what a dashboard wanted this week.
func (m *Metrics) ObservePlacement(outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.placementDuration.WithLabelValues(outcome).Observe(d.Seconds())
}

// SetWarm records whether this replica can score a placement.
//
// Per replica and not aggregated: the number that matters is the
// minimum across the Deployment, because one cold replica in a Service of three
// refuses a third of the estate's deploys while the average still looks fine.
func (m *Metrics) SetWarm(warm bool) {
	if m == nil {
		return
	}
	if warm {
		m.warm.Set(1)
		return
	}
	m.warm.Set(0)
}

// AuthRejected records a request refused by the authentication middleware.
//
// A spike here is either a broken pipeline or somebody probing, and the reason
// is what separates the two: a run of `TokenExpired` is a clock or a retry loop,
// a run of `IssuerNotAllowed` is not.
func (m *Metrics) AuthRejected(reason string) {
	if m == nil {
		return
	}
	m.authRejections.WithLabelValues(reason).Inc()
}

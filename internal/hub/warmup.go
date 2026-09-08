package hub

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
)

// warmupPollInterval is how often a starting replica re-asks whether the fleet
// has checked in. Short, because the whole wait is measured in seconds and the
// check is two in-memory reads.
const warmupPollInterval = time.Second

// WarmupCheck reports whether this replica's capacity index covers the fleet,
// and names the cells it has not heard from.
//
// The names are for the operator, not for control flow: a rollout that stalls
// on warmup should say which cell it is waiting for, or the wait is
// indistinguishable from a hang.
type WarmupCheck func(ctx context.Context) (warm bool, missing []string)

// FleetCoverage builds the check the hub runs in production: every registered
// cell that is expected to report has a fresh entry in the capacity index.
//
// Coverage rather than "any capacity at all" because the two differ exactly
// where it matters. A replica holding one cell out of six is not warm; it is a
// replica that will place every deploy on that one cell, having silently
// excluded the other five as Unknown.
func FleetCoverage(reader client.Reader, index *capacity.Registry, namespace string) WarmupCheck {
	return func(ctx context.Context) (bool, []string) {
		var list cellcastv1alpha1.ClusterList
		if err := reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			// A replica that cannot read the registry cannot know what it is
			// missing, which is not the same as missing nothing.
			return false, nil
		}

		snapshot := index.Snapshot()
		var missing []string
		for i := range list.Items {
			cluster := &list.Items[i]
			if !expectedToReport(cluster) {
				continue
			}
			if entry, ok := snapshot[cluster.Name]; !ok || entry.Health != capacity.HealthFresh {
				missing = append(missing, cluster.Name)
			}
		}
		slices.Sort(missing)
		return len(missing) == 0, missing
	}
}

// expectedToReport reports whether a cell's silence should hold up readiness.
func expectedToReport(cluster *cellcastv1alpha1.Cluster) bool {
	// A cell with no declared reporter accepts no heartbeats at all, so waiting
	// for one is waiting for something the hub itself refuses (ADR-009).
	if cluster.Spec.Reporter == nil {
		return false
	}
	// A draining cell takes no new placements, so its capacity cannot change a
	// decision. Holding up a hub rollout for a cell that is deliberately being
	// emptied is the wrong way round: draining a cell is the moment you most
	// want the hub healthy.
	return cluster.Spec.State != cellcastv1alpha1.ClusterStateDraining
}

// trackWarmth polls until the capacity index covers the fleet, then latches.
//
// It keeps polling past the readiness deadline on purpose. A replica that went
// ready while still cold has to be able to notice the heartbeats that its own
// readiness is what allowed to arrive; stopping the poll at the deadline is how
// that hub would stay cold for the rest of its life.
func (s *Server) trackWarmth(ctx context.Context, warm chan<- struct{}) {
	if s.warmupCheck == nil {
		close(warm)
		return
	}

	tick := time.NewTicker(warmupPollInterval)
	defer tick.Stop()

	for {
		ok, missing := s.warmupCheck(ctx)
		if ok {
			s.markWarm()
			close(warm)
			return
		}
		s.lastMissing.Store(&missing)

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// markWarm latches this replica as able to score a placement.
//
// One-way. Capacity going stale later is the placement engine's problem and it
// already has an answer for it: the affected cells become Unknown and are
// excluded, and a decision that can no longer be made is refused with
// CapacityUnknown. Letting warmth fall back to false would instead pull every
// replica out of the Service the moment the fleet had a bad minute, turning a
// degraded decision into no hub at all.
func (s *Server) markWarm() {
	if s.warm.Swap(true) {
		return
	}
	s.metrics.SetWarm(true)
	s.log.Info("capacity index covers the fleet")
}

// awaitWarmth blocks until this replica is warm or the warmup deadline passes.
func (s *Server) awaitWarmth(ctx context.Context) {
	warm := make(chan struct{})
	go s.trackWarmth(ctx, warm)

	deadline := time.NewTimer(s.cfg.WarmupTimeout)
	defer deadline.Stop()

	select {
	case <-warm:
	case <-ctx.Done():
	case <-deadline.C:
		// Ready while cold is deliberate, and it is the only way out of a
		// deadlock the fleet can otherwise reach: agents heartbeat through the
		// Service, a Service routes only to ready pods, so a fleet whose hub
		// replicas all restarted at once has no path back to warm until one of
		// them goes ready without waiting to be asked. Placements are refused
		// until the heartbeats land, which is the fail-closed half of the same
		// decision.
		s.log.Warn("becoming ready before capacity covers the fleet",
			slog.Duration("waited", s.cfg.WarmupTimeout),
			slog.Any("cells_not_reporting", s.missingCells()),
		)
	}
}

// missingCells returns the cells the last warmup check did not see.
func (s *Server) missingCells() []string {
	if missing := s.lastMissing.Load(); missing != nil {
		return *missing
	}
	return nil
}

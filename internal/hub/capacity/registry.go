// Package capacity holds the hub's view of how loaded each cell is.
//
// Capacity is the only high-write data in cellcast and it is worthless the
// moment it is stale, which is exactly why it never touches etcd. It lives in
// memory in the hub process and is rebuilt from agent heartbeats on restart
// (docs/architecture.md ADR-002). A hub restart costs one heartbeat interval of
// blindness, during which affected cells are Unknown and therefore excluded
// rather than mis-scored.
//
// This package deliberately knows nothing about Kubernetes, HTTP or policy. It
// is a bounded map with a clock, and keeping it that way is what makes the
// staleness guard testable without a cluster.
package capacity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

// Defaults for a fleet whose agents heartbeat every 30 seconds.
const (
	// DefaultStaleness is three missed heartbeats. Shorter reacts to a dead
	// agent faster but starts excluding healthy cells over one dropped packet.
	DefaultStaleness = 90 * time.Second

	// DefaultRetention is how long an Unknown entry is kept before it is
	// dropped entirely. Keeping it is what lets the hub distinguish "this cell
	// went quiet" from "this cell was never here", which are different
	// operator problems.
	DefaultRetention = time.Hour

	// DefaultMaxCells bounds the index. The ingest path already refuses reports
	// for cells that are not registered, so reaching this means something is
	// wrong; the bound is here so that "something is wrong" is a rejected
	// report rather than an evicted hub (docs/threat-model.md T-06).
	DefaultMaxCells = 1000

	// DefaultPruneInterval is how often expired entries are reclaimed.
	DefaultPruneInterval = time.Minute
)

// Health is whether a cell's capacity is usable for scoring.
type Health string

const (
	// HealthFresh means the last report is inside the staleness window.
	HealthFresh Health = "Fresh"

	// HealthUnknown means no usable recent report. An Unknown cell is excluded
	// from scoring and is never read as empty. This is the whole point of the
	// package: naive least-loaded treats a missing entry as zero utilisation,
	// which is the best possible score, so the first cluster to break badly
	// enough that its agent stops reporting would become the target for every
	// deploy in the estate.
	HealthUnknown Health = "Unknown"
)

// ErrTooManyCells reports that the index is full.
var ErrTooManyCells = errors.New("capacity index is full")

// Report is one agent heartbeat.
//
// Units are explicit in the field names because the two plausible readings of
// "cpu: 4" differ by a factor of a thousand, and a placement decision made on
// the wrong one is silently wrong rather than loudly broken.
type Report struct {
	// Cell is the Cluster this report describes.
	Cell string

	// Nodes is the number of schedulable nodes.
	Nodes int

	// CPUMilliAllocatable is the sum of node allocatable CPU, in millicores.
	CPUMilliAllocatable int64
	// CPUMilliCommitted is the sum of pod CPU requests, in millicores.
	//
	// Requests rather than usage on purpose: the scheduler places on requests,
	// so requests are what determines whether the next deploy fits. A cell can
	// be at 20% CPU usage and completely unschedulable.
	CPUMilliCommitted int64

	// MemoryBytesAllocatable is the sum of node allocatable memory.
	MemoryBytesAllocatable int64
	// MemoryBytesCommitted is the sum of pod memory requests.
	MemoryBytesCommitted int64

	// Pods is the number of running pods.
	Pods int
	// PodCapacity is the sum of node pod capacity.
	PodCapacity int
}

// Validate reports whether a heartbeat is usable.
//
// A report is rejected rather than clamped. Clamping a negative or nonsensical
// value produces a plausible-looking utilisation that steers real deploys, and
// the agent that sent it never finds out it is broken.
func (r Report) Validate() error {
	if r.Cell == "" {
		return errors.New("cell is required")
	}
	negatives := map[string]int64{
		"nodes":                  int64(r.Nodes),
		"cpuMilliAllocatable":    r.CPUMilliAllocatable,
		"cpuMilliCommitted":      r.CPUMilliCommitted,
		"memoryBytesAllocatable": r.MemoryBytesAllocatable,
		"memoryBytesCommitted":   r.MemoryBytesCommitted,
		"pods":                   int64(r.Pods),
		"podCapacity":            int64(r.PodCapacity),
	}
	for _, name := range slices.Sorted(maps.Keys(negatives)) {
		if negatives[name] < 0 {
			return fmt.Errorf("%s must not be negative, got %d", name, negatives[name])
		}
	}
	if r.CPUMilliAllocatable == 0 && r.MemoryBytesAllocatable == 0 {
		// A cell with nothing allocatable is not an empty cell, it is a cell
		// whose agent cannot see its nodes. Accepting it would score it as
		// perfectly idle.
		return errors.New("cpuMilliAllocatable and memoryBytesAllocatable must not both be zero")
	}
	return nil
}

// Entry is a stored report plus what the registry derived from it.
type Entry struct {
	Report

	// ObservedAt is when the report was received, by the hub's clock. Agent
	// clocks are not trusted for staleness: a cell with a skewed clock could
	// otherwise report itself permanently fresh.
	ObservedAt time.Time

	// Health is computed at read time, never stored.
	Health Health

	// Utilisation is the fraction of the cell that is committed, in [0,1].
	// Meaningful only when Health is Fresh.
	Utilisation float64
}

// Utilisation returns how full a cell is, as the larger of its CPU and memory
// pressure.
//
// The maximum rather than the mean, because a cell at 95% memory and 10% CPU is
// not half loaded, it is nearly full. Averaging the two is how a scheduler
// keeps sending work to a cell that is one pod away from evicting things.
//
// Dimensions with zero allocatable are skipped rather than counted as zero
// pressure, so a cell reporting only memory is scored on memory instead of
// being scored as half empty.
func (r Report) Utilisation() float64 {
	worst := 0.0
	if r.CPUMilliAllocatable > 0 {
		worst = max(worst, float64(r.CPUMilliCommitted)/float64(r.CPUMilliAllocatable))
	}
	if r.MemoryBytesAllocatable > 0 {
		worst = max(worst, float64(r.MemoryBytesCommitted)/float64(r.MemoryBytesAllocatable))
	}
	// Committed can exceed allocatable on an overcommitted cell. Reporting >1
	// rather than clamping keeps the ordering between two overcommitted cells
	// meaningful.
	return worst
}

// Options configures a Registry.
type Options struct {
	// Staleness is how long a report stays usable.
	Staleness time.Duration
	// Retention is how long an Unknown entry is kept before being dropped.
	Retention time.Duration
	// MaxCells bounds the index.
	MaxCells int
	// PruneInterval is how often Run reclaims expired entries.
	PruneInterval time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Staleness <= 0 {
		o.Staleness = DefaultStaleness
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.MaxCells <= 0 {
		o.MaxCells = DefaultMaxCells
	}
	if o.PruneInterval <= 0 {
		o.PruneInterval = DefaultPruneInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// Retaining an entry for less time than it stays fresh would delete cells
	// that are working.
	o.Retention = max(o.Retention, o.Staleness)
	return o
}

// Registry is the in-memory capacity index.
//
// Safe for concurrent use: every placement request reads it and every agent in
// the fleet writes to it.
type Registry struct {
	opts Options

	mu      sync.RWMutex
	entries map[string]stored
	// unknown tracks which cells were Unknown at the last prune, so the
	// transition into Unknown can be logged once rather than every tick.
	unknown map[string]bool
}

type stored struct {
	report     Report
	observedAt time.Time
}

// New returns an empty registry.
func New(opts Options) *Registry {
	return &Registry{
		opts:    opts.withDefaults(),
		entries: make(map[string]stored),
		unknown: make(map[string]bool),
	}
}

// Staleness returns the configured staleness window.
func (r *Registry) Staleness() time.Duration { return r.opts.Staleness }

// Report records a heartbeat.
//
// The caller is responsible for having established that the cell is registered
// and that the reporter is entitled to speak for it. This package cannot check
// either without knowing about Kubernetes.
func (r *Registry) Report(rep Report) error {
	if err := rep.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.entries[rep.Cell]; !exists && len(r.entries) >= r.opts.MaxCells {
		return fmt.Errorf("%w (%d cells)", ErrTooManyCells, r.opts.MaxCells)
	}

	r.entries[rep.Cell] = stored{report: rep, observedAt: r.opts.Now()}
	delete(r.unknown, rep.Cell)
	return nil
}

// Lookup returns the current entry for a cell.
//
// The second return is whether anything has ever been reported for the cell,
// not whether it is usable. A cell that has gone quiet returns true with
// Health Unknown, which is a different situation from a cell nobody has ever
// reported for, and callers need to be able to tell them apart.
func (r *Registry) Lookup(cell string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s, ok := r.entries[cell]
	if !ok {
		return Entry{}, false
	}
	return r.entryLocked(s, r.opts.Now()), true
}

// Snapshot returns every entry, keyed by cell.
//
// One consistent read for a placement decision. Calling Lookup per candidate
// would let the fleet change underneath a single scoring pass.
func (r *Registry) Snapshot() map[string]Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.opts.Now()
	out := make(map[string]Entry, len(r.entries))
	for cell, s := range r.entries {
		out[cell] = r.entryLocked(s, now)
	}
	return out
}

// entryLocked derives the read-time view of a stored report.
//
// Health is computed here rather than written by the prune loop on purpose. A
// stalled or crashed sweeper must not be able to leave a stale entry looking
// fresh, which is the failure mode that turns the staleness guard into
// decoration.
func (r *Registry) entryLocked(s stored, now time.Time) Entry {
	e := Entry{Report: s.report, ObservedAt: s.observedAt, Health: HealthFresh}
	if now.Sub(s.observedAt) > r.opts.Staleness {
		e.Health = HealthUnknown
		return e
	}
	e.Utilisation = s.report.Utilisation()
	return e
}

// Forget drops a cell's entry.
//
// Called when a Cluster is deleted, so a decommissioned cell does not sit in
// the index until retention expires.
func (r *Registry) Forget(cell string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, cell)
	delete(r.unknown, cell)
}

// Stats summarises the index.
type Stats struct {
	Fresh   int
	Unknown int
}

// Stats returns the current fresh and unknown counts.
func (r *Registry) Stats() Stats {
	var out Stats
	for _, e := range r.Snapshot() {
		if e.Health == HealthFresh {
			out.Fresh++
		} else {
			out.Unknown++
		}
	}
	return out
}

// Prune drops entries past the retention window and returns the cells that
// have newly become Unknown since the last call.
func (r *Registry) Prune() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.opts.Now()
	var wentUnknown []string
	for cell, s := range r.entries {
		age := now.Sub(s.observedAt)
		if age > r.opts.Retention {
			delete(r.entries, cell)
			delete(r.unknown, cell)
			continue
		}
		if age > r.opts.Staleness && !r.unknown[cell] {
			r.unknown[cell] = true
			wentUnknown = append(wentUnknown, cell)
		}
	}
	slices.Sort(wentUnknown)
	return wentUnknown
}

// Run prunes on a ticker until ctx is cancelled.
//
// The log line on a cell going Unknown is the alert surface until ENG-178 adds
// metrics. A cell dropping out of scoring silently is the failure the whole
// staleness guard exists to make visible, so it must not be visible only to
// whoever is reading a placement trace at the time.
func (r *Registry) Run(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(r.opts.PruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, cell := range r.Prune() {
				log.Warn("cell capacity is stale and excluded from placement",
					slog.String("cell", cell),
					slog.Duration("staleness_window", r.opts.Staleness),
				)
			}
		}
	}
}

package capacity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a manual clock. Capacity is entirely about the passage of time, so
// every interesting assertion in this package is unreachable with time.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func healthyReport(cell string) Report {
	return Report{
		Cell:                   cell,
		Nodes:                  6,
		CPUMilliAllocatable:    24000,
		CPUMilliCommitted:      6000,
		MemoryBytesAllocatable: 96 << 30,
		MemoryBytesCommitted:   24 << 30,
		Pods:                   80,
		PodCapacity:            660,
	}
}

func testRegistry(t *testing.T, c *clock) *Registry {
	t.Helper()
	return New(Options{Staleness: 90 * time.Second, Retention: time.Hour, Now: c.Now})
}

// TestStaleCellIsUnknownNotEmpty is the single most important test in this
// package. Naive least-loaded reads a missing entry as zero utilisation, which
// is the best possible score, so without this the first cluster to break badly
// enough that its agent stops reporting becomes the target for every deploy in
// the estate (docs/architecture.md ADR-002).
func TestStaleCellIsUnknownNotEmpty(t *testing.T) {
	c := newClock()
	r := testRegistry(t, c)

	if err := r.Report(healthyReport("c1")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}

	entry, ok := r.Lookup("c1")
	if !ok || entry.Health != HealthFresh {
		t.Fatalf("Lookup() = %+v, %v, want a fresh entry", entry, ok)
	}

	c.advance(89 * time.Second)
	if entry, _ := r.Lookup("c1"); entry.Health != HealthFresh {
		t.Errorf("health inside the window = %q, want %q", entry.Health, HealthFresh)
	}

	c.advance(2 * time.Second)
	entry, ok = r.Lookup("c1")
	if !ok {
		t.Fatal("Lookup() reported the cell as absent, want it present but Unknown")
	}
	if entry.Health != HealthUnknown {
		t.Errorf("health past the window = %q, want %q", entry.Health, HealthUnknown)
	}
	if entry.Utilisation != 0 {
		t.Errorf("utilisation = %v; it is only meaningful when Fresh", entry.Utilisation)
	}
}

// TestUnknownIsDistinctFromNeverReported pins the two states apart. "This cell
// went quiet" and "nobody has ever reported for this cell" are different
// operator problems and a caller has to be able to tell them apart.
func TestUnknownIsDistinctFromNeverReported(t *testing.T) {
	c := newClock()
	r := testRegistry(t, c)

	if _, ok := r.Lookup("never"); ok {
		t.Error("Lookup(\"never\") reported present, want absent")
	}

	if err := r.Report(healthyReport("quiet")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}
	c.advance(10 * time.Minute)

	entry, ok := r.Lookup("quiet")
	if !ok {
		t.Fatal("Lookup(\"quiet\") reported absent, want present and Unknown")
	}
	if entry.Health != HealthUnknown {
		t.Errorf("health = %q, want %q", entry.Health, HealthUnknown)
	}
}

// TestHealthIsComputedAtReadTime pins that staleness does not depend on the
// prune loop having run. A sweeper that has stalled or crashed must not be able
// to leave a stale entry looking fresh, which would turn the guard into
// decoration.
func TestHealthIsComputedAtReadTime(t *testing.T) {
	c := newClock()
	r := testRegistry(t, c)
	if err := r.Report(healthyReport("c1")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}

	c.advance(5 * time.Minute)

	// No Prune call anywhere in this test.
	if entry, _ := r.Lookup("c1"); entry.Health != HealthUnknown {
		t.Errorf("health without a prune = %q, want %q", entry.Health, HealthUnknown)
	}
	if entry := r.Snapshot()["c1"]; entry.Health != HealthUnknown {
		t.Errorf("snapshot health without a prune = %q, want %q", entry.Health, HealthUnknown)
	}
}

func TestUtilisationTakesTheWorstDimension(t *testing.T) {
	tests := []struct {
		name   string
		report Report
		want   float64
	}{
		{
			name:   "balanced",
			report: Report{CPUMilliAllocatable: 1000, CPUMilliCommitted: 250, MemoryBytesAllocatable: 1000, MemoryBytesCommitted: 250},
			want:   0.25,
		},
		{
			name: "memory pressure dominates",
			// The averaging bug scores this 0.525 and keeps sending work to a
			// cell that is one pod away from evicting things.
			report: Report{CPUMilliAllocatable: 1000, CPUMilliCommitted: 100, MemoryBytesAllocatable: 1000, MemoryBytesCommitted: 950},
			want:   0.95,
		},
		{
			name:   "cpu pressure dominates",
			report: Report{CPUMilliAllocatable: 1000, CPUMilliCommitted: 800, MemoryBytesAllocatable: 1000, MemoryBytesCommitted: 100},
			want:   0.8,
		},
		{
			name:   "overcommitted reports above one rather than clamping",
			report: Report{CPUMilliAllocatable: 1000, CPUMilliCommitted: 1500, MemoryBytesAllocatable: 1000, MemoryBytesCommitted: 100},
			want:   1.5,
		},
		{
			name: "a dimension with no allocatable is skipped, not scored as empty",
			// Counting the missing dimension as zero pressure would halve this
			// to 0.45 and make a nearly full cell look like the best target.
			report: Report{CPUMilliAllocatable: 0, CPUMilliCommitted: 0, MemoryBytesAllocatable: 1000, MemoryBytesCommitted: 900},
			want:   0.9,
		},
		{
			name:   "idle",
			report: Report{CPUMilliAllocatable: 1000, MemoryBytesAllocatable: 1000},
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.report.Utilisation(); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Utilisation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReportValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Report)
		wantErr string
	}{
		{name: "healthy", mutate: func(*Report) {}},
		{name: "no cell", mutate: func(r *Report) { r.Cell = "" }, wantErr: "cell is required"},
		{name: "negative committed cpu", mutate: func(r *Report) { r.CPUMilliCommitted = -1 }, wantErr: "cpuMilliCommitted"},
		{name: "negative nodes", mutate: func(r *Report) { r.Nodes = -1 }, wantErr: "nodes"},
		{name: "negative pods", mutate: func(r *Report) { r.Pods = -3 }, wantErr: "pods"},
		{
			// A cell with nothing allocatable is not empty, it is a cell whose
			// agent cannot see its nodes. Storing it would score it as idle.
			name: "nothing allocatable",
			mutate: func(r *Report) {
				r.CPUMilliAllocatable = 0
				r.MemoryBytesAllocatable = 0
			},
			wantErr: "must not both be zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := healthyReport("c1")
			tt.mutate(&rep)

			err := rep.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if got := err.Error(); !strings.Contains(got, tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", got, tt.wantErr)
			}
		})
	}
}

// TestRejectedReportDoesNotReplaceAGoodOne pins that a broken agent cannot
// blank out a cell's last known good capacity.
func TestRejectedReportDoesNotReplaceAGoodOne(t *testing.T) {
	c := newClock()
	r := testRegistry(t, c)
	if err := r.Report(healthyReport("c1")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}

	bad := healthyReport("c1")
	bad.CPUMilliCommitted = -5
	if err := r.Report(bad); err == nil {
		t.Fatal("Report() accepted a negative committed value, want an error")
	}

	entry, ok := r.Lookup("c1")
	if !ok || entry.CPUMilliCommitted != 6000 {
		t.Errorf("entry = %+v, want the previous good report retained", entry)
	}
}

// TestIndexIsBounded keeps an authenticated caller from filling hub memory with
// heartbeats for invented cell names (docs/threat-model.md T-06).
func TestIndexIsBounded(t *testing.T) {
	c := newClock()
	r := New(Options{MaxCells: 3, Now: c.Now})

	for i := range 3 {
		if err := r.Report(healthyReport(fmt.Sprintf("c%d", i))); err != nil {
			t.Fatalf("Report(c%d) = %v, want nil", i, err)
		}
	}

	err := r.Report(healthyReport("c4"))
	if !errors.Is(err, ErrTooManyCells) {
		t.Fatalf("Report() past the bound = %v, want ErrTooManyCells", err)
	}

	// An existing cell must still be able to refresh once the index is full,
	// or a full index would freeze the whole fleet's capacity view.
	if err := r.Report(healthyReport("c1")); err != nil {
		t.Errorf("Report() for an existing cell at the bound = %v, want nil", err)
	}
}

func TestForgetDropsACell(t *testing.T) {
	c := newClock()
	r := testRegistry(t, c)
	if err := r.Report(healthyReport("c1")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}

	r.Forget("c1")
	if _, ok := r.Lookup("c1"); ok {
		t.Error("Lookup() after Forget reported present, want absent")
	}
}

func TestPruneDropsPastRetentionAndReportsNewlyUnknown(t *testing.T) {
	c := newClock()
	r := New(Options{Staleness: 90 * time.Second, Retention: 10 * time.Minute, Now: c.Now})

	for _, cell := range []string{"c1", "c2"} {
		if err := r.Report(healthyReport(cell)); err != nil {
			t.Fatalf("Report(%s) = %v, want nil", cell, err)
		}
	}

	if got := r.Prune(); len(got) != 0 {
		t.Errorf("Prune() while fresh = %v, want none newly unknown", got)
	}

	c.advance(2 * time.Minute)
	if got := r.Prune(); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("Prune() past staleness = %v, want [c1 c2]", got)
	}
	// The transition is reported once, not on every tick.
	if got := r.Prune(); len(got) != 0 {
		t.Errorf("Prune() repeated = %v, want nothing newly unknown", got)
	}
	if stats := r.Stats(); stats.Unknown != 2 || stats.Fresh != 0 {
		t.Errorf("Stats() = %+v, want 2 unknown", stats)
	}

	// A cell that starts reporting again leaves the Unknown set and can be
	// reported as newly unknown a second time.
	if err := r.Report(healthyReport("c1")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}
	c.advance(2 * time.Minute)
	if got := r.Prune(); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("Prune() after recovery and a second silence = %v, want [c1]", got)
	}

	c.advance(30 * time.Minute)
	r.Prune()
	if _, ok := r.Lookup("c2"); ok {
		t.Error("Lookup(\"c2\") past retention reported present, want dropped")
	}
}

// TestRetentionNeverUndercutsStaleness pins the configuration guard. Dropping
// entries sooner than they go stale would delete cells that are working.
func TestRetentionNeverUndercutsStaleness(t *testing.T) {
	r := New(Options{Staleness: time.Minute, Retention: time.Second})
	if r.opts.Retention < r.opts.Staleness {
		t.Errorf("retention = %s, staleness = %s; want retention raised to at least staleness",
			r.opts.Retention, r.opts.Staleness)
	}
}

func TestRunPrunesUntilContextIsCancelled(t *testing.T) {
	c := newClock()
	r := New(Options{Staleness: time.Second, Retention: 2 * time.Second, PruneInterval: time.Millisecond, Now: c.Now})
	if err := r.Report(healthyReport("c1")); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}
	c.advance(10 * time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := r.Lookup("c1"); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run() did not prune the expired entry within 2s")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

// TestConcurrentReportsAndReads exists for the race detector. Every agent in
// the fleet writes here while every placement request reads.
func TestConcurrentReportsAndReads(t *testing.T) {
	r := New(Options{})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = r.Report(healthyReport(fmt.Sprintf("c%d", i%3)))
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				r.Snapshot()
				r.Lookup("c1")
				r.Stats()
			}
		}()
	}
	wg.Wait()
}

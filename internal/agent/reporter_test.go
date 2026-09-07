package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testReporter returns a Reporter with a fixed random source, so the intervals
// it computes are exact rather than approximately right.
func testReporter(rnd float64) *Reporter {
	return &Reporter{
		Interval:   30 * time.Second,
		Timeout:    10 * time.Second,
		MaxBackoff: 2 * time.Minute,
		Log:        discardLogger(),
		Rand:       func() float64 { return rnd },
	}
}

// TestReporterRunPublishesFreshMeasurements is the core of "degrades honestly".
// Every publish must carry a measurement taken for that publish.
func TestReporterRunPublishesFreshMeasurements(t *testing.T) {
	var (
		mu        sync.Mutex
		collected int
		published []Snapshot
	)
	done := make(chan struct{})

	r := testReporter(0)
	r.Interval = time.Millisecond
	r.Timeout = 500 * time.Microsecond
	r.MaxBackoff = time.Millisecond
	r.Collect = func() Snapshot {
		mu.Lock()
		defer mu.Unlock()
		collected++
		return Snapshot{Nodes: collected}
	}
	r.Publish = func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, s)
		if len(published) == 3 {
			close(done)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		if err := r.Run(ctx); err != nil {
			t.Errorf("Run() = %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for three heartbeats")
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	for i, s := range published[:3] {
		if s.Nodes != i+1 {
			t.Errorf("publish %d carried snapshot %+v; each publish must carry its own measurement", i, s)
		}
	}
}

// TestReporterNeverRepublishesAFailedSnapshot pins that a report the hub did
// not accept is discarded rather than queued. A replayed snapshot would steer
// live deploys with numbers that were true minutes ago, and nothing downstream
// could tell it was stale.
func TestReporterNeverRepublishesAFailedSnapshot(t *testing.T) {
	var (
		mu        sync.Mutex
		collected int
		attempts  []Snapshot
	)
	done := make(chan struct{})

	r := testReporter(0)
	r.Interval = time.Millisecond
	r.Timeout = 500 * time.Microsecond
	r.MaxBackoff = 2 * time.Millisecond
	r.Collect = func() Snapshot {
		mu.Lock()
		defer mu.Unlock()
		collected++
		return Snapshot{Nodes: collected}
	}
	r.Publish = func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, s)
		if len(attempts) == 3 {
			close(done)
		}
		return errors.New("hub unreachable")
	}

	ctx, cancel := context.WithCancel(t.Context())
	go r.Run(ctx) //nolint:errcheck // Run only returns nil once ctx is cancelled
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for three attempts")
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	seen := map[int]bool{}
	for _, s := range attempts[:3] {
		if seen[s.Nodes] {
			t.Fatalf("snapshot %+v was published twice; a failed report must be discarded, not retried", s)
		}
		seen[s.Nodes] = true
	}
}

func TestReporterBackoff(t *testing.T) {
	// rand at 0.5 makes the jitter term exactly zero, so these are the
	// underlying intervals rather than a range.
	r := testReporter(0.5)

	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 30 * time.Second},
		{failures: 2, want: time.Minute},
		{failures: 3, want: 2 * time.Minute},
		{failures: 4, want: 2 * time.Minute},
		{failures: 20, want: 2 * time.Minute},
	}

	for _, tt := range tests {
		if got := r.backoff(tt.failures); got != tt.want {
			t.Errorf("backoff(%d) = %s, want %s", tt.failures, got, tt.want)
		}
	}
}

func TestReporterJitterStaysInBounds(t *testing.T) {
	interval := 30 * time.Second
	lo := interval - time.Duration(jitterFraction*float64(interval))
	hi := interval + time.Duration(jitterFraction*float64(interval))

	for _, rnd := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
		got := testReporter(rnd).jitter(interval)
		if got < lo || got > hi {
			t.Errorf("jitter(%s) with rand %v = %s, want within [%s, %s]", interval, rnd, got, lo, hi)
		}
	}
}

// TestReporterFirstHeartbeatUsesFullJitter pins the startup spread. A ±10%
// window around a shared start time leaves a fleet-wide rollout in phase for a
// long time; the first wait has to be able to land anywhere in the interval.
func TestReporterFirstHeartbeatUsesFullJitter(t *testing.T) {
	interval := 30 * time.Second

	for _, tc := range []struct {
		rnd  float64
		want time.Duration
	}{
		{rnd: 0, want: 0},
		{rnd: 0.5, want: 15 * time.Second},
		{rnd: 0.9, want: 27 * time.Second},
	} {
		if got := testReporter(tc.rnd).random(interval); got != tc.want {
			t.Errorf("random(%s) with rand %v = %s, want %s", interval, tc.rnd, got, tc.want)
		}
	}
}

func TestReporterRunStopsOnCancel(t *testing.T) {
	r := testReporter(0)
	r.Interval = time.Millisecond
	r.Timeout = 500 * time.Microsecond
	r.MaxBackoff = time.Millisecond
	r.Collect = func() Snapshot { return Snapshot{} }
	r.Publish = func(context.Context, Snapshot) error { return nil }

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- r.Run(ctx) }()

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Run() = %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

// TestReporterOnceAppliesTheRequestTimeout keeps a hung hub from holding the
// loop open past the next tick.
func TestReporterOnceAppliesTheRequestTimeout(t *testing.T) {
	r := testReporter(0)
	r.Timeout = 10 * time.Millisecond
	r.Collect = func() Snapshot { return Snapshot{} }
	r.Publish = func(ctx context.Context, _ Snapshot) error {
		<-ctx.Done()
		return ctx.Err()
	}

	start := time.Now()
	if err := r.once(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("once() = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("once() took %s; the request timeout was not applied", elapsed)
	}
}

package agent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// jitterFraction is how far a steady-state heartbeat may drift either side of
// the configured interval. Enough to keep a fleet from re-converging on one
// instant, small enough that the effective rate is still the one configured.
const jitterFraction = 0.1

// Reporter publishes a cell's capacity on a heartbeat.
//
// Collection and publication are function fields rather than concrete types so
// the loop's timing, backoff and failure handling are testable without a
// cluster or a hub. Everything security-relevant about the loop is in when it
// gives up and what it does with a report it could not deliver.
type Reporter struct {
	// Collect measures the cell. Called immediately before each publish,
	// never in advance.
	Collect func() Snapshot
	// Publish sends one report.
	Publish func(context.Context, Snapshot) error

	Interval   time.Duration
	Timeout    time.Duration
	MaxBackoff time.Duration
	Log        *slog.Logger

	// Rand returns a value in [0,1). Nil means the global source.
	Rand func() float64
}

// Run publishes until ctx is cancelled.
//
// Nothing is ever buffered. Each attempt measures the cell again, so a report
// that failed to send is discarded rather than retried, and the hub only ever
// sees numbers that were true when they were sent. That is the whole of
// "degrades honestly": a cell whose agent cannot reach the hub goes Unknown and
// drops out of scoring, which is a state the hub detects. A queue of stale
// reports delivered on reconnection would instead steer live deploys with
// minutes-old numbers, and nothing downstream could tell.
func (r *Reporter) Run(ctx context.Context) error {
	// Full jitter before the first heartbeat, not just around later ones. A
	// fleet-wide rollout starts every agent within a few seconds of the others,
	// and ±10% around a shared start time keeps them in phase for a long time.
	timer := time.NewTimer(r.random(r.Interval))
	defer timer.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		if err := r.once(ctx); err != nil {
			if ctx.Err() != nil {
				// Shutting down. The in-flight attempt was cancelled by the
				// same signal, so it is not a failure worth logging.
				return nil
			}
			failures++
			wait := r.backoff(failures)
			r.logFailure(err, failures, wait)
			timer.Reset(wait)
			continue
		}

		if failures > 0 {
			r.Log.Info("capacity reporting recovered", slog.Int("failed_attempts", failures))
			failures = 0
		}
		timer.Reset(r.jitter(r.Interval))
	}
}

// once measures and publishes a single report under the request timeout.
func (r *Reporter) once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	return r.Publish(ctx, r.Collect())
}

// logFailure records a failed heartbeat at the level its cause deserves.
func (r *Reporter) logFailure(err error, failures int, wait time.Duration) {
	attrs := []any{
		slog.Any("error", err),
		slog.Int("consecutive_failures", failures),
		slog.Duration("retry_in", wait),
	}

	var hubErr *HubError
	if errors.As(err, &hubErr) && hubErr.Misconfigured() {
		// A 4xx will still be a 4xx on the next attempt. Logging it at warn
		// alongside a transient connection refusal would bury the one failure
		// that needs a human, which in practice is a cell registered with the
		// wrong reporter issuer.
		r.Log.Error("hub rejected this agent's capacity report; it will not succeed until the cell's registration is corrected", attrs...)
		return
	}
	r.Log.Warn("capacity report failed", attrs...)
}

// backoff returns how long to wait after n consecutive failures.
//
// Exponential from the heartbeat interval, capped, and jittered so that a fleet
// that lost the hub together does not return to it together.
func (r *Reporter) backoff(n int) time.Duration {
	wait := r.Interval
	for i := 1; i < n && wait < r.MaxBackoff; i++ {
		wait *= 2
	}
	return r.jitter(min(wait, r.MaxBackoff))
}

// jitter spreads d by up to jitterFraction either side.
func (r *Reporter) jitter(d time.Duration) time.Duration {
	offset := (r.random01()*2 - 1) * jitterFraction * float64(d)
	return d + time.Duration(offset)
}

// random returns a duration uniformly distributed in [0,d).
func (r *Reporter) random(d time.Duration) time.Duration {
	return time.Duration(r.random01() * float64(d))
}

func (r *Reporter) random01() float64 {
	if r.Rand != nil {
		return r.Rand()
	}
	return rand.Float64()
}

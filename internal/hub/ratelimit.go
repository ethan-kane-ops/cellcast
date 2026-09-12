package hub

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// sweepEvery bounds how often idle callers are looked for. Sweeping on every
// call would make each placement pay for the whole map.
const sweepEvery = time.Minute

// callerLimiter bounds how often one caller may ask for a placement.
//
// Keyed on the caller's issuer and subject: who is asking, as authentication
// established it. Placement sits in the deploy critical path and each mint is
// a TokenRequest round trip to a spoke, so one pipeline stuck in a retry loop,
// or one compromised runner, would otherwise keep every replica busy and pass
// the cost on to the cells (docs/threat-model.md T-06).
//
// Per replica. With N replicas the effective limit is N times the configured
// one. A shared bucket needs shared state, and a store for numbers that are
// high-write and worthless within seconds is exactly what ADR-002 declines for
// capacity. The multiplication is documented rather than hidden.
//
// The key is only as stable as the subject. Buildkite puts the commit in `sub`,
// so there each build gets its own bucket: a retry loop inside one build is
// still limited, and a flood of separate builds is not.
type callerLimiter struct {
	limit rate.Limit
	burst int
	// idle is how long a caller can go unseen before its bucket is dropped.
	// Long enough for the bucket to have refilled completely, at which point a
	// fresh one is indistinguishable from it, so forgetting changes nothing a
	// caller can observe.
	idle time.Duration

	mu        sync.Mutex
	callers   map[string]*callerEntry
	lastSweep time.Time
}

type callerEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// newCallerLimiter allows each caller perSecond placements a second in bursts
// of burst. It returns nil when perSecond is zero, and a nil limiter allows
// everything.
func newCallerLimiter(perSecond float64, burst int) *callerLimiter {
	if perSecond <= 0 {
		return nil
	}
	refill := time.Duration(float64(burst) / perSecond * float64(time.Second))
	return &callerLimiter{
		limit:   rate.Limit(perSecond),
		burst:   burst,
		idle:    max(refill, sweepEvery),
		callers: make(map[string]*callerEntry),
	}
}

// allow reports whether the caller may place at now, and if not, how long
// until it may.
func (l *callerLimiter) allow(issuer, subject string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	// The issuer is one of the hub's configured URLs and cannot hold a NUL, so
	// the first NUL marks where it ends and no two callers share a key.
	key := issuer + "\x00" + subject

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= sweepEvery {
		l.sweep(now)
	}

	e, ok := l.callers[key]
	if !ok {
		e = &callerEntry{lim: rate.NewLimiter(l.limit, l.burst)}
		l.callers[key] = e
	}
	e.seen = now

	r := e.lim.ReserveN(now, 1)
	if wait := r.DelayFrom(now); wait > 0 {
		// Cancelled, so a refused request does not spend a token the caller
		// was never given. Otherwise every retry inside the wait would push
		// the next allowed request further out, and a caller that backs off
		// correctly would still be punished for having asked.
		r.CancelAt(now)
		return false, wait
	}
	return true, 0
}

func (l *callerLimiter) sweep(now time.Time) {
	for key, e := range l.callers {
		if now.Sub(e.seen) >= l.idle {
			delete(l.callers, key)
		}
	}
	l.lastSweep = now
}

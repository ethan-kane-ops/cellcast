package hub

import (
	"fmt"
	"testing"
	"time"
)

var epoch = time.Unix(1_700_000_000, 0)

func TestACallerOverItsBurstIsRefusedAndAnotherIsNot(t *testing.T) {
	// One caller's flood must not become everybody's outage, which is the
	// entire case for keying on identity rather than limiting the route.
	l := newCallerLimiter(1, 3)
	flood, quiet := "repo:acme/flood:ref:refs/heads/main", "repo:acme/quiet:ref:refs/heads/main"

	for i := range 3 {
		if ok, _ := l.allow(githubIssuer, flood, epoch); !ok {
			t.Fatalf("request %d of a burst of 3 was refused", i+1)
		}
	}
	ok, wait := l.allow(githubIssuer, flood, epoch)
	if ok {
		t.Fatal("a fourth request in the same instant was allowed past a burst of 3")
	}
	if wait <= 0 || wait > time.Second {
		t.Errorf("wait = %s, want at most one token interval", wait)
	}
	if ok, _ := l.allow(githubIssuer, quiet, epoch); !ok {
		t.Error("a different caller in the same instant was refused")
	}
}

func TestARefusalDoesNotSpendATokenTheCallerWasNotGiven(t *testing.T) {
	// A caller retrying in a tight loop gets its next token when the interval
	// says so, not later for having asked in the meantime.
	l := newCallerLimiter(1, 1)
	subject := "repo:acme/app:ref:refs/heads/main"

	if ok, _ := l.allow(githubIssuer, subject, epoch); !ok {
		t.Fatal("the first request was refused")
	}
	for range 5 {
		l.allow(githubIssuer, subject, epoch.Add(100*time.Millisecond))
	}
	if ok, _ := l.allow(githubIssuer, subject, epoch.Add(time.Second)); !ok {
		t.Error("the caller was still refused a full interval after its last allowed request")
	}
}

func TestIdleCallersAreForgottenWithoutChangingTheAnswer(t *testing.T) {
	// The key can be a new string for every build, so memory has to be
	// bounded. Dropping a caller only once its bucket would have refilled means
	// it notices nothing: it comes back to a full burst either way.
	l := newCallerLimiter(1, 2)
	for i := range 10 {
		l.allow(githubIssuer, fmt.Sprintf("build-%d", i), epoch)
	}

	later := epoch.Add(l.idle + sweepEvery)
	if ok, _ := l.allow(githubIssuer, "build-0", later); !ok {
		t.Error("a caller back after its bucket had refilled was refused")
	}

	l.mu.Lock()
	held := len(l.callers)
	l.mu.Unlock()
	if held != 1 {
		t.Errorf("the limiter still holds %d callers after they went idle, want only the one that came back", held)
	}
}

func TestAZeroRateLimitsNothing(t *testing.T) {
	if l := newCallerLimiter(0, 50); l != nil {
		t.Fatal("a zero rate built a limiter; zero is how an operator turns the limit off")
	}
	var l *callerLimiter
	for i := range 1000 {
		if ok, _ := l.allow(githubIssuer, "repo:acme/app:ref:refs/heads/main", epoch); !ok {
			t.Fatalf("a disabled limiter refused request %d", i+1)
		}
	}
}

func TestTheRateLimitRefusesABucketThatHoldsNothing(t *testing.T) {
	// A rate with a burst of zero admits nobody: a hub that refuses every
	// placement and calls it a rate limit. Refused at startup instead.
	cfg := DefaultConfig()
	cfg.PlacementRateLimit, cfg.PlacementBurst = 5, 0
	if err := cfg.Validate(); err == nil {
		t.Error("a rate limit with a burst of zero validated")
	}

	cfg.PlacementRateLimit, cfg.PlacementBurst = -1, 50
	if err := cfg.Validate(); err == nil {
		t.Error("a negative rate limit validated")
	}

	cfg.PlacementRateLimit, cfg.PlacementBurst = 0, 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled limit with no burst was refused: %v", err)
	}
}

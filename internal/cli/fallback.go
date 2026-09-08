package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// Sources a decision can come from.
//
// Emitted on every result, in both output modes, because the one thing a
// pipeline must be able to tell apart is a fresh answer from a fallback.
const (
	SourceHub    = "hub"
	SourceCache  = "cache"
	SourcePinned = "pinned"
)

// Confidence values the client attaches to a decision the hub did not make.
// The hub's own values are "high" and "degraded".
const (
	// ConfidenceStale is a decision replayed from the cache. It was true when
	// the hub made it and nothing has confirmed it since.
	ConfidenceStale = "stale"

	// ConfidenceNone is a cell the operator pinned in advance. Nothing about
	// the current fleet was consulted to produce it.
	ConfidenceNone = "none"
)

// Stance values that are not a cell name.
const (
	stanceFail      = "fail"
	stanceLastKnown = "last-known"
)

// stance is what the caller declared should happen when the hub cannot answer.
//
// Declared up front and never inferred. An implicit fallback is how a deploy
// silently lands in the wrong cluster (docs/architecture.md ADR-006).
type stance struct {
	// cell is the pinned fallback, empty for fail and last-known.
	cell string
	// lastKnown is whether the cache may answer.
	lastKnown bool
}

// parseStance reads --on-unavailable.
//
// "fail" and "last-known" are reserved words, so a cell may not be named either
// of them. Anything else is taken as a cell name and validated as one, which is
// what turns a typo like "lastknown" into an error here rather than a pin to a
// cell that does not exist.
func parseStance(v string) (stance, error) {
	switch v {
	case "", stanceFail:
		return stance{}, nil
	case stanceLastKnown:
		return stance{lastKnown: true}, nil
	}

	if errs := validation.IsDNS1123Subdomain(v); len(errs) > 0 {
		return stance{}, fmt.Errorf(
			"--on-unavailable=%s is neither %s, %s, nor a valid cell name: %s",
			v, stanceFail, stanceLastKnown, strings.Join(errs, "; "))
	}
	return stance{cell: v}, nil
}

// fallsBack reports whether this stance has anything to fall back to.
func (s stance) fallsBack() bool { return s.lastKnown || s.cell != "" }

// fallbackAllowed reports whether a failed placement may be answered by the
// caller's stance at all.
//
// This is the fail-closed-on-authorization half of ADR-006, and it sits above
// the stance on purpose: a caller cannot buy its way past a refusal by
// declaring a fallback. The stance only ever chooses between failing and
// guessing among cells the hub had already agreed this caller may reach.
func fallbackAllowed(err error) bool {
	var ref *Refusal
	if !errors.As(err, &ref) {
		// No response at all: connection refused, a timeout, a DNS failure.
		// Nothing was decided, so nothing was refused.
		return true
	}

	if ref.Reason == refusal.Unknown {
		// Something answered and it was not the hub. A gateway reporting that
		// it could not reach an upstream is a hub outage by another name; any
		// other status is an answer this client cannot classify, and an
		// unclassifiable answer is not one to act on.
		switch ref.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}

	return refusal.Optimisation(ref.Reason)
}

// refusedFallback explains why a declared stance did not apply.
//
// Said explicitly rather than by returning the original error alone, because a
// caller that set --on-unavailable and then saw a plain refusal would
// reasonably conclude the flag is broken and go looking in the wrong place.
func refusedFallback(err error) error {
	var ref *Refusal
	if errors.As(err, &ref) && !refusal.Optimisation(ref.Reason) && ref.Reason != refusal.Unknown {
		return fmt.Errorf("%w (--on-unavailable does not apply: the hub answered, and %s is not a failure to choose a cell)",
			err, ref.Reason)
	}
	return fmt.Errorf("%w (--on-unavailable does not apply: the response could not be classified, so it is not one to guess past)", err)
}

package broker

import (
	"errors"
	"fmt"
	"time"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// EnvLabel is the cell label TTL defaults are derived from.
const EnvLabel = "env"

// ErrTTLBelowProviderFloor means policy demands a credential shorter than the
// provider can issue.
//
// This is refused rather than rounded up. Minting a ten-minute token under a
// policy whose ceiling is five minutes would silently overrule the operator on
// the one number they set to bound a compromise, and it would do so in the
// direction that grants more access. An operator who wants a shorter credential
// than Kubernetes can issue needs to know that today, not believe they have it.
var ErrTTLBelowProviderFloor = errors.New("policy ttl ceiling is shorter than the provider can issue")

// Bounds are the resolved default and maximum credential lifetime.
type Bounds struct {
	Default time.Duration
	Max     time.Duration
}

// envBounds are the built-in defaults, keyed by the cell's `env` label.
//
// Production sits at the provider floor rather than below it: see
// ErrTTLBelowProviderFloor and docs/architecture.md ADR-004, which originally
// specified five minutes and could not have it.
var envBounds = map[string]Bounds{
	"prod":        {Default: 10 * time.Minute, Max: 15 * time.Minute},
	"production":  {Default: 10 * time.Minute, Max: 15 * time.Minute},
	"stage":       {Default: 15 * time.Minute, Max: 30 * time.Minute},
	"staging":     {Default: 15 * time.Minute, Max: 30 * time.Minute},
	"dev":         {Default: 30 * time.Minute, Max: 60 * time.Minute},
	"development": {Default: 30 * time.Minute, Max: 60 * time.Minute},
}

// strictestBounds is what an unlabelled or unrecognised cell gets.
//
// The tightest bounds, not the loosest and not a middle value. The realistic
// failure is an operator registering a production cell and forgetting the
// label, so the unlabelled case must not be the one that grants the longest
// credential. Getting this backwards turns a typo into an hour of cluster
// access.
var strictestBounds = envBounds["prod"]

// BoundsFor returns the built-in bounds for a cell's `env` label.
func BoundsFor(labels map[string]string) Bounds {
	b, ok := envBounds[labels[EnvLabel]]
	if !ok {
		return strictestBounds
	}
	return b
}

// Resolution records what lifetime was granted and how it was arrived at.
//
// Returned to the caller rather than kept internal: a pipeline that asked for
// thirty minutes and received ten needs to know before its deploy stops halfway
// through, and an operator debugging a surprising expiry needs the same answer.
type Resolution struct {
	// Granted is the lifetime that will be requested from the provider.
	Granted time.Duration
	// Default is the lifetime that applied with no request.
	Default time.Duration
	// Max is the effective ceiling after policy and the hub bound.
	Max time.Duration
	// Requested is what the caller asked for, zero if it asked for nothing.
	Requested time.Duration
	// Clamped is whether the request was reduced to fit.
	Clamped bool
}

// ResolveTTL works out how long a minted credential may live.
//
// Three layers, narrowest last:
//
//  1. The cell's `env` label supplies built-in defaults.
//  2. The caller's PlacementPolicy overrides them, in either direction. This is
//     operator-authored config, so it is allowed to be more generous than the
//     built-in table.
//  3. The hub's own ceiling bounds everything and is never exceeded. It is not
//     defence in depth: a Kubernetes API server applies no maximum of its own
//     unless one is configured, and will happily issue a token lasting years,
//     so this is the only bound that is certain to exist.
//
// A caller may ask for less. A caller may never ask for more.
func ResolveTTL(
	cellLabels map[string]string,
	policy *cellcastv1alpha1.TokenTTLPolicy,
	requested, hubCeiling, providerFloor time.Duration,
) (Resolution, error) {
	b := BoundsFor(cellLabels)

	if policy != nil {
		if policy.Default != nil && policy.Default.Duration > 0 {
			b.Default = policy.Default.Duration
		}
		if policy.Max != nil && policy.Max.Duration > 0 {
			b.Max = policy.Max.Duration
		}
	}

	if hubCeiling > 0 && b.Max > hubCeiling {
		b.Max = hubCeiling
	}
	// A default above the ceiling is a misconfiguration that would otherwise
	// grant the ceiling silently. Reduce it so Granted and Default agree.
	if b.Default > b.Max {
		b.Default = b.Max
	}

	res := Resolution{Default: b.Default, Max: b.Max, Requested: requested}

	res.Granted = b.Default
	if requested > 0 {
		res.Granted = requested
		if requested > b.Max {
			res.Granted, res.Clamped = b.Max, true
		}
	}

	// The floor is checked against the ceiling, not against the granted value.
	// Rounding a request up to the floor is fine, because the operator's bound
	// still holds. Rounding the operator's bound up is not.
	if providerFloor > 0 && b.Max < providerFloor {
		return res, fmt.Errorf("%w: ceiling is %s, provider floor is %s",
			ErrTTLBelowProviderFloor, b.Max, providerFloor)
	}
	if res.Granted < providerFloor {
		res.Granted = providerFloor
	}

	return res, nil
}

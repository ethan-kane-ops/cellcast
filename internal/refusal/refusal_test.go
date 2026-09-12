package refusal_test

import (
	"testing"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// TestOptimisationClassifiesEveryReason is the exhaustiveness guard.
//
// The table is written out rather than derived, so adding a reason to the
// contract without deciding which side of the ADR-006 line it falls on fails
// here instead of silently inheriting Optimisation's default.
func TestOptimisationClassifiesEveryReason(t *testing.T) {
	want := map[refusal.Reason]bool{
		// Authorization. A fallback here would let a flag override the policy
		// engine.
		refusal.NoPolicy:         false,
		refusal.DarkNotPermitted: false,
		refusal.NoPermittedCells: false,

		// The caller sent something wrong. A fallback would hide the typo and
		// deploy anyway.
		refusal.InvalidRequest: false,

		// Optimisation. The caller is permitted; the hub cannot rank.
		refusal.NoEligibleCells:      true,
		refusal.CapacityUnknown:      true,
		refusal.PlacementUnavailable: true,
		// Declined before policy was evaluated, like the one above.
		refusal.RateLimited: true,

		// Minting never falls back, even though both of these are 503s that
		// look like availability failures.
		refusal.MintUnavailable: false,
		refusal.MintFailed:      false,

		// An answer that could not be parsed is not an answer to act on.
		refusal.Unknown: false,
	}

	all := refusal.All()
	if len(all) != len(want) {
		t.Fatalf("All() has %d reasons and the table classifies %d; classify the new one", len(all), len(want))
	}

	for _, r := range all {
		expected, ok := want[r]
		if !ok {
			t.Errorf("reason %q is not classified", r)
			continue
		}
		if got := refusal.Optimisation(r); got != expected {
			t.Errorf("Optimisation(%q) = %v, want %v", r, got, expected)
		}
	}
}

// TestUnrecognisedReasonIsClosed pins the default. A reason from a newer hub
// must not be read as permission to fall back.
func TestUnrecognisedReasonIsClosed(t *testing.T) {
	if refusal.Optimisation(refusal.Reason("SomethingNewerHubsSend")) {
		t.Error("an unrecognised reason is treated as an optimisation failure; it must fail closed")
	}
}

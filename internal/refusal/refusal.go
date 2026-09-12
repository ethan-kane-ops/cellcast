// Package refusal is the wire contract for a refused placement.
//
// It is a leaf package with no imports because both ends of the contract need
// it: the hub writes these codes, and the client's declared fallback stance
// decides what to do from them (docs/architecture.md ADR-006). Stating the
// strings separately on each side is how the two quietly stop agreeing, and the
// failure that produces is a client falling open on a refusal it should have
// honoured.
package refusal

// Reason is the machine-readable half of a refusal.
//
// Clients match on this and never on the prose beside it. The prose is written
// for a human reading a build log and is free to change.
type Reason string

const (
	// NoPolicy means no PlacementPolicy matches the caller. Deny by default.
	NoPolicy Reason = "NoPolicy"

	// DarkNotPermitted means the caller asked for a dark cell under a policy
	// that does not allow it.
	DarkNotPermitted Reason = "DarkNotPermitted"

	// NoPermittedCells means the matching policy's selector permits no
	// registered cell.
	NoPermittedCells Reason = "NoPermittedCells"

	// InvalidRequest means the request itself was malformed.
	InvalidRequest Reason = "InvalidRequest"

	// NoEligibleCells means every permitted cell is draining, or is dark
	// without a dark-targeting request.
	NoEligibleCells Reason = "NoEligibleCells"

	// CapacityUnknown means every permitted, eligible cell has stale or missing
	// capacity, so the hub cannot tell which is least loaded.
	CapacityUnknown Reason = "CapacityUnknown"

	// PlacementUnavailable means the hub is running but cannot decide: its
	// placement engine is not wired up, its registry has not synced, or the
	// replica that answered has not yet heard from the fleet it would score.
	PlacementUnavailable Reason = "PlacementUnavailable"

	// RateLimited means this caller has asked for more placements than its
	// share, and the hub declined before doing any work. The response's
	// Retry-After says when to ask again.
	RateLimited Reason = "RateLimited"

	// MintUnavailable means the credential broker is not available.
	MintUnavailable Reason = "MintUnavailable"

	// MintFailed means the decision was made and the credential could not be.
	MintFailed Reason = "MintFailed"

	// Unknown is what a client records when a response carries no reason it can
	// parse, which is what happens when a proxy answers instead of the hub. The
	// hub never writes it.
	Unknown Reason = "Unknown"
)

// All is every reason the contract defines.
//
// Exported so a test can assert each one has been classified. A
// new reason that nobody classified would fall through Optimisation's default
// and be treated as an authorization refusal, which is the safe answer but not
// necessarily the right one.
func All() []Reason {
	return []Reason{
		NoPolicy, DarkNotPermitted, NoPermittedCells, InvalidRequest,
		NoEligibleCells, CapacityUnknown, PlacementUnavailable, RateLimited,
		MintUnavailable, MintFailed, Unknown,
	}
}

// Optimisation reports whether a refusal may be answered by the caller's
// declared fallback stance.
//
// This is the ADR-006 line drawn in code, and the split it draws is between two
// failures that look alike and are not:
//
//   - The hub could not establish that the caller is permitted. Refuse. If a
//     fallback answered this, anyone could deploy anywhere by passing a flag,
//     and the policy engine would be decoration.
//   - The hub established the caller is permitted and could not work out which
//     of its permitted cells is best. Fall back. The cost is a suboptimal cell,
//     never an unauthorised one.
//
// Minting sits on the closed side of the line. A cached placement is a cached
// decision, never a cached credential, so a mint that failed is never softened
// into a success by a stance.
func Optimisation(r Reason) bool {
	switch r {
	// RateLimited sits with PlacementUnavailable: both are the hub declining
	// before it has evaluated any policy, never a finding that the caller is
	// not permitted. A stance never yields a credential, so answering one
	// grants nothing the caller did not already have.
	case NoEligibleCells, CapacityUnknown, PlacementUnavailable, RateLimited:
		return true
	default:
		return false
	}
}

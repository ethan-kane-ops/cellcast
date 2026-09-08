package placement

import "fmt"

// RefusedError carries the reasoning behind a refusal.
//
// The sentinel errors above say what happened. This says why, and it exists
// because the audit trail (ENG-176) has to record a refusal with the same
// detail as a success: which policy governed the attempt, which cells were
// considered, and which filter stage rejected each one. Without it a refused
// placement is audited as a bare error string, and "why was my deploy refused"
// has no answer that does not involve a debug build.
//
// It is an error rather than a second return value on purpose. A Decision
// returned alongside an error is a Decision somebody eventually uses, and every
// field on it would be a cell the caller was not permitted to reach.
type RefusedError struct {
	// Err is the refusal itself, always one of the sentinels in this package.
	// errors.Is unwraps to it, so existing handling is unaffected.
	Err error
	// Policy is the PlacementPolicy that governed the attempt, empty when the
	// refusal happened before one was selected.
	Policy string
	// Candidates is every registered cell with the stage that refused it, empty
	// when the refusal happened before the filter ran.
	Candidates []Candidate
}

func (e *RefusedError) Error() string {
	if e.Policy == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: policy %s", e.Err, e.Policy)
}

func (e *RefusedError) Unwrap() error { return e.Err }

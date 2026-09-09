package v1alpha1

// The placement semantics of ClusterState live here rather than in the hub so
// that there is exactly one definition of what each state means. A consumer
// outside this repository reads the rules from here, rather than keeping a
// second copy of the table to drift from.
//
// See docs/architecture.md ADR-008.

// Cluster status condition types.
const (
	// ClusterConditionAccepting reports whether the cell takes new placements
	// without the caller asking for it by name.
	ClusterConditionAccepting = "AcceptingPlacements"
)

// Reasons for ClusterConditionAccepting. One per state, so `kubectl get
// cluster -o json` answers "why is nothing landing here" without a lookup
// table.
const (
	ClusterReasonLive     = "Live"
	ClusterReasonDark     = "Dark"
	ClusterReasonDraining = "Draining"
	ClusterReasonUnknown  = "UnknownState"
)

// Normalize resolves the empty state to the default.
//
// The CRD defaults state to LIVE, but an object written by a client that
// bypassed defaulting still has to resolve to something, and defaulting to
// "not LIVE" would silently drain a cell.
func (s ClusterState) Normalize() ClusterState {
	if s == "" {
		return ClusterStateLive
	}
	return s
}

// RejectsPlacement returns why a cell in this state may not receive a new
// placement, or the empty string when it may.
//
// wantDark is true when the caller explicitly asked for a dark cell and policy
// permits it. It is the caller's intent, already authorized: this function does
// not decide whether the caller was allowed to ask.
//
// The returned string is a rejection reason rendered to the caller by
// `--explain`. It describes the cell's state, which the caller is
// already permitted to see, and nothing else.
func (s ClusterState) RejectsPlacement(wantDark bool) string {
	switch s.Normalize() {
	case ClusterStateLive:
		if wantDark {
			return "cell is LIVE and the request targets dark cells"
		}
		return ""
	case ClusterStateDark:
		if wantDark {
			return ""
		}
		return "cell is DARK and the request does not target dark cells"
	case ClusterStateDraining:
		// DRAINING refuses even an explicit dark request. A cell being upgraded
		// is not a safe target for a smoke test either.
		return "cell is DRAINING and accepts no new placements"
	default:
		return "cell is in an unrecognised state"
	}
}

// AcceptsPlacement reports whether a cell in this state may receive a new
// placement.
//
// Defined in terms of RejectsPlacement so the predicate and the reason cannot
// disagree.
func (s ClusterState) AcceptsPlacement(wantDark bool) bool {
	return s.RejectsPlacement(wantDark) == ""
}

// ServesProductionTraffic reports whether workloads already on the cell are
// expected to serve.
//
// This is the other half of the ADR-008 table and the half darkgate consumes.
// It is independent of AcceptsPlacement: DRAINING keeps serving
// while refusing new work, which is the entire point of the state.
func (s ClusterState) ServesProductionTraffic() bool {
	switch s.Normalize() {
	case ClusterStateLive, ClusterStateDraining:
		return true
	default:
		return false
	}
}

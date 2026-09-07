package v1alpha1

import "testing"

// TestClusterStatePlacementSemantics is the ADR-008 table expressed as a test.
//
// Every row here is a claim the rest of the system relies on, and the one that
// matters most is DRAINING refusing a dark-targeted request: a cell in the
// middle of an upgrade is not a safe place to run a smoke test either.
func TestClusterStatePlacementSemantics(t *testing.T) {
	tests := []struct {
		name           string
		state          ClusterState
		wantDark       bool
		wantAccepts    bool
		wantProduction bool
	}{
		{name: "live takes ordinary placements", state: ClusterStateLive, wantAccepts: true, wantProduction: true},
		{name: "live is not a dark target", state: ClusterStateLive, wantDark: true, wantAccepts: false, wantProduction: true},
		{name: "dark refuses ordinary placements", state: ClusterStateDark, wantAccepts: false, wantProduction: false},
		{name: "dark takes a targeted placement", state: ClusterStateDark, wantDark: true, wantAccepts: true, wantProduction: false},
		{name: "draining refuses ordinary placements", state: ClusterStateDraining, wantAccepts: false, wantProduction: true},
		{name: "draining refuses a targeted placement too", state: ClusterStateDraining, wantDark: true, wantAccepts: false, wantProduction: true},
		{name: "empty behaves as live", state: "", wantAccepts: true, wantProduction: true},
		{name: "unrecognised state accepts nothing", state: "RETIRED", wantAccepts: false, wantProduction: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.AcceptsPlacement(tt.wantDark); got != tt.wantAccepts {
				t.Errorf("ClusterState(%q).AcceptsPlacement(%v) = %v, want %v",
					tt.state, tt.wantDark, got, tt.wantAccepts)
			}
			if got := tt.state.ServesProductionTraffic(); got != tt.wantProduction {
				t.Errorf("ClusterState(%q).ServesProductionTraffic() = %v, want %v",
					tt.state, got, tt.wantProduction)
			}
		})
	}
}

// TestRejectionReasonMatchesPredicate pins the two entry points to one answer.
// A predicate that disagrees with the reason it prints produces a rejection
// nobody can act on.
func TestRejectionReasonMatchesPredicate(t *testing.T) {
	states := []ClusterState{ClusterStateLive, ClusterStateDark, ClusterStateDraining, "", "RETIRED"}
	for _, state := range states {
		for _, wantDark := range []bool{false, true} {
			reason := state.RejectsPlacement(wantDark)
			accepts := state.AcceptsPlacement(wantDark)
			if accepts != (reason == "") {
				t.Errorf("ClusterState(%q): AcceptsPlacement(%v) = %v but RejectsPlacement = %q",
					state, wantDark, accepts, reason)
			}
		}
	}
}

// TestNormalizeDefaultsToLive pins the direction of the default. Resolving an
// unset state to anything other than LIVE would silently drain every cell
// written by a client that bypassed CRD defaulting.
func TestNormalizeDefaultsToLive(t *testing.T) {
	if got := ClusterState("").Normalize(); got != ClusterStateLive {
		t.Errorf("ClusterState(\"\").Normalize() = %q, want %q", got, ClusterStateLive)
	}
	for _, state := range []ClusterState{ClusterStateDark, ClusterStateDraining, "RETIRED"} {
		if got := state.Normalize(); got != state {
			t.Errorf("ClusterState(%q).Normalize() = %q, want it unchanged", state, got)
		}
	}
}

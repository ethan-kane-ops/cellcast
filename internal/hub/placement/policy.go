// Package placement decides which cell a caller may deploy to.
//
// Placement is two phases in a fixed order: filter, then score. Filter answers
// which cells this caller is permitted to reach and which are eligible right
// now; score picks the best of the survivors. Reversing the order produces a
// system that passes its happy-path tests and routes a dev deploy into the
// least-loaded production cluster the first time production is quiet
// (docs/architecture.md ADR-005, docs/threat-model.md T-03).
//
// This is the confused-deputy defence. Authentication (ENG-172) proves who is
// asking and says nothing about what they may ask for.
package placement

import (
	"cmp"
	"slices"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// matchSubject reports whether a policy subject selector matches a caller.
//
// The issuer is compared literally. Every claim named in the selector must be
// present on the identity and equal; a claim the identity does not carry is a
// non-match rather than a wildcard, because a token missing the claim a policy
// constrains on is exactly the case the constraint exists to catch.
func matchSubject(sel cellcastv1alpha1.SubjectSelector, id *identity.Identity) bool {
	if sel.Issuer != id.Issuer {
		return false
	}
	for key, want := range sel.Claims {
		got, ok := id.Claims[key]
		if !ok || got != want {
			return false
		}
	}
	return true
}

// specificity is how many claim constraints a subject selector imposes.
//
// Used to order overlapping policies. A selector naming no claims matches every
// caller from an issuer and is the least specific thing a policy can say.
func specificity(sel cellcastv1alpha1.SubjectSelector) int { return len(sel.Claims) }

// policyMatch is a policy that applies to a caller, and how tightly.
type policyMatch struct {
	policy      *cellcastv1alpha1.PlacementPolicy
	specificity int
}

// selectPolicy picks the policy governing a caller.
//
// Deny by default: no match is a clean rejection, never a fallback to "any
// cell". When more than one policy matches, the most specific wins, because a
// policy constraining {repository, environment} is what an operator writes when
// they mean to carve an exception out of a policy constraining {repository}.
// Ties on specificity are broken by name so the answer is stable, and reported
// so the ambiguity is visible rather than silently resolved.
func selectPolicy(policies []cellcastv1alpha1.PlacementPolicy, id *identity.Identity) (*cellcastv1alpha1.PlacementPolicy, bool, error) {
	var matches []policyMatch
	for i := range policies {
		best := -1
		for _, sel := range policies[i].Spec.Subjects {
			if matchSubject(sel, id) {
				best = max(best, specificity(sel))
			}
		}
		if best >= 0 {
			matches = append(matches, policyMatch{policy: &policies[i], specificity: best})
		}
	}

	if len(matches) == 0 {
		// Deliberately carries no detail. Naming the issuer reads as "the
		// issuer was wrong" when the mismatch is almost always a claim, and
		// the caller cannot act on either. The engine logs the issuer and
		// subject for the operator instead.
		return nil, false, ErrNoPolicy
	}

	slices.SortFunc(matches, func(a, b policyMatch) int {
		if c := cmp.Compare(b.specificity, a.specificity); c != 0 {
			return c
		}
		return cmp.Compare(a.policy.Name, b.policy.Name)
	})

	ambiguous := len(matches) > 1 && matches[0].specificity == matches[1].specificity
	return matches[0].policy, ambiguous, nil
}

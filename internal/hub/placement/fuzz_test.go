package placement

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
)

// filterBudget is the time one filter pass over a small fleet may take.
//
// Absurdly generous, and deliberately so: it is not a performance assertion, it
// is a tripwire for a selector whose evaluation is superlinear in something an
// operator can write. A real pass over ten cells is microseconds, so nothing
// short of a genuine blowup comes near this, and it cannot flake on a loaded
// machine the way a tight bound would.
const filterBudget = 2 * time.Second

// FuzzFilter covers the permission and eligibility stages against a selector
// expression.
//
// The selector comes from a PlacementPolicy, so it is operator-authored rather
// than caller-supplied, which makes this the least attacker-facing of the fuzz
// targets and still worth having: the filter is what stands between a dev
// pipeline and a prod cell, and a selector nobody meant to write is the way
// that stops being true.
func FuzzFilter(f *testing.F) {
	f.Add("env=prod", "env=prod", false)
	f.Add("env in (prod,staging)", "env=staging", false)
	f.Add("env notin (dev)", "env=prod", true)
	f.Add("!env", "region=euw1", false)
	f.Add("", "env=prod", false)
	f.Add("env=prod,region=euw1", "env=prod,region=euw1", false)
	f.Add(strings.Repeat("a", 300)+"=b", "env=prod", false)
	f.Add("env in ("+strings.Repeat("x,", 500)+"prod)", "env=prod", false)
	f.Add("env==\x00", "env=\x00", false)

	f.Fuzz(func(t *testing.T, expression, cellLabels string, wantDark bool) {
		selector, err := labels.Parse(expression)
		if err != nil {
			return
		}
		parsed, err := labels.ConvertSelectorToLabelsMap(cellLabels)
		if err != nil {
			return
		}

		clusters := []cellcastv1alpha1.Cluster{
			*cell("prod-euw1", cellcastv1alpha1.ClusterStateLive, parsed),
			*cell("prod-euw2", cellcastv1alpha1.ClusterStateDraining, parsed),
			*cell("dark-euw1", cellcastv1alpha1.ClusterStateDark, parsed),
			*cell("never-reported", cellcastv1alpha1.ClusterStateLive, parsed),
		}
		snapshot := map[string]capacity.Entry{
			"prod-euw1": {Health: capacity.HealthFresh, Utilisation: 0.42},
			"prod-euw2": {Health: capacity.HealthFresh, Utilisation: 0.10},
			"dark-euw1": {Health: capacity.HealthUnknown},
		}

		start := time.Now()
		got := filter(clusters, selector, wantDark, snapshot)
		if elapsed := time.Since(start); elapsed > filterBudget {
			t.Fatalf("filtering %d cells against %q took %s", len(clusters), expression, elapsed)
		}

		// Every registered cell appears exactly once, whatever the selector
		// says. `--explain` and the audit trail both rely on that: a cell
		// missing from the table is one nobody can ask about.
		if len(got) != len(clusters) {
			t.Fatalf("filter returned %d candidates for %d cells", len(got), len(clusters))
		}
		if !slices.IsSortedFunc(got, func(a, b Candidate) int { return strings.Compare(a.Cell, b.Cell) }) {
			t.Fatalf("filter returned an unsorted table: %+v", got)
		}

		seen := map[string]bool{}
		for _, c := range got {
			if seen[c.Cell] {
				t.Fatalf("filter returned %s twice", c.Cell)
			}
			seen[c.Cell] = true

			// A refusal must say which stage refused it and why. A candidate
			// with a stage and no reason is one an operator cannot act on, and
			// one with a reason and no stage cannot be grouped.
			if c.Admitted {
				if c.Stage != "" || c.Reason != "" {
					t.Fatalf("admitted %s still carries stage=%q reason=%q", c.Cell, c.Stage, c.Reason)
				}
			} else if c.Stage == "" || c.Reason == "" {
				t.Fatalf("refused %s carries stage=%q reason=%q", c.Cell, c.Stage, c.Reason)
			}

			// The score is compared against other cells' scores, so a NaN would
			// make every comparison false and hand the placement to whichever
			// cell happened to be first.
			if math.IsNaN(c.Utilisation) || math.IsInf(c.Utilisation, 0) {
				t.Fatalf("%s scored %v", c.Cell, c.Utilisation)
			}
			if c.Admitted && (c.Utilisation < 0 || c.Utilisation > 1) {
				t.Fatalf("%s was admitted with utilisation %v, outside [0,1]", c.Cell, c.Utilisation)
			}

			// Permission is the first stage for a reason: a cell the caller may
			// not reach must never be refused on state or capacity instead,
			// because the reason is disclosed by --explain.
			if !c.Admitted && c.Stage != StagePermission && !selector.Matches(labels.Set(parsed)) {
				t.Fatalf("%s is not permitted but was refused at the %s stage", c.Cell, c.Stage)
			}
		}
	})
}

// FuzzLabelSelectorConversion covers the step before the filter: turning the
// CRD's structured selector into one that can match.
//
// A conversion that succeeded on a selector nobody intended would be a policy
// permitting cells nobody meant to permit, which is the failure this whole
// package exists to prevent.
func FuzzLabelSelectorConversion(f *testing.F) {
	f.Add("env", "prod", "In", "prod")
	f.Add("env", "prod", "NotIn", "dev")
	f.Add("env", "prod", "Exists", "")
	f.Add("env", "prod", "DoesNotExist", "")
	f.Add("env", "prod", "Nonsense", "x")
	f.Add("", "", "In", "")
	f.Add(strings.Repeat("a", 300), "v", "In", "v")

	f.Fuzz(func(t *testing.T, matchKey, matchValue, operator, exprValue string) {
		spec := metav1.LabelSelector{
			MatchLabels: map[string]string{matchKey: matchValue},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      matchKey,
				Operator: metav1.LabelSelectorOperator(operator),
				Values:   []string{exprValue},
			}},
		}

		selector, err := metav1.LabelSelectorAsSelector(&spec)
		if err != nil {
			return
		}
		if selector == nil {
			t.Fatal("conversion succeeded and returned a nil selector")
		}

		// A converted selector must not be Everything(): a policy whose
		// permittedCells matched every registered cell would grant a caller the
		// whole fleet, and the operator who wrote a malformed selector would
		// have no way to tell.
		if selector.Empty() {
			t.Fatalf("selector %+v converted to one matching every cell", spec)
		}
		// Whatever it matches, it must answer without panicking for any labels.
		_ = selector.Matches(labels.Set{matchKey: matchValue})
		_ = selector.Matches(labels.Set{})
	})
}

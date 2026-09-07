package placement

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/oidc"
)

const testNamespace = "cellcast-system"

const githubIssuer = "https://token.actions.githubusercontent.com"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func cell(name string, state cellcastv1alpha1.ClusterState, labels map[string]string) *cellcastv1alpha1.Cluster {
	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       "https://" + name + ".example.test",
			Provider:       cellcastv1alpha1.ProviderEKS,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "t1"},
			State:          state,
		},
	}
}

func policy(name string, subjects []cellcastv1alpha1.SubjectSelector, permitted map[string]string) *cellcastv1alpha1.PlacementPolicy {
	return &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects:       subjects,
			PermittedCells: metav1.LabelSelector{MatchLabels: permitted},
		},
	}
}

func subject(claims map[string]string) cellcastv1alpha1.SubjectSelector {
	return cellcastv1alpha1.SubjectSelector{Issuer: githubIssuer, Claims: claims}
}

func identity(claims map[string]string) *hub.Identity {
	return &hub.Identity{Issuer: githubIssuer, Subject: "repo:example/app:ref:refs/heads/main", Claims: claims}
}

// loaded returns a capacity index reporting the given utilisation per cell.
func loaded(t *testing.T, util map[string]float64) *capacity.Registry {
	t.Helper()
	index := capacity.New(capacity.Options{Staleness: time.Hour})
	for name, u := range util {
		err := index.Report(capacity.Report{
			Cell:                   name,
			Nodes:                  3,
			CPUMilliAllocatable:    10000,
			CPUMilliCommitted:      int64(u * 10000),
			MemoryBytesAllocatable: 10000,
			MemoryBytesCommitted:   0,
		})
		if err != nil {
			t.Fatalf("Report(%s) = %v, want nil", name, err)
		}
	}
	return index
}

func newEngine(t *testing.T, index *capacity.Registry, objs ...client.Object) *Engine {
	t.Helper()
	scheme, err := hub.NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewEngine(k8s, index, testNamespace, discardLogger())
}

// TestDevPipelineCannotReachAProdCell is the reason this package exists.
//
// The prod cell is deliberately the least loaded in the fleet, so a
// score-then-filter implementation returns it. Filter-then-score refuses it on
// permission and never looks at its utilisation at all
// (docs/architecture.md ADR-005, docs/threat-model.md T-03).
func TestDevPipelineCannotReachAProdCell(t *testing.T) {
	index := loaded(t, map[string]float64{"prod-euw1": 0.05, "dev-euw1": 0.80})
	engine := newEngine(t, index,
		cell("prod-euw1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "prd"}),
		cell("dev-euw1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("dev", []cellcastv1alpha1.SubjectSelector{
			subject(map[string]string{"repository": "example/app", "environment": "dev"}),
		}, map[string]string{"env": "dev"}),
	)

	dev := identity(map[string]string{"repository": "example/app", "environment": "dev"})
	decision, err := engine.Place(t.Context(), dev, Request{Workload: "api"})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}

	if decision.Cell != "dev-euw1" {
		t.Fatalf("placed on %q, want dev-euw1; the prod cell is emptier and must still be unreachable", decision.Cell)
	}

	prod := candidateFor(t, decision, "prod-euw1")
	if prod.Admitted {
		t.Fatal("the prod cell was admitted for a dev caller")
	}
	if prod.Stage != StagePermission {
		t.Errorf("prod refused at stage %q, want %q; refusing it later means its capacity was consulted first",
			prod.Stage, StagePermission)
	}
	if prod.Utilisation != 0 {
		t.Errorf("prod utilisation = %v in the trace, want it never scored", prod.Utilisation)
	}
}

// TestPermissionIsEvaluatedBeforeState pins the stage ordering. A cell the
// caller may not reach must be refused on permission whatever else is wrong
// with it, so a rejection never discloses the state of a foreign cell.
func TestPermissionIsEvaluatedBeforeState(t *testing.T) {
	index := loaded(t, map[string]float64{"dev-euw1": 0.1})
	engine := newEngine(t, index,
		cell("prod-euw1", cellcastv1alpha1.ClusterStateDraining, map[string]string{"env": "prd"}),
		cell("dev-euw1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("dev", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	decision, err := engine.Place(t.Context(), identity(nil), Request{})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	if got := candidateFor(t, decision, "prod-euw1").Stage; got != StagePermission {
		t.Errorf("prod refused at stage %q, want %q", got, StagePermission)
	}
}

func TestDenyByDefault(t *testing.T) {
	index := loaded(t, map[string]float64{"dev-euw1": 0.1})
	engine := newEngine(t, index,
		cell("dev-euw1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("dev", []cellcastv1alpha1.SubjectSelector{
			subject(map[string]string{"repository": "example/app"}),
		}, map[string]string{"env": "dev"}),
	)

	tests := []struct {
		name string
		id   *hub.Identity
	}{
		{
			name: "a caller no policy names",
			id:   identity(map[string]string{"repository": "someone-else/app"}),
		},
		{
			name: "a caller missing the constrained claim entirely",
			id:   identity(nil),
		},
		{
			name: "the right claims from the wrong issuer",
			id: &hub.Identity{
				Issuer: "https://agent.buildkite.com",
				Claims: map[string]string{"repository": "example/app"},
			},
		},
		{name: "no identity at all", id: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engine.Place(t.Context(), tt.id, Request{})
			if !errors.Is(err, ErrNoPolicy) {
				t.Fatalf("Place() = %v, want ErrNoPolicy; there is no fallback to \"any cell\"", err)
			}
		})
	}
}

func TestEligibilityStacksOnPermission(t *testing.T) {
	tests := []struct {
		name       string
		state      cellcastv1alpha1.ClusterState
		targetDark bool
		allowDark  bool
		wantCell   string
		wantErr    error
	}{
		{name: "live is placeable", state: cellcastv1alpha1.ClusterStateLive, wantCell: "c1"},
		{name: "draining is not", state: cellcastv1alpha1.ClusterStateDraining, wantErr: ErrNoEligibleCells},
		{name: "dark is not, untargeted", state: cellcastv1alpha1.ClusterStateDark, wantErr: ErrNoEligibleCells},
		{
			name:  "dark is, when targeted and permitted",
			state: cellcastv1alpha1.ClusterStateDark, targetDark: true, allowDark: true, wantCell: "c1",
		},
		{
			// Refused rather than downgraded to an ordinary placement: a smoke
			// test that silently lands on a live cell is worse than one that
			// fails.
			name:  "targeting dark without permission is refused",
			state: cellcastv1alpha1.ClusterStateDark, targetDark: true, allowDark: false, wantErr: ErrDarkNotPermitted,
		},
		{
			name:  "draining refuses a targeted dark request too",
			state: cellcastv1alpha1.ClusterStateDraining, targetDark: true, allowDark: true, wantErr: ErrNoEligibleCells,
		},
		{
			name:  "live is not a dark target",
			state: cellcastv1alpha1.ClusterStateLive, targetDark: true, allowDark: true, wantErr: ErrNoEligibleCells,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"})
			pol.Spec.AllowDarkTargeting = tt.allowDark

			engine := newEngine(t, loaded(t, map[string]float64{"c1": 0.1}),
				cell("c1", tt.state, map[string]string{"env": "dev"}), pol)

			decision, err := engine.Place(t.Context(), identity(nil), Request{TargetDark: tt.targetDark})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Place() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Place() = %v, want a placement", err)
			}
			if decision.Cell != tt.wantCell {
				t.Errorf("placed on %q, want %q", decision.Cell, tt.wantCell)
			}
		})
	}
}

// TestStaleCellIsNeverChosen is the staleness guard reaching placement. A
// missing capacity entry reads as zero utilisation, which is the best possible
// score, so an unguarded engine sends every deploy at the cell that broke.
func TestStaleCellIsNeverChosen(t *testing.T) {
	index := capacity.New(capacity.Options{Staleness: time.Hour})
	// Only c2 reports. c1 is registered, permitted and LIVE, but silent.
	if err := index.Report(capacity.Report{
		Cell: "c2", CPUMilliAllocatable: 10000, CPUMilliCommitted: 9000,
		MemoryBytesAllocatable: 10000,
	}); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}

	engine := newEngine(t, index,
		cell("c1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c2", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	decision, err := engine.Place(t.Context(), identity(nil), Request{})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	if decision.Cell != "c2" {
		t.Fatalf("placed on %q, want c2; the silent cell must not win by looking empty", decision.Cell)
	}
	if got := candidateFor(t, decision, "c1").Stage; got != StageCapacity {
		t.Errorf("c1 refused at stage %q, want %q", got, StageCapacity)
	}
}

// TestRefusalsAreDistinguishable pins the four failure modes apart. An
// authorization refusal and a fleet-wide capacity blackout must never be
// handled the same way by the client (ENG-175).
func TestRefusalsAreDistinguishable(t *testing.T) {
	permitting := policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"})

	tests := []struct {
		name string
		objs []client.Object
		util map[string]float64
		want error
	}{
		{
			name: "no policy",
			objs: []client.Object{cell("c1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"})},
			util: map[string]float64{"c1": 0.1},
			want: ErrNoPolicy,
		},
		{
			name: "policy selector matches no cell",
			objs: []client.Object{
				cell("c1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "prd"}),
				permitting,
			},
			util: map[string]float64{"c1": 0.1},
			want: ErrNoPermittedCells,
		},
		{
			name: "every permitted cell is draining",
			objs: []client.Object{
				cell("c1", cellcastv1alpha1.ClusterStateDraining, map[string]string{"env": "dev"}),
				permitting,
			},
			util: map[string]float64{"c1": 0.1},
			want: ErrNoEligibleCells,
		},
		{
			name: "every permitted cell has unusable capacity",
			objs: []client.Object{
				cell("c1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
				permitting,
			},
			util: nil,
			want: ErrCapacityUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := newEngine(t, loaded(t, tt.util), tt.objs...)
			_, err := engine.Place(t.Context(), identity(nil), Request{})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Place() = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestNoCapacityIndexFailsClosed pins that a hub with no capacity view refuses
// rather than picking arbitrarily.
func TestNoCapacityIndexFailsClosed(t *testing.T) {
	engine := newEngine(t, nil,
		cell("c1", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	if _, err := engine.Place(t.Context(), identity(nil), Request{}); !errors.Is(err, ErrCapacityUnknown) {
		t.Fatalf("Place() with no capacity index = %v, want ErrCapacityUnknown", err)
	}
}

func TestLeastLoadedPicksTheEmptiestAndBreaksTiesByName(t *testing.T) {
	index := loaded(t, map[string]float64{"c-a": 0.5, "c-b": 0.2, "c-c": 0.2})
	engine := newEngine(t, index,
		cell("c-a", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c-b", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c-c", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	// Repeated so that a map-iteration-order dependency shows up rather than
	// passing four times out of five.
	for i := range 20 {
		decision, err := engine.Place(t.Context(), identity(nil), Request{})
		if err != nil {
			t.Fatalf("Place() call %d = %v, want a placement", i, err)
		}
		if decision.Cell != "c-b" {
			t.Fatalf("call %d placed on %q, want c-b every time", i, decision.Cell)
		}
	}
}

func TestRoundRobinRotatesDeterministically(t *testing.T) {
	index := loaded(t, map[string]float64{"c-a": 0.9, "c-b": 0.1, "c-c": 0.5})
	pol := policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"})
	pol.Spec.Strategy = cellcastv1alpha1.ScoringRoundRobin

	engine := newEngine(t, index,
		cell("c-a", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c-b", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c-c", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		pol,
	)

	var got []string
	for range 7 {
		decision, err := engine.Place(t.Context(), identity(nil), Request{})
		if err != nil {
			t.Fatalf("Place() = %v, want a placement", err)
		}
		got = append(got, decision.Cell)
	}

	want := []string{"c-a", "c-b", "c-c", "c-a", "c-b", "c-c", "c-a"}
	if !slices.Equal(got, want) {
		t.Errorf("round robin sequence = %v, want %v", got, want)
	}
}

// TestStrategyComesFromPolicyNotTheRequest pins the control that stops a caller
// steering itself. Request has no strategy field at all, which is the point;
// this asserts the policy's choice is what runs.
func TestStrategyComesFromPolicyNotTheRequest(t *testing.T) {
	index := loaded(t, map[string]float64{"c-a": 0.9, "c-b": 0.1})
	pol := policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"})
	pol.Spec.Strategy = cellcastv1alpha1.ScoringRoundRobin

	engine := newEngine(t, index,
		cell("c-a", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c-b", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		pol,
	)

	decision, err := engine.Place(t.Context(), identity(nil), Request{})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	if decision.Strategy != cellcastv1alpha1.ScoringRoundRobin {
		t.Errorf("strategy = %q, want %q", decision.Strategy, cellcastv1alpha1.ScoringRoundRobin)
	}
	// Round robin starts at the first cell by name, which is the loaded one.
	// Least-loaded would have returned c-b.
	if decision.Cell != "c-a" {
		t.Errorf("placed on %q, want c-a; the policy's strategy did not run", decision.Cell)
	}
}

func TestUnsetStrategyDefaultsToLeastLoaded(t *testing.T) {
	index := loaded(t, map[string]float64{"c-a": 0.9, "c-b": 0.1})
	engine := newEngine(t, index,
		cell("c-a", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("c-b", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	decision, err := engine.Place(t.Context(), identity(nil), Request{})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	if decision.Strategy != cellcastv1alpha1.ScoringLeastLoaded || decision.Cell != "c-b" {
		t.Errorf("decision = %+v, want LeastLoaded on c-b", decision)
	}
}

// TestTraceCoversEveryRegisteredCell pins what --explain (ENG-114) renders. A
// cell missing from the trace is a cell whose absence the caller cannot explain.
func TestTraceCoversEveryRegisteredCell(t *testing.T) {
	index := loaded(t, map[string]float64{"live-dev": 0.2})
	engine := newEngine(t, index,
		cell("live-dev", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("drain-dev", cellcastv1alpha1.ClusterStateDraining, map[string]string{"env": "dev"}),
		cell("quiet-dev", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		cell("prod", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "prd"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	decision, err := engine.Place(t.Context(), identity(nil), Request{})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}

	var names []string
	for _, c := range decision.Candidates {
		names = append(names, c.Cell)
	}
	want := []string{"drain-dev", "live-dev", "prod", "quiet-dev"}
	if !slices.Equal(names, want) {
		t.Fatalf("trace covers %v, want %v ordered by name", names, want)
	}

	stages := map[string]string{
		"prod":      StagePermission,
		"drain-dev": StageState,
		"quiet-dev": StageCapacity,
	}
	for name, wantStage := range stages {
		if got := candidateFor(t, decision, name).Stage; got != wantStage {
			t.Errorf("%s refused at stage %q, want %q", name, got, wantStage)
		}
	}
	if c := candidateFor(t, decision, "live-dev"); !c.Admitted || c.Reason != "" {
		t.Errorf("live-dev = %+v, want admitted with no reason", c)
	}
}

func TestMostSpecificPolicyWins(t *testing.T) {
	index := loaded(t, map[string]float64{"broad": 0.1, "narrow": 0.9})
	engine := newEngine(t, index,
		cell("broad", cellcastv1alpha1.ClusterStateLive, map[string]string{"tier": "broad"}),
		cell("narrow", cellcastv1alpha1.ClusterStateLive, map[string]string{"tier": "narrow"}),
		policy("a-broad", []cellcastv1alpha1.SubjectSelector{
			subject(map[string]string{"repository": "example/app"}),
		}, map[string]string{"tier": "broad"}),
		policy("b-narrow", []cellcastv1alpha1.SubjectSelector{
			subject(map[string]string{"repository": "example/app", "environment": "production"}),
		}, map[string]string{"tier": "narrow"}),
	)

	id := identity(map[string]string{"repository": "example/app", "environment": "production"})
	decision, err := engine.Place(t.Context(), id, Request{})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	if decision.Policy != "b-narrow" {
		t.Errorf("policy = %q, want b-narrow; the exception must beat the general rule", decision.Policy)
	}
	// Chosen despite being the more loaded cell, which proves the policy
	// selection drove it rather than the score.
	if decision.Cell != "narrow" {
		t.Errorf("placed on %q, want narrow", decision.Cell)
	}
}

// TestEquallySpecificPoliciesResolveByName pins that an ambiguous
// configuration still produces one stable answer rather than a coin flip.
func TestEquallySpecificPoliciesResolveByName(t *testing.T) {
	index := loaded(t, map[string]float64{"a-cell": 0.5, "z-cell": 0.1})
	engine := newEngine(t, index,
		cell("a-cell", cellcastv1alpha1.ClusterStateLive, map[string]string{"tier": "a"}),
		cell("z-cell", cellcastv1alpha1.ClusterStateLive, map[string]string{"tier": "z"}),
		policy("aaa", []cellcastv1alpha1.SubjectSelector{
			subject(map[string]string{"repository": "example/app"}),
		}, map[string]string{"tier": "a"}),
		policy("zzz", []cellcastv1alpha1.SubjectSelector{
			subject(map[string]string{"repository": "example/app"}),
		}, map[string]string{"tier": "z"}),
	)

	id := identity(map[string]string{"repository": "example/app"})
	for i := range 10 {
		decision, err := engine.Place(t.Context(), id, Request{})
		if err != nil {
			t.Fatalf("Place() call %d = %v, want a placement", i, err)
		}
		if decision.Policy != "aaa" {
			t.Fatalf("call %d chose policy %q, want aaa every time", i, decision.Policy)
		}
	}
}

func candidateFor(t *testing.T, d *Decision, cell string) Candidate {
	t.Helper()
	for _, c := range d.Candidates {
		if c.Cell == cell {
			return c
		}
	}
	t.Fatalf("cell %q is missing from the trace %+v", cell, d.Candidates)
	return Candidate{}
}

// TestPolicyClaimKeysMatchWhatTheAuthenticatorProduces closes the seam between
// ENG-172 and this package.
//
// A policy constraining "repo" against an authenticator that emits "repository"
// matches nothing, and a policy that matches nothing is a silent deny-all that
// looks correct in Git. Nothing else in either package would catch it: the
// authenticator's tests assert the claims it extracts, this package's tests
// assert what it does with claims, and neither compares the two vocabularies.
func TestPolicyClaimKeysMatchWhatTheAuthenticatorProduces(t *testing.T) {
	for _, provider := range []string{"github", "buildkite"} {
		p, err := oidc.ProviderFor(provider)
		if err != nil {
			t.Fatalf("ProviderFor(%q) = %v, want nil", provider, err)
		}

		for _, claim := range p.Claims {
			id := &hub.Identity{Issuer: githubIssuer, Claims: map[string]string{claim: "value"}}
			sel := cellcastv1alpha1.SubjectSelector{
				Issuer: githubIssuer,
				Claims: map[string]string{claim: "value"},
			}
			if !matchSubject(sel, id) {
				t.Errorf("a policy constraining %q does not match an identity carrying it", claim)
			}
		}
	}
}

// TestSubjectClaimsAreExactNotPrefix pins that claim matching cannot be widened
// by a value that merely starts the same way. "example/app-staging" must not
// satisfy a policy naming "example/app".
func TestSubjectClaimsAreExactNotPrefix(t *testing.T) {
	sel := subject(map[string]string{"repository": "example/app"})

	for _, claim := range []string{"example/app-staging", "example/app2", "example/ap", "EXAMPLE/APP"} {
		if matchSubject(sel, identity(map[string]string{"repository": claim})) {
			t.Errorf("repository %q matched a policy naming example/app", claim)
		}
	}
	if !matchSubject(sel, identity(map[string]string{"repository": "example/app"})) {
		t.Error("the exact repository did not match")
	}
}

package placement

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1beta1 "github.com/ethan-kane-ops/cellcast/api/v1beta1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

var devLabels = map[string]string{"env": "dev"}

// fleet is an engine with a memory over one fake client, indexed the way the
// manager indexes the real one, on a clock the test moves.
type fleet struct {
	engine *Engine
	memory *Memory
	k8s    client.Client
	index  *capacity.Registry
	now    time.Time
}

func newFleet(t *testing.T, util map[string]float64, objs ...client.Object) *fleet {
	t.Helper()
	f := &fleet{
		index: loaded(t, util),
		// Whole seconds, because a stored timestamp keeps no more than that.
		now: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}
	f.k8s = fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithIndex(&cellcastv1beta1.WorkloadPlacement{}, PolicyIndex, IndexPolicy).
		Build()
	f.memory = NewMemory(f.k8s, testNamespace, MemoryOptions{
		ExpireAfter:  DefaultExpireAfter,
		MaxPerPolicy: DefaultMaxPerPolicy,
	}, discardLogger())
	f.memory.now = func() time.Time { return f.now }
	f.engine = NewEngine(f.k8s, f.index, testNamespace, discardLogger(), WithMemory(f.memory))
	return f
}

func (f *fleet) advance(d time.Duration) { f.now = f.now.Add(d) }

// deploy places a workload and remembers it, which is what the hub does once
// the mint has succeeded.
func (f *fleet) deploy(t *testing.T, id *identity.Identity, workload string) *Decision {
	t.Helper()
	d, err := f.engine.Place(t.Context(), id, Request{Workload: workload})
	if err != nil {
		t.Fatalf("Place(%s) = %v, want a placement", workload, err)
	}
	if err := f.engine.Remember(t.Context(), d); err != nil {
		t.Fatalf("Remember(%s) = %v, want nil", workload, err)
	}
	return d
}

// load replaces what a cell last reported.
func (f *fleet) load(t *testing.T, cell string, utilisation float64) {
	t.Helper()
	err := f.index.Report(capacity.Report{
		Cell:                   cell,
		Nodes:                  3,
		CPUMilliAllocatable:    10000,
		CPUMilliCommitted:      int64(utilisation * 10000),
		MemoryBytesAllocatable: 10000,
	})
	if err != nil {
		t.Fatalf("Report(%s) = %v, want nil", cell, err)
	}
}

func (f *fleet) records(t *testing.T) []cellcastv1beta1.WorkloadPlacement {
	t.Helper()
	var list cellcastv1beta1.WorkloadPlacementList
	if err := f.k8s.List(t.Context(), &list, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("listing workload placements: %v", err)
	}
	return list.Items
}

func (f *fleet) editCell(t *testing.T, name string, edit func(*cellcastv1beta1.Cluster)) {
	t.Helper()
	var cl cellcastv1beta1.Cluster
	if err := f.k8s.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: name}, &cl); err != nil {
		t.Fatalf("Get(%s) = %v", name, err)
	}
	edit(&cl)
	if err := f.k8s.Update(t.Context(), &cl); err != nil {
		t.Fatalf("Update(%s) = %v", name, err)
	}
}

// twoCells is the fleet most of these tests run on: c-a is the emptier, so
// least-loaded places a new workload there.
func twoCells(t *testing.T, pol *cellcastv1beta1.PlacementPolicy) *fleet {
	t.Helper()
	return newFleet(t, map[string]float64{"c-a": 0.1, "c-b": 0.5},
		cell("c-a", cellcastv1beta1.ClusterStateLive, devLabels),
		cell("c-b", cellcastv1beta1.ClusterStateLive, devLabels),
		pol,
	)
}

func anyCaller() *cellcastv1beta1.PlacementPolicy {
	return policy("p", []cellcastv1beta1.SubjectSelector{subject(nil)}, devLabels)
}

func verdictFor(t *testing.T, d *Decision, name string) Candidate {
	t.Helper()
	for _, c := range d.Candidates {
		if c.Cell == name {
			return c
		}
	}
	t.Fatalf("%s is not in the candidate table %+v", name, d.Candidates)
	return Candidate{}
}

// TestASecondDeployStaysWhereTheFirstLanded is the ticket. Utilisation shifts
// between two deploys of one workload, and without stickiness the second lands
// somewhere else and the service is split across two cells.
func TestASecondDeployStaysWhereTheFirstLanded(t *testing.T) {
	f := twoCells(t, anyCaller())

	first := f.deploy(t, caller(nil), "checkout")
	if first.Cell != "c-a" || first.Previous != "" {
		t.Fatalf("first placement = %q (previous %q), want c-a with nothing remembered", first.Cell, first.Previous)
	}

	// Least-loaded alone would now choose c-b.
	f.load(t, "c-a", 0.8)

	second := f.deploy(t, caller(nil), "checkout")
	if second.Cell != "c-a" || !second.Kept() {
		t.Errorf("second placement = %q (previous %q), want c-a, kept where the first landed", second.Cell, second.Previous)
	}

	// A workload the hub has not placed before is scored as it always was.
	if other := f.deploy(t, caller(nil), "payments"); other.Cell != "c-b" || other.Previous != "" {
		t.Errorf("a new workload = %q (previous %q), want c-b, the least loaded", other.Cell, other.Previous)
	}
}

// TestDecidingWritesNothing keeps the property ADR-011 rests on: a replica
// answers a placement without writing, and a decision is remembered only when
// the hub asks, after a mint. Place alone is the whole of a dry run.
func TestDecidingWritesNothing(t *testing.T) {
	f := twoCells(t, anyCaller())

	for range 3 {
		if _, err := f.engine.Place(t.Context(), caller(nil), Request{Workload: "checkout"}); err != nil {
			t.Fatalf("Place() = %v, want a placement", err)
		}
	}
	if got := f.records(t); len(got) != 0 {
		t.Errorf("deciding wrote %d records, want none until Remember", len(got))
	}
}

// TestARememberedCellMustStillPassTheFilter is what makes stickiness safe to
// have. Memory chooses among the admitted and readmits nothing: a workload
// whose cell has become unavailable is placed afresh, and remembered where it
// lands so that it does not bounce back when the old cell returns.
func TestARememberedCellMustStillPassTheFilter(t *testing.T) {
	setState := func(state cellcastv1beta1.ClusterState) func(*testing.T, *fleet) {
		return func(t *testing.T, f *fleet) {
			f.editCell(t, "c-a", func(cl *cellcastv1beta1.Cluster) { cl.Spec.State = state })
		}
	}
	setEnv := func(env string) func(*testing.T, *fleet) {
		return func(t *testing.T, f *fleet) {
			f.editCell(t, "c-a", func(cl *cellcastv1beta1.Cluster) { cl.Labels = map[string]string{"env": env} })
		}
	}

	tests := []struct {
		name   string
		change func(*testing.T, *fleet)
		// undo puts c-a back, nil when it cannot come back.
		undo func(*testing.T, *fleet)
		// stage is where c-a is refused, empty when it is not a candidate at all.
		stage string
	}{
		{
			name:   "it is draining",
			change: setState(cellcastv1beta1.ClusterStateDraining),
			undo:   setState(cellcastv1beta1.ClusterStateLive),
			stage:  StageState,
		},
		{
			name:   "the policy no longer selects it",
			change: setEnv("prod"),
			undo:   setEnv("dev"),
			stage:  StagePermission,
		},
		{
			name: "it was deregistered",
			change: func(t *testing.T, f *fleet) {
				if err := f.k8s.Delete(t.Context(), cell("c-a", cellcastv1beta1.ClusterStateLive, devLabels)); err != nil {
					t.Fatalf("deleting c-a: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := twoCells(t, anyCaller())
			f.deploy(t, caller(nil), "checkout")
			tt.change(t, f)

			moved := f.deploy(t, caller(nil), "checkout")
			if moved.Cell != "c-b" {
				t.Fatalf("placed on %q, want c-b: the remembered cell cannot take it", moved.Cell)
			}
			if moved.Previous != "c-a" || moved.Kept() {
				t.Errorf("previous = %q, kept = %v; want c-a named as where it was, and not kept", moved.Previous, moved.Kept())
			}
			if tt.stage != "" {
				if got := verdictFor(t, moved, "c-a").Stage; got != tt.stage {
					t.Errorf("c-a was refused at %q, want %q", got, tt.stage)
				}
			}

			if tt.undo == nil {
				return
			}
			// c-a is back and emptier than c-b, and the workload stays in c-b,
			// which is where it runs now.
			tt.undo(t, f)
			if again := f.deploy(t, caller(nil), "checkout"); again.Cell != "c-b" || !again.Kept() {
				t.Errorf("after c-a returned the workload went to %q (kept %v), want it kept in c-b", again.Cell, again.Kept())
			}
		})
	}
}

// TestStickinessNoneDecidesEveryPlacementAfresh is the opt out, for workloads
// meant to spread.
func TestStickinessNoneDecidesEveryPlacementAfresh(t *testing.T) {
	pol := anyCaller()
	pol.Spec.Stickiness = &cellcastv1beta1.Stickiness{Mode: cellcastv1beta1.StickinessNone}
	f := twoCells(t, pol)

	f.deploy(t, caller(nil), "batch")
	f.load(t, "c-a", 0.8)

	if second := f.deploy(t, caller(nil), "batch"); second.Cell != "c-b" || second.Previous != "" {
		t.Errorf("second placement = %q (previous %q), want c-b decided afresh", second.Cell, second.Previous)
	}
	if got := f.records(t); len(got) != 0 {
		t.Errorf("a policy with stickiness None had %d placements remembered, want none", len(got))
	}
}

// TestADarkPlacementIsNeverRemembered: a smoke test on a dark cell must not
// make the next real deploy forget where the workload lives. The dark cell is
// never eligible for that deploy, so remembering it would be forgetting.
func TestADarkPlacementIsNeverRemembered(t *testing.T) {
	pol := anyCaller()
	pol.Spec.AllowDarkTargeting = true
	f := newFleet(t, map[string]float64{"c-a": 0.1, "c-b": 0.5, "d-1": 0.0},
		cell("c-a", cellcastv1beta1.ClusterStateLive, devLabels),
		cell("c-b", cellcastv1beta1.ClusterStateLive, devLabels),
		cell("d-1", cellcastv1beta1.ClusterStateDark, devLabels),
		pol,
	)
	f.deploy(t, caller(nil), "checkout")

	dark, err := f.engine.Place(t.Context(), caller(nil), Request{Workload: "checkout", TargetDark: true})
	if err != nil {
		t.Fatalf("dark Place() = %v, want a placement", err)
	}
	if dark.Cell != "d-1" || dark.Previous != "" {
		t.Errorf("dark placement = %q (previous %q), want d-1 with nothing read from memory", dark.Cell, dark.Previous)
	}
	if err := f.engine.Remember(t.Context(), dark); err != nil {
		t.Fatalf("Remember(dark) = %v, want nil", err)
	}

	f.load(t, "c-a", 0.8)
	if live := f.deploy(t, caller(nil), "checkout"); live.Cell != "c-a" || !live.Kept() {
		t.Errorf("the next live deploy went to %q (kept %v), want c-a: the smoke test changed where it lives", live.Cell, live.Kept())
	}
}

// TestAnExpiredRecordReadsAsNone whether or not the sweep has reached it, so a
// stalled sweep cannot keep a workload somewhere past its expiry.
func TestAnExpiredRecordReadsAsNone(t *testing.T) {
	f := twoCells(t, anyCaller())
	f.deploy(t, caller(nil), "checkout")

	f.advance(DefaultExpireAfter)
	f.load(t, "c-a", 0.8)

	d := f.deploy(t, caller(nil), "checkout")
	if d.Cell != "c-b" || d.Previous != "" {
		t.Errorf("placement after expiry = %q (previous %q), want c-b decided afresh", d.Cell, d.Previous)
	}
	// The expired record is reused rather than joined by a second one.
	if got := f.records(t); len(got) != 1 || got[0].Spec.Cell != "c-b" {
		t.Errorf("records = %+v, want one, naming c-b", got)
	}
}

// TestRememberWritesOnlyWhatChanged: a busy workload costs the API server one
// write an hour, not one per deploy.
func TestRememberWritesOnlyWhatChanged(t *testing.T) {
	f := twoCells(t, anyCaller())
	f.deploy(t, caller(nil), "checkout")
	written := f.records(t)[0].ResourceVersion

	f.advance(refreshAfter - time.Minute)
	f.deploy(t, caller(nil), "checkout")
	if got := f.records(t)[0].ResourceVersion; got != written {
		t.Errorf("a deploy to the same cell inside the hour rewrote the record (%s, was %s)", got, written)
	}

	f.advance(time.Minute)
	f.deploy(t, caller(nil), "checkout")
	rec := f.records(t)[0]
	if rec.ResourceVersion == written {
		t.Error("the record was not refreshed once it was an hour old")
	}
	if !rec.Spec.LastPlacedAt.Time.Equal(f.now) {
		t.Errorf("lastPlacedAt = %s, want %s", rec.Spec.LastPlacedAt.Time, f.now)
	}
}

// TestRecordsBelongToOnePolicy: a workload name is the caller's own string,
// and the same name under another policy is another team's workload.
func TestRecordsBelongToOnePolicy(t *testing.T) {
	teamA := policy("team-a", []cellcastv1beta1.SubjectSelector{subject(map[string]string{"repository": "acme/a"})}, devLabels)
	teamA.UID = "uid-team-a"
	teamB := policy("team-b", []cellcastv1beta1.SubjectSelector{subject(map[string]string{"repository": "acme/b"})}, devLabels)
	teamB.UID = "uid-team-b"
	f := newFleet(t, map[string]float64{"c-a": 0.1, "c-b": 0.5},
		cell("c-a", cellcastv1beta1.ClusterStateLive, devLabels),
		cell("c-b", cellcastv1beta1.ClusterStateLive, devLabels),
		teamA, teamB,
	)

	f.deploy(t, caller(map[string]string{"repository": "acme/a"}), "api")
	f.load(t, "c-a", 0.8)

	b := f.deploy(t, caller(map[string]string{"repository": "acme/b"}), "api")
	if b.Cell != "c-b" || b.Previous != "" {
		t.Errorf("team-b's api = %q (previous %q), want c-b: team-a's record decided it", b.Cell, b.Previous)
	}

	owners := map[string]string{"team-a": "uid-team-a", "team-b": "uid-team-b"}
	recs := f.records(t)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want one per policy", len(recs))
	}
	// Owned by the policy, so deleting the policy deletes what was remembered
	// under it.
	for _, rec := range recs {
		if len(rec.OwnerReferences) != 1 || string(rec.OwnerReferences[0].UID) != owners[rec.Spec.Policy] {
			t.Errorf("%s under %s is owned by %+v, want its policy", rec.Spec.Workload, rec.Spec.Policy, rec.OwnerReferences)
		}
	}
}

// TestTheMemoryStopsAtItsBoundAndPlacesAnyway: the bound stops a caller
// inventing workload names from filling the namespace, and costs only new
// workloads their memory.
func TestTheMemoryStopsAtItsBoundAndPlacesAnyway(t *testing.T) {
	f := twoCells(t, anyCaller())
	f.memory.opts.MaxPerPolicy = 2
	f.deploy(t, caller(nil), "one")
	f.deploy(t, caller(nil), "two")

	d, err := f.engine.Place(t.Context(), caller(nil), Request{Workload: "three"})
	if err != nil {
		t.Fatalf("Place(three) = %v, want a placement: a full memory refuses nobody", err)
	}
	if err := f.engine.Remember(t.Context(), d); !errors.Is(err, ErrMemoryFull) {
		t.Errorf("Remember(three) = %v, want ErrMemoryFull", err)
	}
	if got := f.records(t); len(got) != 2 {
		t.Errorf("records = %d, want the bound of 2", len(got))
	}

	f.load(t, "c-a", 0.8)
	if again := f.deploy(t, caller(nil), "one"); !again.Kept() {
		t.Errorf("a workload remembered before the bound was reached went to %q, want it kept in c-a", again.Cell)
	}
}

// TestAnyWorkloadNameCanBeRemembered: the name is the caller's own string, so
// the object name is derived from it rather than being it.
func TestAnyWorkloadNameCanBeRemembered(t *testing.T) {
	for _, name := range []string{"checkout", "Checkout API / v2", "team:app@sha", strings.Repeat("x", 300), "ünïcödé"} {
		if errs := validation.IsDNS1123Subdomain(recordName("p", name)); len(errs) > 0 {
			t.Errorf("recordName(%q) is not a valid object name: %v", name, errs)
		}
	}
	if recordName("ab", "c") == recordName("a", "bc") {
		t.Error("two different policy and workload pairs share a record")
	}

	f := twoCells(t, anyCaller())
	f.deploy(t, caller(nil), "Checkout API / v2")
	f.load(t, "c-a", 0.8)
	if d := f.deploy(t, caller(nil), "Checkout API / v2"); !d.Kept() {
		t.Errorf("a workload name that is not an object name went to %q, want it kept in c-a", d.Cell)
	}
}

// TestTheSweepDeletesOnlyExpiredRecords.
func TestTheSweepDeletesOnlyExpiredRecords(t *testing.T) {
	f := twoCells(t, anyCaller())
	f.deploy(t, caller(nil), "retired")
	f.advance(60 * 24 * time.Hour)
	f.deploy(t, caller(nil), "active")
	f.advance(31 * 24 * time.Hour)

	f.memory.sweep(t.Context())

	recs := f.records(t)
	if len(recs) != 1 || recs[0].Spec.Workload != "active" {
		t.Errorf("after the sweep = %+v, want only the record placed within the expiry", recs)
	}
}

// TestAKeptWorkloadDoesNotTurnTheRotation: under RoundRobin, stickiness spreads
// new workloads and leaves existing ones where they are, and a workload that
// stayed put does not use up a turn.
func TestAKeptWorkloadDoesNotTurnTheRotation(t *testing.T) {
	pol := anyCaller()
	pol.Spec.Strategy = cellcastv1beta1.ScoringRoundRobin
	f := newFleet(t, map[string]float64{"c-a": 0.5, "c-b": 0.5, "c-c": 0.5},
		cell("c-a", cellcastv1beta1.ClusterStateLive, devLabels),
		cell("c-b", cellcastv1beta1.ClusterStateLive, devLabels),
		cell("c-c", cellcastv1beta1.ClusterStateLive, devLabels),
		pol,
	)

	f.deploy(t, caller(nil), "one")
	if again := f.deploy(t, caller(nil), "one"); again.Cell != "c-a" || !again.Kept() {
		t.Fatalf("second deploy of one = %q, want it kept in c-a", again.Cell)
	}
	if two := f.deploy(t, caller(nil), "two"); two.Cell != "c-b" {
		t.Errorf("a new workload went to %q, want c-b: the kept placement took a turn", two.Cell)
	}
}

// TestARecordIsBelievedOnlyIfItNamesTheWorkload: a record is found by a name
// derived from its policy and workload, and then believed only if its spec says
// the same, so one copied under another workload's name steers nothing.
func TestARecordIsBelievedOnlyIfItNamesTheWorkload(t *testing.T) {
	f := twoCells(t, anyCaller())
	f.load(t, "c-a", 0.8)
	f.deploy(t, caller(nil), "payments")

	copied := f.records(t)[0].DeepCopy()
	copied.Name, copied.ResourceVersion = recordName("p", "checkout"), ""
	if err := f.k8s.Create(t.Context(), copied); err != nil {
		t.Fatalf("creating the copy: %v", err)
	}

	f.load(t, "c-a", 0.1)
	d, err := f.engine.Place(t.Context(), caller(nil), Request{Workload: "checkout"})
	if err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	if d.Cell != "c-a" || d.Previous != "" {
		t.Errorf("placed on %q (previous %q), want c-a decided afresh: payments' record steered checkout", d.Cell, d.Previous)
	}
}

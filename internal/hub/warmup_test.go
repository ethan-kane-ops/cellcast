package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// reportingCellInState is a registered cell, in a given placement state, whose
// agent is entitled to heartbeat.
func reportingCellInState(name string, state cellcastv1alpha1.ClusterState) *cellcastv1alpha1.Cluster {
	cluster := clusterFixture(name, 1, state)
	cluster.Spec.Reporter = &cellcastv1alpha1.ReporterIdentity{
		Issuer:  testAgent.Issuer,
		Subject: testAgent.Subject,
	}
	return cluster
}

// warmIndex returns an index holding a fresh report for each named cell.
func warmIndex(t *testing.T, cells ...string) *capacity.Registry {
	t.Helper()
	index := capacity.New(capacity.Options{})
	for _, cell := range cells {
		err := index.Report(capacity.Report{
			Cell:                   cell,
			Nodes:                  3,
			CPUMilliAllocatable:    12000,
			CPUMilliCommitted:      4000,
			MemoryBytesAllocatable: 64 << 30,
			MemoryBytesCommitted:   16 << 30,
		})
		if err != nil {
			t.Fatalf("Report(%s) = %v, want nil", cell, err)
		}
	}
	return index
}

func fleetReader(t *testing.T, cells ...client.Object) client.Reader {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(cells...).Build()
}

func TestFleetCoverageWaitsOnlyForCellsThatCouldReport(t *testing.T) {
	tests := []struct {
		name        string
		fleet       []client.Object
		reported    []string
		wantWarm    bool
		wantMissing []string
	}{
		{
			name:     "an empty fleet is covered by an empty index",
			wantWarm: true,
		},
		{
			name:     "every reporting cell has been heard from",
			fleet:    []client.Object{reportingCellInState("prod-euw1", cellcastv1alpha1.ClusterStateLive)},
			reported: []string{"prod-euw1"},
			wantWarm: true,
		},
		{
			name:        "one cell is still silent",
			fleet:       []client.Object{reportingCellInState("prod-euw1", cellcastv1alpha1.ClusterStateLive), reportingCellInState("prod-euw2", cellcastv1alpha1.ClusterStateLive)},
			reported:    []string{"prod-euw1"},
			wantWarm:    false,
			wantMissing: []string{"prod-euw2"},
		},
		{
			// Waiting on a cell that is not allowed to heartbeat is waiting
			// forever: the ingest path refuses a report for a cell with no
			// declared reporter.
			name:     "a cell with no reporter is not waited for",
			fleet:    []client.Object{registeredCell("prod-euw1")},
			wantWarm: true,
		},
		{
			// Draining a cell is the moment the hub most needs to be healthy,
			// so an emptied cell's silence must not hold up a rollout.
			name:     "a draining cell is not waited for",
			fleet:    []client.Object{reportingCellInState("prod-euw1", cellcastv1alpha1.ClusterStateDraining)},
			wantWarm: true,
		},
		{
			// Dark cells still take placements from a caller that asks for
			// one, so a hub that cannot score them is not warm.
			name:        "a dark cell is waited for",
			fleet:       []client.Object{reportingCellInState("qa-euw1", cellcastv1alpha1.ClusterStateDark)},
			wantWarm:    false,
			wantMissing: []string{"qa-euw1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := FleetCoverage(fleetReader(t, tt.fleet...), warmIndex(t, tt.reported...), testNamespace)

			warm, missing := check(t.Context())
			if warm != tt.wantWarm {
				t.Errorf("warm = %v, want %v (missing %v)", warm, tt.wantWarm, missing)
			}
			if !slices.Equal(missing, tt.wantMissing) {
				t.Errorf("missing = %v, want %v", missing, tt.wantMissing)
			}
		})
	}
}

func TestAStaleReportIsNotCoverage(t *testing.T) {
	// The window is what makes a report usable, so a cell heard from an hour
	// ago is exactly as unscoreable as one never heard from. Treating the entry
	// as coverage would let a replica go ready holding numbers the placement
	// engine has already discarded.
	index := capacity.New(capacity.Options{Staleness: time.Nanosecond})
	if err := index.Report(capacity.Report{Cell: "prod-euw1", CPUMilliAllocatable: 1000}); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}
	time.Sleep(time.Millisecond)

	check := FleetCoverage(
		fleetReader(t, reportingCellInState("prod-euw1", cellcastv1alpha1.ClusterStateLive)), index, testNamespace)

	warm, missing := check(t.Context())
	if warm {
		t.Error("warm = true on a stale report, want false")
	}
	if !slices.Equal(missing, []string{"prod-euw1"}) {
		t.Errorf("missing = %v, want [prod-euw1]", missing)
	}
}

// errReader fails every list, as a hub whose cache has gone away does.
type errReader struct{ client.Reader }

func (errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("informer cache is not running")
}

func TestAReplicaThatCannotReadTheRegistryIsNotWarm(t *testing.T) {
	// Not knowing what you are missing is not the same as missing nothing. The
	// tempting shortcut here is to return the cells the index does hold, which
	// reports a hub with no registry access as fully covered.
	check := FleetCoverage(errReader{}, warmIndex(t, "prod-euw1"), testNamespace)

	if warm, _ := check(t.Context()); warm {
		t.Error("warm = true with an unreadable registry, want false")
	}
}

// scriptedCheck answers from a list, repeating its last answer forever.
type scriptedCheck struct {
	answers []bool
	calls   atomic.Int64
}

func (s *scriptedCheck) check(context.Context) (bool, []string) {
	i := int(s.calls.Add(1)) - 1
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	if s.answers[i] {
		return true, nil
	}
	return false, []string{"prod-euw1"}
}

func warmupServer(t *testing.T, check WarmupCheck, warmupTimeout time.Duration) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WarmupTimeout = warmupTimeout
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), WithWarmupCheck(check))
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}
	return srv
}

func TestTheWarmupPollStopsOnceWarm(t *testing.T) {
	// Warmth latches, and the poll behind it has to stop as well. A poll that
	// kept running is one edit away from mirroring a later failing check back
	// onto the latch, and warmth that could fall back would take readiness with
	// it: one bad minute across the fleet would pull every replica out of the
	// Service at once, turning a degraded decision into no hub at all.
	script := &scriptedCheck{answers: []bool{true, false, false}}
	srv := warmupServer(t, script.check, time.Minute)

	srv.awaitWarmth(t.Context())
	if !srv.warm.Load() {
		t.Fatal("warm = false after a passing check, want true")
	}
	settled := script.calls.Load()

	time.Sleep(2 * warmupPollInterval)

	if got := script.calls.Load(); got != settled {
		t.Errorf("the warmup check ran %d more times after latching, want the poll stopped", got-settled)
	}
	if !srv.warm.Load() {
		t.Error("warm = false after latching, want it to have stayed")
	}
}

func TestAColdReplicaGoesReadyOnTheDeadlineAndStillRefusesToPlace(t *testing.T) {
	// The two halves of the same decision. Going ready is what lets agents
	// reach this replica at all, since a Service routes only to ready pods;
	// refusing to place is what stops it answering with a fleet it cannot see.
	srv := warmupServer(t, (&scriptedCheck{answers: []bool{false}}).check, 10*time.Millisecond)

	start := time.Now()
	srv.startup(t.Context())

	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("startup blocked for %s, want it to give up on the deadline", waited)
	}
	if !srv.ready.Load() {
		t.Error("ready = false past the warmup deadline, want true so agents can reach this replica")
	}
	if srv.warm.Load() {
		t.Error("warm = true with a failing check, want false")
	}
	if missing := srv.missingCells(); !slices.Equal(missing, []string{"prod-euw1"}) {
		t.Errorf("missingCells() = %v, want [prod-euw1] so the log names what it waited for", missing)
	}
}

func TestTheWarmupPollOutlivesTheReadinessDeadline(t *testing.T) {
	// The deadlock this exists to break: agents heartbeat through the Service,
	// a Service routes only to ready pods, so a replica that went ready cold is
	// the only thing that can make the reports it is waiting for arrive. A poll
	// that stopped at the deadline would leave that hub cold for life.
	script := &scriptedCheck{answers: []bool{false, false, true}}
	srv := warmupServer(t, script.check, time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv.startup(ctx)

	if !srv.ready.Load() {
		t.Fatal("ready = false past the deadline, want true")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !srv.warm.Load() {
		if time.Now().After(deadline) {
			t.Fatal("warm = false long after the check started passing, want the poll to have continued")
		}
		time.Sleep(warmupPollInterval / 4)
	}
}

// coldPlacementServer builds the API a replica serves before any cell has
// reported: everything wired, nothing heard from.
func coldPlacementServer(t *testing.T, cells ...client.Object) http.Handler {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cells...).Build()

	return testServer(t,
		WithClusterClient(k8s),
		WithCapacityRegistry(capacity.New(capacity.Options{})),
		WithAuthenticator(fixedIdentity{}),
		WithPlacer(&stubPlacer{decision: testDecision()}),
		WithMinter(&stubMinter{}),
		WithWarmupCheck((&scriptedCheck{answers: []bool{false}}).check),
	).apiHandler()
}

func TestAColdReplicaRefusesAPlacementItCannotScore(t *testing.T) {
	h := coldPlacementServer(t, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST = %d (%s), want 503", rec.Code, rec.Body)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding refusal: %v", err)
	}
	// Not CapacityUnknown, which says the fleet went dark. This one is a
	// property of the replica the caller happened to reach and clears itself,
	// so a retry is worth making (ADR-006).
	if got := refusal.Reason(body["reason"]); got != refusal.PlacementUnavailable {
		t.Errorf("reason = %q, want %q", got, refusal.PlacementUnavailable)
	}
}

func TestAColdReplicaMintsNothing(t *testing.T) {
	// The gate has to sit in front of the mint as well as the decision.
	// Refusing with a 503 and issuing a credential anyway would be worse than
	// not having the gate.
	minter := &stubMinter{}
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(registeredCell("prod-euw1")).Build()
	h := testServer(t,
		WithClusterClient(k8s),
		WithAuthenticator(fixedIdentity{}),
		WithPlacer(&stubPlacer{decision: testDecision()}),
		WithMinter(minter),
		WithWarmupCheck((&scriptedCheck{answers: []bool{false}}).check),
	).apiHandler()

	post(t, h, `{"workload":"checkout-api"}`)
	if minter.calledTimes != 0 {
		t.Errorf("Mint called %d times on a cold replica, want 0", minter.calledTimes)
	}
}

func TestAWarmReplicaPlacesNormally(t *testing.T) {
	// The other side of the gate. Without it a suite in which every placement
	// refuses would still pass.
	h := placementServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))

	if rec := post(t, h, `{"workload":"checkout-api"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}
}

func TestIngestStillWorksWhileCold(t *testing.T) {
	// The gate is on placement alone and has to stay there. A cold replica that
	// also refused registration and capacity reads could never be told about
	// the fleet it is waiting for, which is the same deadlock one level down.
	h := coldPlacementServer(t, registeredCell("prod-euw1"))

	for _, path := range []string{"/api/v1/clusters", "/api/v1/capacity"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusServiceUnavailable {
			t.Errorf("GET %s = 503 on a cold replica (%s), want it served", path, rec.Body)
		}
	}
}

func TestTheCellsAReplicaIsWaitingForAreOrdered(t *testing.T) {
	// Guards the log line an operator reads during a stalled rollout: the same
	// three cells should print in the same order every time rather than in map
	// order.
	fleet := []client.Object{
		reportingCellInState("prod-euw3", cellcastv1alpha1.ClusterStateLive),
		reportingCellInState("prod-euw1", cellcastv1alpha1.ClusterStateLive),
		reportingCellInState("prod-euw2", cellcastv1alpha1.ClusterStateLive),
	}
	check := FleetCoverage(fleetReader(t, fleet...), warmIndex(t), testNamespace)

	for range 5 {
		_, missing := check(t.Context())
		if want := []string{"prod-euw1", "prod-euw2", "prod-euw3"}; !slices.Equal(missing, want) {
			t.Fatalf("missing = %v, want %v", missing, want)
		}
	}
}

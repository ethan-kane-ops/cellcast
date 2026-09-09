package hub

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// The load balancer this file models is Kubernetes: the endpoint controller
// notices a pod is unready only on its next probe, and every kube-proxy then
// learns about the removal some time after that. Both delays are real and
// neither is under the hub's control, which is the whole reason DrainDelay
// exists.
const (
	probeInterval = 20 * time.Millisecond
	endpointLag   = 150 * time.Millisecond
)

// steadyPlacer answers every request the same way and records nothing.
//
// The shared stubs elsewhere in this package record what they were called with,
// which is the right shape for a test that makes one request and a data race
// for one that makes thousands at once.
type steadyPlacer struct{}

func (steadyPlacer) Place(context.Context, *identity.Identity, placement.Request) (*placement.Decision, error) {
	return testDecision(), nil
}

// replica is one hub process: its two listeners and the context that ends it.
type replica struct {
	apiAddr   string
	probeAddr string
	stop      context.CancelFunc
	// done closes when Run returns. A channel that is closed rather than sent
	// on, because more than one place waits for the same shutdown and a sent
	// value is only ever received once.
	done chan struct{}
	err  error
}

// startReplica brings up a hub on real ports, warm and able to place.
func startReplica(t *testing.T, drainDelay time.Duration) *replica {
	t.Helper()

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(registeredCell("prod-euw1")).Build()

	cfg := DefaultConfig()
	cfg.Addr = freeAddr(t)
	cfg.ProbeAddr = freeAddr(t)
	cfg.DrainDelay = drainDelay
	cfg.ShutdownTimeout = 5 * time.Second

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := NewServer(cfg, log,
		// The default auditor writes to stdout, and this test makes thousands
		// of placements.
		WithAuditor(audit.New(log, nil)),
		WithClusterClient(k8s),
		WithAuthenticator(fixedIdentity{}),
		WithPlacer(steadyPlacer{}),
		// Never called: the load is dry-run, which stops after the decision.
		// Wired anyway, because a nil minter is itself a refusal.
		WithMinter(&stubMinter{}),
	)
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{apiAddr: cfg.Addr, probeAddr: cfg.ProbeAddr, stop: cancel, done: make(chan struct{})}
	go func() {
		r.err = srv.Run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-r.done
		if r.err != nil {
			t.Errorf("Run() = %v, want nil on a clean shutdown", r.err)
		}
	})

	waitReady(t, r)
	return r
}

func waitReady(t *testing.T, r *replica) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if readyz(r) == http.StatusOK {
			return
		}
		time.Sleep(probeInterval / 2)
	}
	t.Fatalf("replica on %s never became ready", r.probeAddr)
}

func readyz(r *replica) int {
	resp, err := http.Get("http://" + r.probeAddr + "/readyz")
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// endpoints is the pool of replicas currently receiving traffic.
//
// It removes an unready replica the way Kubernetes does: after noticing on a
// probe, and then after a further lag while the removal propagates. A replica
// that stops listening the moment it goes unready is therefore still being
// sent traffic, which is the failure this test exists to catch.
type endpoints struct {
	mu   sync.Mutex
	pool []*replica
}

func (e *endpoints) add(r *replica) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pool = append(e.pool, r)
}

func (e *endpoints) remove(r *replica) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pool = slicesDelete(e.pool, r)
}

func (e *endpoints) pick() (*replica, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pool) == 0 {
		return nil, false
	}
	return e.pool[rand.IntN(len(e.pool))], true
}

func slicesDelete(pool []*replica, want *replica) []*replica {
	out := pool[:0]
	for _, r := range pool {
		if r != want {
			out = append(out, r)
		}
	}
	return out
}

// watch removes r from the pool once its readiness probe has failed and the
// removal has had time to propagate.
func (e *endpoints) watch(ctx context.Context, r *replica) {
	go func() {
		tick := time.NewTicker(probeInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if readyz(r) != http.StatusOK {
					time.Sleep(endpointLag)
					e.remove(r)
					return
				}
			}
		}
	}()
}

// load hammers the pool with placements and counts what failed.
type load struct {
	failures atomic.Int64
	sent     atomic.Int64
	firstErr atomic.Pointer[string]
	wg       sync.WaitGroup
}

// run sends placements through the pool until ctx is cancelled.
//
// Keep-alives are off. A pooled connection to a replica that has
// since shut down fails for reasons that have nothing to do with endpoint
// timing, and Go does not retry a POST; without this the test would measure
// connection reuse rather than the drain.
func (l *load) run(ctx context.Context, e *endpoints, workers int) {
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   2 * time.Second,
	}
	for range workers {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			for ctx.Err() == nil {
				target, ok := e.pick()
				if !ok {
					// No endpoints at all is a fleet with no hub, which is a
					// different failure from the one under test.
					time.Sleep(probeInterval)
					continue
				}
				l.sent.Add(1)
				if err := placeOnce(client, target); err != nil {
					l.failures.Add(1)
					msg := err.Error()
					l.firstErr.CompareAndSwap(nil, &msg)
				}
			}
		}()
	}
}

func placeOnce(client *http.Client, r *replica) error {
	body := strings.NewReader(`{"workload":"checkout-api","dryRun":true}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+r.apiAddr+"/api/v1/placement", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (l *load) stop() { l.wg.Wait() }

// rollUnderLoad replaces one replica with another while placements are in
// flight, and reports how many of them failed.
func rollUnderLoad(t *testing.T, drainDelay time.Duration) (failures, sent int64, firstErr string) {
	t.Helper()

	pool := &endpoints{}
	old := startReplica(t, drainDelay)
	pool.add(old)

	loadCtx, stopLoad := context.WithCancel(context.Background())
	defer stopLoad()
	l := &load{}
	l.run(loadCtx, pool, 8)

	// Let the load settle before anything moves, so a failure cannot be an
	// artefact of the first request racing the first listener.
	time.Sleep(5 * probeInterval)

	// The new replica joins first. maxUnavailable: 0 is what makes that the
	// order, and without it there is no version of this that keeps working.
	fresh := startReplica(t, drainDelay)
	pool.add(fresh)

	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	pool.watch(watchCtx, old)

	old.stop()
	<-old.done

	// One more lag's worth of traffic after the old replica is gone, to catch a
	// pool that was still pointing at it.
	time.Sleep(endpointLag)
	stopLoad()
	l.stop()

	if p := l.firstErr.Load(); p != nil {
		firstErr = *p
	}
	return l.failures.Load(), l.sent.Load(), firstErr
}

// TestARollingUpdateDropsNoPlacements is the acceptance criterion for ADR-011.
//
// The hub replaces one replica with another while placements are in flight and
// nothing fails. A deploy failing because cellcast was being upgraded is the
// exact outage this project exists to prevent, and it is the one nobody
// notices until it happens during someone else's incident.
func TestARollingUpdateDropsNoPlacements(t *testing.T) {
	failures, sent, firstErr := rollUnderLoad(t, 10*probeInterval+2*endpointLag)

	if sent < 50 {
		t.Fatalf("only %d placements were attempted, too few to conclude anything", sent)
	}
	if failures != 0 {
		t.Errorf("%d of %d placements failed during a rolling update, want 0 (first: %s)",
			failures, sent, firstErr)
	}
}

// TestWithoutTheDrainDelayARollingUpdateDropsPlacements is the control.
//
// The test above asserts an absence, and an absence is also what a harness that
// measures nothing reports. Running the same roll with the drain disabled has
// to produce the failures, or the assertion above is decoration.
func TestWithoutTheDrainDelayARollingUpdateDropsPlacements(t *testing.T) {
	failures, sent, _ := rollUnderLoad(t, 0)

	if sent < 50 {
		t.Fatalf("only %d placements were attempted, too few to conclude anything", sent)
	}
	if failures == 0 {
		t.Errorf("0 of %d placements failed with no drain delay; the harness is not measuring the drain", sent)
	}
}

func TestADrainingReplicaReportsUnreadyAndKeepsServing(t *testing.T) {
	// The two have to be true at the same time, and each alone is useless. Only
	// reporting unready still refuses the traffic already on its way here; only
	// keeping the listener open never gets the replica taken out of rotation.
	r := startReplica(t, time.Second)

	r.stop()

	deadline := time.Now().Add(2 * time.Second)
	var sawUnready bool
	for time.Now().Before(deadline) {
		if readyz(r) == http.StatusServiceUnavailable {
			sawUnready = true
			break
		}
		time.Sleep(probeInterval / 4)
	}
	if !sawUnready {
		t.Fatal("readyz never reported 503 after the shutdown signal")
	}

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
	if err := placeOnce(client, r); err != nil {
		t.Errorf("placement during the drain window = %v, want it served", err)
	}

	<-r.done
	if err := placeOnce(client, r); err == nil {
		t.Error("placement after the drain window succeeded, want the listener closed")
	}
}

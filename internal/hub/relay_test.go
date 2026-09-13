package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1beta1 "github.com/ethan-kane-ops/cellcast/api/v1beta1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// recordingRelayer records what the handler hands it, synchronously, so a test
// can tell a report that was not relayed from one that was not relayed yet.
type recordingRelayer struct {
	mu    sync.Mutex
	calls []relayCall
}

type relayCall struct {
	cell, body, authorization string
}

func (r *recordingRelayer) Relay(_ context.Context, cell string, body []byte, authorization string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, relayCall{cell: cell, body: string(body), authorization: authorization})
}

func (r *recordingRelayer) recorded() []relayCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// relayingServer is capacityServer with a relay that records rather than sends.
func relayingServer(t *testing.T, objs ...client.Object) (*Server, *capacity.Registry, *recordingRelayer) {
	t.Helper()
	srv, index, _ := capacityServer(t, objs...)
	relay := &recordingRelayer{}
	srv.relay = relay
	return srv, index, relay
}

// postReport sends a capacity report with the headers an agent or a peer
// replica would set.
func postReport(t *testing.T, srv *Server, cell, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters/"+cell+"/capacity", strings.NewReader(body))
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	srv.apiHandler().ServeHTTP(rec, req)
	return rec
}

// otherCellsReporter is a cell whose declared reporter is not testAgent but the
// same subject from another cell's issuer, the impostor ADR-009 refuses.
func otherCellsReporter(name string) *cellcastv1beta1.Cluster {
	cl := clusterFixture(name, 1, cellcastv1beta1.ClusterStateLive)
	cl.Spec.Reporter = &cellcastv1beta1.ReporterIdentity{
		Issuer:  "https://oidc.c2.example.test",
		Subject: testAgent.Subject,
	}
	return cl
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func staticPeers(addrs ...string) PeerSource {
	return func(context.Context) ([]string, error) { return addrs, nil }
}

func TestAnAcceptedReportIsRelayedAsTheAgentSentIt(t *testing.T) {
	// Every peer authenticates the agent and checks spec.reporter for itself,
	// so what it is sent has to be what the agent sent: the same body, and the
	// agent's own Authorization header rather than anything of this replica's.
	srv, _, relay := relayingServer(t, reportingCell("c1"))

	rec := postReport(t, srv, "c1", validCapacityBody, http.Header{"Authorization": {"Bearer agent-token"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST capacity = %d, want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body)
	}

	want := []relayCall{{cell: "c1", body: validCapacityBody, authorization: "Bearer agent-token"}}
	if got := relay.recorded(); !slices.Equal(got, want) {
		t.Errorf("relayed %+v, want %+v", got, want)
	}
}

func TestARefusedReportIsNotRelayed(t *testing.T) {
	// A peer would refuse it too, for the same reason, so relaying it would only
	// multiply every refused report by the number of replicas.
	tests := []struct {
		name string
		cell string
		body string
		objs []client.Object
		want int
	}{
		{name: "a cell nobody registered", cell: "ghost", body: validCapacityBody, want: http.StatusNotFound},
		{
			name: "a caller that is not the cell's reporter", cell: "c1", body: validCapacityBody,
			objs: []client.Object{otherCellsReporter("c1")}, want: http.StatusForbidden,
		},
		{
			name: "a report the index refuses", cell: "c1", body: `{"nodes":1}`,
			objs: []client.Object{reportingCell("c1")}, want: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, relay := relayingServer(t, tt.objs...)

			rec := postReport(t, srv, tt.cell, tt.body, http.Header{"Authorization": {"Bearer agent-token"}})
			if rec.Code != tt.want {
				t.Fatalf("POST capacity = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if got := relay.recorded(); len(got) != 0 {
				t.Errorf("relayed %+v, want nothing", got)
			}
		})
	}
}

func TestARelayedReportIsStoredAndNotRelayedAgain(t *testing.T) {
	// Every replica relays what it accepts from an agent, so a replica that
	// also relayed what it accepted from a peer would send each report round
	// the replicas for ever.
	srv, index, relay := relayingServer(t, reportingCell("c1"))

	rec := postReport(t, srv, "c1", validCapacityBody, http.Header{relayHeader: {"1"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST relayed capacity = %d, want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body)
	}
	if _, ok := index.Lookup("c1"); !ok {
		t.Error("a relayed report did not reach the index")
	}
	if got := relay.recorded(); len(got) != 0 {
		t.Errorf("a relayed report was relayed again: %+v", got)
	}
}

func TestTheRelayHeaderGrantsNothing(t *testing.T) {
	// Any caller can set the header. A replica that took it as a peer's word
	// that the report had been checked would store any authenticated caller's
	// numbers for any cell, which is T-07 undone.
	srv, index, _ := relayingServer(t, otherCellsReporter("c1"))

	rec := postReport(t, srv, "c1", validCapacityBody, http.Header{relayHeader: {"1"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST relayed capacity from the wrong reporter = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if _, ok := index.Lookup("c1"); ok {
		t.Error("a report carrying the relay header skipped the reporter check")
	}
}

// peerReceipt is what a peer saw of one relayed report.
type peerReceipt struct {
	method, path, body, authorization, relay string
}

// fakePeer is a replica that records every report relayed to it and answers
// each with status.
func fakePeer(t *testing.T, status int) (addr string, received func() []peerReceipt) {
	t.Helper()
	var mu sync.Mutex
	var got []peerReceipt
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, peerReceipt{
			method: r.Method, path: r.URL.Path, body: string(body),
			authorization: r.Header.Get("Authorization"), relay: r.Header.Get(relayHeader),
		})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() []peerReceipt {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func TestPeerRelaySendsTheReportToEveryPeer(t *testing.T) {
	first, firstGot := fakePeer(t, http.StatusAccepted)
	second, secondGot := fakePeer(t, http.StatusAccepted)
	relay := NewPeerRelay(staticPeers(first, second), discardLog())

	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")

	want := []peerReceipt{{
		method: http.MethodPost, path: "/api/v1/clusters/c1/capacity", body: validCapacityBody,
		authorization: "Bearer agent-token",
		// Marked, so the peer stores it without relaying it on.
		relay: "1",
	}}
	for name, got := range map[string][]peerReceipt{"first": firstGot(), "second": secondGot()} {
		if !slices.Equal(got, want) {
			t.Errorf("the %s peer received %+v, want %+v", name, got, want)
		}
	}
}

func TestPeerRelayFollowsNoRedirect(t *testing.T) {
	// The agent's token goes to the address the peer lookup returned. A
	// redirect is somebody else answering, and following it would hand them a
	// token that reports capacity for the cell.
	elsewhere, elsewhereGot := fakePeer(t, http.StatusAccepted)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+elsewhere+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)

	var logs bytes.Buffer
	relay := NewPeerRelay(staticPeers(redirecting.Listener.Addr().String()), slog.New(slog.NewTextHandler(&logs, nil)))
	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")

	if got := elsewhereGot(); len(got) != 0 {
		t.Errorf("the relay followed a redirect and sent %+v to an address the lookup never returned", got)
	}
	if !strings.Contains(logs.String(), "peer answered 307") {
		t.Errorf("a redirect was not reported as a failed relay:\n%s", logs.String())
	}
}

func TestPeerRelayNeverUsesAProxy(t *testing.T) {
	// A proxy configured for the hub's outbound calls sees what passes through
	// it, and every relay carries an agent's token.
	transport, ok := NewPeerRelay(staticPeers(), discardLog()).client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the relay's transport is not an *http.Transport")
	}
	if transport.Proxy != nil {
		t.Error("the relay's transport routes through a proxy, want it to dial each peer directly")
	}
}

func TestPeerRelayReportsAFailingPeerOnceAndNeverTheToken(t *testing.T) {
	// A peer that is down fails every relay, once per heartbeat per cell. One
	// warning when it starts and one line when it clears is what an operator
	// can act on; a warning per report buries it.
	const sentinel = "sentinel-agent-token"
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(flaky.Close)
	// Nothing listens here, so this failure is a refused connection, an error
	// whose text the client builds from the request.
	down := freeAddr(t)

	var logs bytes.Buffer
	relay := NewPeerRelay(staticPeers(flaky.Listener.Addr().String(), down), slog.New(slog.NewTextHandler(&logs, nil)))
	for range 3 {
		relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer "+sentinel)
	}
	if n := strings.Count(logs.String(), "is failing"); n != 2 {
		t.Errorf("logged %d failures for two peers failing three times each, want 2:\n%s", n, logs.String())
	}

	status.Store(http.StatusAccepted)
	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer "+sentinel)
	if n := strings.Count(logs.String(), "recovered"); n != 1 {
		t.Errorf("logged %d recoveries for the one peer that recovered, want 1:\n%s", n, logs.String())
	}

	if strings.Contains(logs.String(), sentinel) {
		t.Errorf("the relay logged the agent's token (docs/threat-model.md T-05):\n%s", logs.String())
	}
}

func TestPeerRelayForgetsAPeerThatIsGone(t *testing.T) {
	// A replica rolled away while failing is gone rather than recovered.
	// Remembering it would grow with every rollout, and its address may come
	// back as a new replica that deserves a warning of its own.
	down := freeAddr(t)
	listed := []string{down}
	peers := func(context.Context) ([]string, error) { return listed, nil }

	var logs bytes.Buffer
	relay := NewPeerRelay(peers, slog.New(slog.NewTextHandler(&logs, nil)))
	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")
	listed = nil
	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")
	listed = []string{down}
	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")

	if n := strings.Count(logs.String(), "is failing"); n != 2 {
		t.Errorf("logged %d failures for an address that failed, left and failed again, want 2:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "recovered") {
		t.Errorf("a peer that left was logged as recovered:\n%s", logs.String())
	}
}

func TestPeerRelayReportsALookupFailureOnce(t *testing.T) {
	failing := true
	peers := func(context.Context) ([]string, error) {
		if failing {
			return nil, errors.New("no such host")
		}
		return nil, nil
	}

	var logs bytes.Buffer
	relay := NewPeerRelay(peers, slog.New(slog.NewTextHandler(&logs, nil)))
	for range 3 {
		relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")
	}
	failing = false
	relay.relay(t.Context(), "c1", []byte(validCapacityBody), "Bearer agent-token")

	if n := strings.Count(logs.String(), "looking up peer replicas failed"); n != 1 {
		t.Errorf("logged %d lookup failures for three in a row, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "looking up peer replicas recovered"); n != 1 {
		t.Errorf("logged %d lookup recoveries, want 1:\n%s", n, logs.String())
	}
}

func TestResolvePeersLeavesOutThisReplica(t *testing.T) {
	// The peers Service resolves to every replica, this one included, and a
	// replica relaying to itself stores each report twice.
	const host = "cellcast-peers.cellcast-system.svc"
	lookup := func(_ context.Context, name string) ([]string, error) {
		if name != host {
			return nil, fmt.Errorf("looked up %q, want %q", name, host)
		}
		return []string{"10.0.0.1", "10.0.0.2", "fd00::3", "fd00:0:0::4"}, nil
	}
	// fd00::4 is spelled differently from how the lookup returns it, as an
	// interface address and a DNS answer can each spell one address.
	self := map[string]bool{"10.0.0.1": true, "fd00::4": true}

	got, err := resolvePeers(host, "8080", lookup, self)(t.Context())
	if err != nil {
		t.Fatalf("resolving peers: %v", err)
	}
	if want := []string{"10.0.0.2:8080", "[fd00::3]:8080"}; !slices.Equal(got, want) {
		t.Errorf("peers = %v, want %v", got, want)
	}
}

func TestResolvePeersNeedsAPort(t *testing.T) {
	if _, err := ResolvePeers("cellcast-peers.cellcast-system.svc"); err == nil {
		t.Error("ResolvePeers accepted a name with no port, want an error")
	}
}

func TestConfigRefusesPeersThatAreNotHostAndPort(t *testing.T) {
	for _, peers := range []string{"cellcast-peers", ":8080", "cellcast-peers:"} {
		cfg := DefaultConfig()
		cfg.Peers = peers
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() with peers %q = nil, want an error", peers)
		}
	}

	cfg := DefaultConfig()
	cfg.Peers = "cellcast-peers.cellcast-system.svc:8080"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with peers %q = %v, want nil", cfg.Peers, err)
	}
}

// tokenAuthenticator accepts one bearer token, as testAgent, and refuses every
// other request, so a relay that did not carry the agent's own token would be
// refused by the peer it reached.
type tokenAuthenticator struct{ token string }

func (a tokenAuthenticator) Authenticate(_ context.Context, r *http.Request) (*identity.Identity, error) {
	if r.Header.Get("Authorization") != "Bearer "+a.token {
		return nil, identity.ErrUnauthenticated
	}
	id := testAgent
	return &id, nil
}

func TestEveryReplicaHearsAReportThatReachedOne(t *testing.T) {
	// The agent keeps one connection open and a Service balances connections,
	// so each report from a cell reaches one replica. Three replicas here, each
	// with its own capacity index as in production, and the report is posted
	// to the first alone.
	const token = "agent-token"
	const replicas = 3
	k8s := newFakeClient(t, reportingCell("c1"))

	addrs := make([]string, replicas)
	indexes := make([]*capacity.Registry, replicas)
	servers := make([]*httptest.Server, replicas)
	for i := range replicas {
		others := func(context.Context) ([]string, error) {
			return slices.Delete(slices.Clone(addrs), i, i+1), nil
		}
		indexes[i] = capacity.New(capacity.Options{})
		srv := testServer(t,
			WithAuthenticator(tokenAuthenticator{token: token}),
			WithClusterClient(k8s),
			WithCapacityRegistry(indexes[i]),
			WithRelay(NewPeerRelay(others, discardLog())),
		)
		// Unstarted, so every address is known before any replica serves.
		servers[i] = httptest.NewUnstartedServer(srv.apiHandler())
		addrs[i] = servers[i].Listener.Addr().String()
	}
	for _, s := range servers {
		s.Start()
		t.Cleanup(s.Close)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		servers[0].URL+"/api/v1/clusters/c1/capacity", strings.NewReader(validCapacityBody))
	if err != nil {
		t.Fatalf("building the report: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := servers[0].Client().Do(req)
	if err != nil {
		t.Fatalf("posting the report: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST capacity = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	deadline := time.Now().Add(5 * time.Second)
	for i, index := range indexes {
		for {
			if entry, ok := index.Lookup("c1"); ok && entry.Health == capacity.HealthFresh {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("replica %d never heard from c1, which reported to replica 0 only", i)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

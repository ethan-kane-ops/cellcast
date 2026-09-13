package hub

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/propagation"
)

// relayHeader marks a capacity report one replica has passed on to another.
//
// It stops the report being passed on again, and that is all it does. The
// replica that receives it authenticates the agent's token and checks the
// cell's spec.reporter exactly as it would for the agent, so a caller that sets
// it gets nothing but a report that goes no further (docs/threat-model.md
// T-07).
const relayHeader = "X-Cellcast-Relay"

// relayTimeout bounds relaying one report: the peer lookup and every peer's
// answer together.
//
// A peer answers from its informer cache in milliseconds, so this is generous
// for one that is still starting, and it is well inside the default heartbeat
// interval. A relay that fails is abandoned long before the agent's next report
// is relayed in its place, and that next report is the retry.
const relayTimeout = 5 * time.Second

// Relayer passes a capacity report this replica accepted on to the others.
//
// Relay returns at once. The agent already has this replica's answer, and
// nothing a peer says could change what the agent should do next.
type Relayer interface {
	Relay(ctx context.Context, cell string, body []byte, authorization string)
}

// PeerSource lists the hub's other replicas as host:port addresses.
type PeerSource func(ctx context.Context) ([]string, error)

// PeerRelay relays capacity reports to every replica a PeerSource lists.
//
// Each relay carries the agent's own Authorization header, so every replica
// authenticates the agent and checks spec.reporter for itself (ADR-009). A
// relayed report is the agent's report delivered by another route, not one
// replica vouching for it to another (docs/architecture.md ADR-014).
type PeerRelay struct {
	peers  PeerSource
	client *http.Client
	log    *slog.Logger

	// What failed last time, so a failure is logged when it starts and when it
	// clears rather than on every report. A peer that is down fails every
	// relay, and a warning per heartbeat per cell buries the line that says so.
	mu           sync.Mutex
	failing      map[string]bool
	lookupFailed bool
}

// NewPeerRelay builds a relay to the replicas peers lists.
func NewPeerRelay(peers PeerSource, log *slog.Logger) *PeerRelay {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Straight to the peer. A proxy set for the hub's outbound traffic, which
	// issuer discovery may need, is not somewhere an agent's token should go.
	transport.Proxy = nil
	return &PeerRelay{
		peers: peers,
		client: &http.Client{
			Transport: transport,
			// The token goes to the address the peer lookup returned and
			// nowhere else. A replica never redirects, so a redirect is
			// something other than a replica answering.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log:     log,
		failing: make(map[string]bool),
	}
}

// Relay implements Relayer.
func (p *PeerRelay) Relay(ctx context.Context, cell string, body []byte, authorization string) {
	// Detached from the request, which ends as soon as the agent has its
	// answer, and still carrying its trace so the peers' spans join it.
	go p.relay(context.WithoutCancel(ctx), cell, body, authorization)
}

// relay sends one report to every peer, and returns once they have all
// answered or the timeout has passed.
func (p *PeerRelay) relay(ctx context.Context, cell string, body []byte, authorization string) {
	ctx, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()

	peers, err := p.peers(ctx)
	p.noteLookup(err)
	if err != nil {
		return
	}
	p.forgetAllBut(peers)

	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Go(func() { p.note(peer, cell, p.send(ctx, peer, cell, body, authorization)) })
	}
	wg.Wait()
}

// send posts the report to one peer as the agent posted it.
func (p *PeerRelay) send(ctx context.Context, peer, cell string, body []byte, authorization string) error {
	target := (&url.URL{Scheme: "http", Host: peer}).JoinPath("api", "v1", "clusters", cell, "capacity")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building relay request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	req.Header.Set(relayHeader, "1")
	traceContext.Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := p.client.Do(req)
	if err != nil {
		// Safe to log whole: a client error renders the URL, which holds a pod
		// address and a cell name, and never a header.
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body on a response we are done with
	// Drained so the connection to the peer is reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCapacityBytes))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("peer answered %d", resp.StatusCode)
	}
	return nil
}

// note records how a relay to one peer went, and logs only a change.
func (p *PeerRelay) note(peer, cell string, err error) {
	p.mu.Lock()
	was := p.failing[peer]
	if err != nil {
		p.failing[peer] = true
	} else {
		delete(p.failing, peer)
	}
	p.mu.Unlock()

	switch {
	case err != nil && !was:
		p.log.Warn("relaying capacity reports to a peer replica is failing, so it hears only from agents connected to it",
			slog.String("peer", peer),
			slog.String("cell", cell),
			slog.Any("error", err),
		)
	case err == nil && was:
		p.log.Info("relaying capacity reports to a peer replica recovered", slog.String("peer", peer))
	}
}

// noteLookup records whether the peer lookup worked, and logs only a change.
func (p *PeerRelay) noteLookup(err error) {
	p.mu.Lock()
	was := p.lookupFailed
	p.lookupFailed = err != nil
	p.mu.Unlock()

	switch {
	case err != nil && !was:
		p.log.Warn("looking up peer replicas failed, so no capacity report is relayed until it works",
			slog.Any("error", err))
	case err == nil && was:
		p.log.Info("looking up peer replicas recovered")
	}
}

// forgetAllBut drops what is remembered about peers no longer listed. A replica
// that was rolled away is gone rather than recovered, and its address may come
// back as a different replica that has never failed.
func (p *PeerRelay) forgetAllBut(peers []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for peer := range p.failing {
		if !slices.Contains(peers, peer) {
			delete(p.failing, peer)
		}
	}
}

// ResolvePeers lists the replicas behind hostport's name, less this one.
//
// The name is meant to be a headless Service that publishes unready addresses,
// which is what the chart creates. It resolves to every replica's own pod
// address, including one that is still starting, and that is the replica that
// most needs the reports: it cannot go ready until it holds them, and a Service
// routes nothing to it until it is ready (ADR-011).
//
// Looked up on every report rather than cached, so a replica that has just
// started hears the next heartbeat and one that has gone stops being sent them.
func ResolvePeers(hostport string) (PeerSource, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("parsing peers: %w", err)
	}
	self, err := localAddresses()
	if err != nil {
		return nil, err
	}
	return resolvePeers(host, port, net.DefaultResolver.LookupHost, self), nil
}

func resolvePeers(host, port string, lookup func(context.Context, string) ([]string, error), self map[string]bool) PeerSource {
	return func(ctx context.Context) ([]string, error) {
		addrs, err := lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("looking up %s: %w", host, err)
		}
		peers := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			if ip := net.ParseIP(addr); ip != nil && self[ip.String()] {
				continue
			}
			peers = append(peers, net.JoinHostPort(addr, port))
		}
		return peers, nil
	}
}

// localAddresses returns this process's own IP addresses, which are how it
// recognises itself among its peers. Every replica has a pod address of its
// own, so the address alone is enough.
func localAddresses() (map[string]bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing local addresses: %w", err)
	}
	self := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			self[ipnet.IP.String()] = true
		}
	}
	return self, nil
}

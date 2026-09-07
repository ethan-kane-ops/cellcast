package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/oidc"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// Timings for the live fleet. Short because the point is to watch a cell fall
// out of scoring inside a test rather than to model a production cadence.
const (
	fleetHeartbeat = 2 * time.Second
	fleetStaleness = 6 * time.Second
	fleetTimeout   = 90 * time.Second
)

// agentSubject is the `sub` claim every agent in the fleet carries. It is the
// same string in all three cells, which is why the binding is on the issuer.
const agentSubject = "system:serviceaccount:cellcast-system:cellcast-agent"

// liveCell is one entry in the fleet description written by `just verify-agent`.
type liveCell struct {
	Name       string `json:"name"`
	Kubeconfig string `json:"kubeconfig"`
	Issuer     string `json:"issuer"`
	CAFile     string `json:"ca"`
	AgentToken string `json:"agentToken"`
	// CallerToken authenticates a placement request. A separate ServiceAccount
	// from the agent's, so the test proves policy tells them apart rather than
	// that one identity happens to be allowed to do both.
	CallerToken string `json:"callerToken"`
	// Load is how many CPU-heavy pods the recipe parked on the cell, so the
	// expected scoring order is set by the fixture rather than by whichever
	// kind cluster happened to be busier.
	Load int `json:"load"`
}

// TestLiveAgentFleet is ENG-174's done-when: three real clusters, three real
// agents, one hub, and a cell that drops out of scoring when its agent dies.
//
// Every component is the real one, including the OIDC path. Each kind cluster
// is created with its own service account issuer, so the agents present tokens
// that differ only in the claim the binding actually relies on.
func TestLiveAgentFleet(t *testing.T) {
	if os.Getenv("CELLCAST_LIVE") != "1" {
		t.Skip("set CELLCAST_LIVE=1; run this through `just verify-agent`")
	}
	fleet := loadFleet(t)

	// The first cell's cluster doubles as the hub's own registry store.
	k8s := registryClient(t, fleet[0].Kubeconfig)
	seedAgentFleet(t, k8s, fleet)

	index := capacity.New(capacity.Options{
		Staleness:     fleetStaleness,
		Retention:     time.Hour,
		PruneInterval: time.Second,
	})

	addr := freeAddr(t)
	srv := agentFleetServer(t, k8s, index, fleet, addr)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v, want nil on a clean shutdown", err)
		}
	})
	waitForHub(t, srv)

	agents := make(map[string]*exec.Cmd, len(fleet))
	for _, cell := range fleet {
		agents[cell.Name] = startAgent(t, cell, addr)
	}

	t.Run("every cell reports and is scored", func(t *testing.T) {
		for _, cell := range fleet {
			entry := waitForHealth(t, index, cell.Name, capacity.HealthFresh)
			t.Logf("%s: %d nodes, %d/%d cpu millicores, %d pods, utilisation %.3f",
				cell.Name, entry.Nodes, entry.CPUMilliCommitted, entry.CPUMilliAllocatable,
				entry.Pods, entry.Utilisation)

			if entry.Nodes == 0 {
				t.Errorf("%s reported no nodes", cell.Name)
			}
			if entry.CPUMilliAllocatable == 0 || entry.MemoryBytesAllocatable == 0 {
				t.Errorf("%s reported nothing allocatable; the agent cannot see its nodes", cell.Name)
			}
			// The agent's own pods are running, so a cell reporting zero
			// committed CPU is one whose pod arithmetic is broken.
			if entry.CPUMilliCommitted == 0 {
				t.Errorf("%s reported no committed CPU with pods running", cell.Name)
			}
		}
	})

	t.Run("the agent's numbers match the cluster", func(t *testing.T) {
		// The hub is only as good as the arithmetic, so compare what the agent
		// published against what the API server says directly. kind cordons
		// nothing and every node is Ready, so a plain count is the right
		// comparison here; the exclusions have their own unit coverage.
		cell := fleet[0]
		entry, _ := index.Lookup(cell.Name)

		if want := kubectlCount(t, cell.Kubeconfig, "get", "nodes"); entry.Nodes != want {
			t.Errorf("%s reported %d nodes, the cluster has %d", cell.Name, entry.Nodes, want)
		}
		want := kubectlCount(t, cell.Kubeconfig, "get", "pods", "-A",
			"--field-selector=status.phase!=Succeeded,status.phase!=Failed")
		if entry.Pods != want {
			t.Errorf("%s reported %d pods, the cluster has %d non-terminal", cell.Name, entry.Pods, want)
		}
	})

	t.Run("placement scores the fleet by load", func(t *testing.T) {
		out := runFleetCLI(t, addr, fleet[0], "--workload", "checkout-api", "--dry-run", "--explain")
		t.Logf("\n%s", out)

		// The recipe parks load on the cells in a known order, so the emptiest
		// cell is known in advance rather than read back out of the answer.
		want := emptiest(fleet)
		if !strings.Contains(out, "would place checkout-api on "+want) {
			t.Errorf("placement did not choose %s, the least loaded cell:\n%s", want, out)
		}
		for _, cell := range fleet {
			if !strings.Contains(out, cell.Name) {
				t.Errorf("explain omits %s; every registered cell should appear:\n%s", cell.Name, out)
			}
		}
	})

	t.Run("an agent cannot report for another cell", func(t *testing.T) {
		// The T-07 assertion, with real tokens from real clusters. The two
		// tokens carry the identical `sub`, so anything that passes here
		// without comparing issuers is broken.
		victim, impostor := fleet[1], fleet[0]

		status, body := postCapacity(t, addr, victim.Name, readToken(t, impostor.AgentToken))
		if status != http.StatusForbidden {
			t.Errorf("cell %s accepted a report from %s's agent: %d %s",
				victim.Name, impostor.Name, status, body)
		}

		// Positive control. The same request from the cell's own agent is
		// accepted, so the refusal above is the binding and not a broken route.
		//
		// This writes a synthetic report into the index, which is why this
		// subtest runs after the load-ordering one rather than before it.
		status, body = postCapacity(t, addr, victim.Name, readToken(t, victim.AgentToken))
		if status != http.StatusAccepted {
			t.Errorf("cell %s refused its own agent: %d %s", victim.Name, status, body)
		}
	})

	t.Run("killing an agent drops its cell out of scoring", func(t *testing.T) {
		victim := emptiest(fleet)
		t.Logf("killing the agent for %s, the cell placement currently prefers", victim)

		if err := agents[victim].Process.Kill(); err != nil {
			t.Fatalf("killing the %s agent: %v", victim, err)
		}
		delete(agents, victim)

		waitForHealth(t, index, victim, capacity.HealthUnknown)

		out := runFleetCLI(t, addr, fleet[0], "--workload", "checkout-api", "--dry-run", "--explain")
		t.Logf("\n%s", out)

		if strings.Contains(out, "would place checkout-api on "+victim) {
			t.Errorf("placement still chose %s after its agent died:\n%s", victim, out)
		}
		// Excluded, not invisible. An operator asking why a cell stopped
		// receiving deploys has to be able to see that it went quiet.
		if !strings.Contains(out, victim) {
			t.Errorf("explain no longer mentions %s; a silent cell must be visible, not absent:\n%s", victim, out)
		}
	})

	for name, cmd := range agents {
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("stopping the %s agent: %v", name, err)
		}
	}
}

func loadFleet(t *testing.T) []liveCell {
	t.Helper()

	path := os.Getenv("CELLCAST_FLEET")
	if path == "" {
		t.Fatal("CELLCAST_FLEET is unset; run this through `just verify-agent`")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fleet description: %v", err)
	}

	var fleet []liveCell
	if err := json.Unmarshal(raw, &fleet); err != nil {
		t.Fatalf("decoding fleet description: %v", err)
	}
	if len(fleet) < 3 {
		t.Fatalf("fleet has %d cells, want at least 3", len(fleet))
	}
	return fleet
}

// emptiest returns the cell the recipe left least loaded.
func emptiest(fleet []liveCell) string {
	best := fleet[0]
	for _, cell := range fleet[1:] {
		if cell.Load < best.Load {
			best = cell
		}
	}
	return best.Name
}

func registryClient(t *testing.T, kubeconfig string) client.Client {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("loading %s: %v", kubeconfig, err)
	}
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building registry client: %v", err)
	}
	return k8s
}

// seedAgentFleet registers one Cluster per cell, each naming its own agent.
func seedAgentFleet(t *testing.T, k8s client.Client, fleet []liveCell) {
	t.Helper()

	objs := []client.Object{&cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-deployer", Namespace: testNamespace},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{{
				Issuer: fleet[0].Issuer,
				// Pinned to the deploying account. Without this the policy
				// would also cover the agent, which authenticates through the
				// same issuer (ADR-009).
				Subject: "system:serviceaccount:apps:deployer",
			}},
			PermittedCells: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "verify"}},
			Strategy:       cellcastv1alpha1.ScoringLeastLoaded,
		},
	}}

	for _, cell := range fleet {
		objs = append(objs, &cellcastv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: cell.Name, Namespace: testNamespace,
				Labels: map[string]string{"fleet": "verify"},
			},
			Spec: cellcastv1alpha1.ClusterSpec{
				Endpoint:       "https://" + cell.Name + ".invalid",
				Provider:       cellcastv1alpha1.ProviderGeneric,
				TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "unused-in-dry-run"},
				Reporter: &cellcastv1alpha1.ReporterIdentity{
					Issuer:  cell.Issuer,
					Subject: agentSubject,
				},
				State: cellcastv1alpha1.ClusterStateLive,
			},
		})
	}

	for _, obj := range objs {
		if err := k8s.Create(t.Context(), obj); err != nil {
			t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
		}
		t.Cleanup(func() { _ = k8s.Delete(context.Background(), obj) })
	}
}

// agentFleetServer builds a hub that verifies every cell's issuer for real.
func agentFleetServer(t *testing.T, k8s client.Client, index *capacity.Registry,
	fleet []liveCell, addr string) *Server {
	t.Helper()

	authCfg := oidc.DefaultConfig()
	authCfg.Audience = "cellcast"
	seen := map[string]bool{}
	for _, cell := range fleet {
		if seen[cell.Issuer] {
			// Two cells sharing an issuer is the collision ADR-009 warns about,
			// and it would make this test prove nothing.
			t.Fatalf("cells share the issuer %s; each cluster needs its own", cell.Issuer)
		}
		seen[cell.Issuer] = true
		authCfg.Issuers = append(authCfg.Issuers, oidc.IssuerConfig{Issuer: cell.Issuer, Provider: "generic"})
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	authn, err := oidc.New(t.Context(), authCfg, log, oidc.WithHTTPClient(fleetHTTPClient(t, fleet)))
	if err != nil {
		t.Fatalf("oidc.New() = %v, want nil", err)
	}

	hubCfg := DefaultConfig()
	hubCfg.Addr = addr
	hubCfg.ProbeAddr = freeAddr(t)
	hubCfg.Namespace = testNamespace
	hubCfg.CapacityStaleness = fleetStaleness

	engine := placement.NewEngine(k8s, index, hubCfg.Namespace, log)
	srv, err := NewServer(hubCfg, log,
		WithClusterClient(k8s),
		WithCapacityRegistry(index),
		WithPlacer(engine),
		WithAuthenticator(authn),
	)
	if err != nil {
		t.Fatalf("NewServer() = %v, want nil", err)
	}
	return srv
}

// fleetHTTPClient trusts every cluster's CA, so the hub can fetch each issuer's
// discovery document and key set.
func fleetHTTPClient(t *testing.T, fleet []liveCell) *http.Client {
	t.Helper()

	pool := x509.NewCertPool()
	for _, cell := range fleet {
		pem, err := os.ReadFile(cell.CAFile)
		if err != nil {
			t.Fatalf("reading %s: %v", cell.CAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			t.Fatalf("%s has no usable certificate", cell.CAFile)
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

// startAgent runs the built agent binary against one cell.
func startAgent(t *testing.T, cell liveCell, hubAddr string) *exec.Cmd {
	t.Helper()

	bin, err := filepath.Abs("../../bin/cellcast-agent")
	if err != nil {
		t.Fatalf("resolving the agent binary: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s not built; run `just build` first", bin)
	}

	// Deliberately not t.Context: the test kills these itself, and a context
	// cancelled by test teardown would race the assertions that do the killing.
	cmd := exec.Command(bin, //nolint:gosec // a path this test built itself
		"--hub-endpoint=http://"+hubAddr,
		"--cell-name="+cell.Name,
		"--kubeconfig="+cell.Kubeconfig,
		"--token-path="+cell.AgentToken,
		"--namespace=cellcast-system",
		"--heartbeat-interval="+fleetHeartbeat.String(),
		"--request-timeout=1s",
		"--max-backoff="+fleetHeartbeat.String(),
		"--probe-addr="+freeAddr(t),
		"--log-format=text",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the %s agent: %v", cell.Name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// waitForHealth blocks until a cell reaches the wanted health.
func waitForHealth(t *testing.T, index *capacity.Registry, cell string, want capacity.Health) capacity.Entry {
	t.Helper()

	deadline := time.Now().Add(fleetTimeout)
	var last capacity.Entry
	for time.Now().Before(deadline) {
		entry, ok := index.Lookup(cell)
		if ok && entry.Health == want {
			return entry
		}
		last = entry
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not reach health %s within %s (last seen %+v)", cell, want, fleetTimeout, last)
	return last
}

// postCapacity sends a report directly, so a specific token can be aimed at a
// specific cell.
func postCapacity(t *testing.T, hubAddr, cell, token string) (int, string) {
	t.Helper()

	body := `{"nodes":1,"cpuMilliAllocatable":1000,"cpuMilliCommitted":100,` +
		`"memoryBytesAllocatable":1073741824,"memoryBytesCommitted":104857600,` +
		`"pods":1,"podCapacity":110}`

	url := fmt.Sprintf("http://%s/api/v1/clusters/%s/capacity", hubAddr, cell)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting capacity: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	var out bytes.Buffer
	if _, err := out.ReadFrom(resp.Body); err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return resp.StatusCode, strings.TrimSpace(out.String())
}

func readToken(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

// runFleetCLI places through the real client binary, authenticating as the
// deploying ServiceAccount rather than as an agent.
func runFleetCLI(t *testing.T, hubAddr string, cell liveCell, args ...string) string {
	t.Helper()

	bin, err := filepath.Abs("../../bin/cellcast")
	if err != nil {
		t.Fatalf("resolving the client binary: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), bin, append([]string{"place", "--hub", "http://" + hubAddr}, args...)...)
	cmd.Env = append(os.Environ(), "CELLCAST_TOKEN="+readToken(t, cell.CallerToken))

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cellcast place: %v\n%s", err, out)
	}
	return string(out)
}

// kubectlCount returns how many objects a kubectl query returns.
func kubectlCount(t *testing.T, kubeconfig string, args ...string) int {
	t.Helper()

	args = append(args, "--no-headers")
	cmd := exec.CommandContext(t.Context(), "kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
	}

	var n int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

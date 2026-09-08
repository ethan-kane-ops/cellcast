package hub

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
)

// TestLiveFallback kills the hub in the middle of a pipeline and checks that
// each declared stance behaves the way it was declared (ENG-175, ADR-006).
//
// Run it with `just verify-e2e`. The hub runs in-process and is stopped by
// closing its listeners, which is what a killed hub looks like from the far end
// of a TCP connection: the client gets a refused connection, not a status code.
func TestLiveFallback(t *testing.T) {
	if os.Getenv("CELLCAST_LIVE") != "1" {
		t.Skip("set CELLCAST_LIVE=1 with a kubeconfig for a throwaway cluster")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	seedFleet(t, k8s, cfg.Host, caBundle(t, cfg))

	index := capacity.New(capacity.Options{})
	report(t, index, "prod-euw1", 0.20)
	report(t, index, "prod-euw2", 0.80)
	report(t, index, "dev-euw1", 0.01)

	addr := freeAddr(t)
	cacheDir := t.TempDir()
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	pipeline := func(args ...string) (string, error) {
		return placeCLI(t, addr, cacheDir, append([]string{
			"--workload", "checkout-api", "--kubeconfig", kubeconfig, "--json",
		}, args...)...)
	}

	stop := startHub(t, cfg, k8s, index, addr)

	t.Run("a fresh placement comes from the hub", func(t *testing.T) {
		out, err := pipeline()
		if err != nil {
			t.Fatalf("place = %v\n%s", err, out)
		}
		t.Logf("\n%s", out)

		got := decodeResult(t, out)
		if got["source"] != "hub" || got["cell"] != "prod-euw1" {
			t.Errorf("source %v cell %v, want hub/prod-euw1", got["source"], got["cell"])
		}
		if got["confidence"] != "high" {
			t.Errorf("confidence = %v, want high; every permitted cell reported", got["confidence"])
		}
		if _, err := os.Stat(kubeconfig); err != nil {
			t.Fatalf("no kubeconfig was written: %v", err)
		}
	})

	// While the hub is still up, because this is the case that must fail even
	// when a fallback is available and the hub is perfectly healthy.
	t.Run("an authorization refusal is never answered by a stance", func(t *testing.T) {
		out, err := pipeline("--dark", "--on-unavailable", "prod-euw2")
		if err == nil {
			t.Fatalf("a dark placement under a policy that forbids it was answered by --on-unavailable:\n%s", out)
		}
		t.Logf("\n%s", out)

		if !strings.Contains(out, "--on-unavailable") {
			t.Errorf("output does not say why the stance did not apply:\n%s", out)
		}
		if strings.Contains(out, `"cell": "prod-euw2"`) {
			t.Errorf("the declared cell was used to answer a refusal:\n%s", out)
		}
	})

	stop()

	t.Run("fail refuses to guess", func(t *testing.T) {
		out, err := pipeline()
		if err == nil {
			t.Fatalf("place = nil with a dead hub and the default stance:\n%s", out)
		}
		t.Logf("\n%s", out)

		if strings.Contains(out, "prod-euw1") {
			t.Errorf("a cell was named without a stance permitting it:\n%s", out)
		}
	})

	t.Run("last-known keeps the pipeline moving without a credential", func(t *testing.T) {
		out, err := pipeline("--on-unavailable", "last-known")
		if err != nil {
			t.Fatalf("place = %v, want the cached decision\n%s", err, out)
		}
		t.Logf("\n%s", out)

		got := decodeResult(t, out)
		if got["source"] != "cache" || got["cell"] != "prod-euw1" {
			t.Errorf("source %v cell %v, want cache/prod-euw1", got["source"], got["cell"])
		}
		if got["confidence"] != "stale" {
			t.Errorf("confidence = %v, want stale", got["confidence"])
		}
		if got["credential"] != nil {
			t.Error("a cached decision came with a credential")
		}
		// The credential written before the hub died is a live credential for
		// whichever cell that run chose. Left behind, the next deploy step
		// would pick it up and nobody would have chosen where it landed.
		if _, err := os.Stat(kubeconfig); !os.IsNotExist(err) {
			t.Errorf("stat(kubeconfig) = %v; the previous run's credential survived a fallback", err)
		}
	})

	t.Run("a pinned cell overrides what was cached", func(t *testing.T) {
		out, err := pipeline("--on-unavailable", "prod-euw2")
		if err != nil {
			t.Fatalf("place = %v, want the declared cell\n%s", err, out)
		}
		t.Logf("\n%s", out)

		got := decodeResult(t, out)
		if got["source"] != "pinned" || got["cell"] != "prod-euw2" {
			t.Errorf("source %v cell %v, want pinned/prod-euw2", got["source"], got["cell"])
		}
	})

	startHub(t, cfg, k8s, index, addr)

	// A client that kept serving from its cache after the outage ended would be
	// a slow-motion version of the outage.
	t.Run("the client returns to the hub once it is back", func(t *testing.T) {
		out, err := pipeline("--on-unavailable", "last-known")
		if err != nil {
			t.Fatalf("place = %v\n%s", err, out)
		}
		t.Logf("\n%s", out)

		got := decodeResult(t, out)
		if got["source"] != "hub" {
			t.Errorf("source = %v, want hub once the hub is answering again", got["source"])
		}
		if got["credential"] == nil {
			t.Error("the recovered placement carries no credential")
		}
		if _, err := os.Stat(kubeconfig); err != nil {
			t.Errorf("no kubeconfig was written after recovery: %v", err)
		}
	})
}

// startHub runs a hub on addr and returns a function that stops it. Stopping is
// idempotent and also runs on cleanup, so a test that fails partway through
// does not leave a listener behind for the next one.
func startHub(t *testing.T, cfg *rest.Config, k8s client.Client, index *capacity.Registry, addr string) func() {
	t.Helper()

	srv := liveServer(t, cfg, k8s, index, addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitForHub(t, srv)

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run() = %v, want nil on a clean shutdown", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// placeCLI runs the built client with its own decision cache, and returns the
// output whether the command succeeded or not: a refusal is a result here.
func placeCLI(t *testing.T, addr, cacheDir string, args ...string) (string, error) {
	t.Helper()

	bin, err := filepath.Abs("../../bin/cellcast")
	if err != nil {
		t.Fatalf("resolving the client binary: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s not built; run `just build` first", bin)
	}

	cmd := exec.CommandContext(t.Context(), bin, append([]string{"place", "--hub", "http://" + addr}, args...)...)
	cmd.Env = append(os.Environ(),
		"CELLCAST_TOKEN=stand-in-for-a-real-oidc-token",
		"CELLCAST_CACHE_DIR="+cacheDir,
	)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// decodeResult reads the JSON the client printed, ignoring anything before it
// so a warning on the same stream does not break the parse.
func decodeResult(t *testing.T, out string) map[string]any {
	t.Helper()

	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in the client output:\n%s", out)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(out[start:]), &body); err != nil {
		t.Fatalf("decoding the client output: %v\n%s", err, out)
	}
	return body
}

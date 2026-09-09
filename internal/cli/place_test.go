package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// theToken is a sentinel so a leak into any output is unmistakable.
const theToken = "SENTINEL-CLI-TOKEN-9d3e71"

// hubStub serves one canned placement response.
func hubStub(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer caller-token" {
			t.Errorf("Authorization = %q, want the caller's bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encoding stub response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func successBody(cell string) map[string]any {
	return map[string]any{
		"cell":     cell,
		"policy":   "app-prod",
		"strategy": "LeastLoaded",
		"credential": map[string]any{
			"server":                   "https://" + cell + ".example.test",
			"certificateAuthorityData": base64.StdEncoding.EncodeToString([]byte("ca-material")),
			"namespace":                "apps",
			"serviceAccount":           "deployer",
			"token":                    theToken,
			"expiresAt":                time.Now().Add(10 * time.Minute).Format(time.RFC3339),
		},
		"ttl":        map[string]any{"granted": "10m0s", "default": "10m0s", "max": "15m0s"},
		"confidence": "high",
		"decidedFor": "repo:acme/checkout:ref:refs/heads/main",
	}
}

// run executes `cellcast place` with args, returning combined output.
func run(t *testing.T, hub string, args ...string) (string, error) {
	t.Helper()

	t.Setenv(tokenEnv, "caller-token")
	if os.Getenv(cacheDirEnv) == "" {
		// Never the developer's own cache. A test that placed successfully
		// would otherwise leave an entry in it that a later run could read.
		t.Setenv(cacheDirEnv, t.TempDir())
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"place", "--hub", hub}, args...))

	err := cmd.Execute()
	return out.String(), err
}

// TestTokenNeverReachesOutput is the control T-05 names as the most likely
// real-world leak in the system: credential material in build output, readable
// by more people than the cluster is.
//
// The token has to reach the kubeconfig and nowhere else. Both halves are
// asserted, because a test that only checks stdout would pass just as well if
// the credential had never been written at all.
func TestTokenNeverReachesOutput(t *testing.T) {
	srv := hubStub(t, http.StatusOK, successBody("prod-euw1"))
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	out, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig, "--explain")
	if err != nil {
		t.Fatalf("place = %v, want nil", err)
	}

	if strings.Contains(out, theToken) {
		t.Errorf("command output contains the token:\n%s", out)
	}
	// Even a prefix is enough to correlate a leaked token back to a deploy.
	if strings.Contains(out, theToken[:10]) {
		t.Errorf("command output contains a token prefix:\n%s", out)
	}

	raw, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatalf("reading the written kubeconfig: %v", err)
	}
	if !strings.Contains(string(raw), theToken) {
		t.Error("the kubeconfig does not contain the token; the credential went nowhere")
	}
}

func TestKubeconfigIsOwnerOnly(t *testing.T) {
	srv := hubStub(t, http.StatusOK, successBody("prod-euw1"))
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	if _, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("place = %v, want nil", err)
	}

	info, err := os.Stat(kubeconfig)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != kubeconfigMode {
		t.Errorf("kubeconfig mode = %O, want %O", got, kubeconfigMode)
	}
}

// TestKubeconfigIsRewritable covers a rerun in the same workspace, which is the
// normal case on a CI runner with a cached checkout.
func TestKubeconfigIsRewritable(t *testing.T) {
	srv := hubStub(t, http.StatusOK, successBody("prod-euw1"))
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	for i := range 2 {
		if _, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig); err != nil {
			t.Fatalf("place run %d = %v, want nil", i+1, err)
		}
	}
}

// TestJSONOutputCarriesNoToken covers the mode most likely to be piped into
// something that logs it.
func TestJSONOutputCarriesNoToken(t *testing.T) {
	srv := hubStub(t, http.StatusOK, successBody("prod-euw1"))
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	out, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig, "--json")
	if err != nil {
		t.Fatalf("place = %v, want nil", err)
	}
	if strings.Contains(out, theToken) {
		t.Errorf("json output contains the token:\n%s", out)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got["cell"] != "prod-euw1" {
		t.Errorf("cell = %v, want prod-euw1", got["cell"])
	}
	if got["kubeconfig"] != kubeconfig {
		t.Errorf("kubeconfig = %v, want %s", got["kubeconfig"], kubeconfig)
	}
}

func TestDryRunWritesNoFile(t *testing.T) {
	body := successBody("prod-euw1")
	delete(body, "credential")
	body["dryRun"] = true

	srv := hubStub(t, http.StatusOK, body)
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	out, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig, "--dry-run")
	if err != nil {
		t.Fatalf("place = %v, want nil", err)
	}
	if _, err := os.Stat(kubeconfig); !os.IsNotExist(err) {
		t.Errorf("dry run wrote %s", kubeconfig)
	}
	if !strings.Contains(out, "would place") {
		t.Errorf("output = %q, want it to say the placement was hypothetical", out)
	}
}

// TestThereIsNoTokenFlag pins an omission.
//
// A token in a flag is a token in the process table, readable by every other
// user on a shared runner. Somebody will eventually try to add this for
// convenience, and the test is here to say no.
func TestThereIsNoTokenFlag(t *testing.T) {
	cmd := NewRootCmd()
	for _, c := range cmd.Commands() {
		if c.Name() != "place" {
			continue
		}
		if f := c.Flags().Lookup("token"); f != nil {
			t.Error("place has a --token flag; a token passed as an argument is world-readable in the process table")
		}
		if f := c.Flags().Lookup("token-file"); f == nil {
			t.Error("place has no --token-file flag")
		}
	}
}

func TestClampedTTLIsReported(t *testing.T) {
	body := successBody("prod-euw1")
	body["ttl"] = map[string]any{
		"granted": "15m0s", "default": "10m0s", "max": "15m0s",
		"requested": "1h0m0s", "clamped": true,
	}

	srv := hubStub(t, http.StatusOK, body)
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	out, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig, "--ttl", "1h")
	if err != nil {
		t.Fatalf("place = %v, want nil", err)
	}
	// A deploy that assumes it has an hour will fail partway through rather
	// than at the start, so this cannot be silent.
	for _, want := range []string{"15m", "1h", "capped"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to mention %q", out, want)
		}
	}
}

func TestRefusalsSurfaceTheReason(t *testing.T) {
	srv := hubStub(t, http.StatusForbidden, map[string]string{
		"reason": "NoPolicy",
		"error":  "no placement policy permits this caller",
	})

	_, err := run(t, srv.URL, "--workload", "checkout-api")
	if err == nil {
		t.Fatal("place = nil, want a refusal")
	}

	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("error = %T, want *Refusal", err)
	}
	if ref.Reason != refusal.NoPolicy {
		t.Errorf("reason = %q, want NoPolicy", ref.Reason)
	}
	if ref.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", ref.Status)
	}
}

func TestTokenSources(t *testing.T) {
	t.Run("the environment supplies it", func(t *testing.T) {
		t.Setenv(tokenEnv, "from-env")
		c, err := NewClient("https://hub.test", "", time.Second)
		if err != nil {
			t.Fatalf("NewClient() = %v, want nil", err)
		}
		if c.token != "from-env" {
			t.Errorf("token = %q, want from-env", c.token)
		}
	})

	t.Run("a file wins over the environment", func(t *testing.T) {
		t.Setenv(tokenEnv, "from-env")
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
			t.Fatalf("writing token file: %v", err)
		}

		c, err := NewClient("https://hub.test", path, time.Second)
		if err != nil {
			t.Fatalf("NewClient() = %v, want nil", err)
		}
		// Trailing newline stripped: a token file written by `echo` has one,
		// and a bearer header with a newline in it is rejected outright.
		if c.token != "from-file" {
			t.Errorf("token = %q, want from-file", c.token)
		}
	})

	t.Run("neither is an error", func(t *testing.T) {
		t.Setenv(tokenEnv, "")
		if _, err := NewClient("https://hub.test", "", time.Second); err == nil {
			t.Error("NewClient() = nil, want an error when no token is available")
		}
	})

	t.Run("an unreadable token file names the path, not the contents", func(t *testing.T) {
		t.Setenv(tokenEnv, "")
		_, err := NewClient("https://hub.test", "/nonexistent/token", time.Second)
		if err == nil {
			t.Fatal("NewClient() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "/nonexistent/token") {
			t.Errorf("error = %q, want it to name the path", err)
		}
	})
}

// mutableHub serves whatever the test sets, so one server can answer normally,
// then refuse, then be closed underneath the client.
type mutableHub struct {
	*httptest.Server
	status int
	body   any
}

func newMutableHub(t *testing.T) *mutableHub {
	t.Helper()
	h := &mutableHub{status: http.StatusOK, body: successBody("prod-euw1")}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.status)
		if err := json.NewEncoder(w).Encode(h.body); err != nil {
			t.Errorf("encoding stub response: %v", err)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *mutableHub) refuse(status int, reason refusal.Reason, msg string) {
	h.status, h.body = status, map[string]string{"reason": string(reason), "error": msg}
}

// warmCache points the client at a private cache directory and places once
// against a live hub, so a later call has something to fall back to.
func warmCache(t *testing.T, hub string, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(cacheDirEnv, dir)

	if _, err := run(t, hub, append([]string{"--workload", "checkout-api", "--dry-run"}, args...)...); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}
	return dir
}

func TestSuccessfulPlacementIsCached(t *testing.T) {
	h := newMutableHub(t)
	dir := warmCache(t, h.URL)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the cache directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the cache holds %d entries, want 1", len(entries))
	}
}

// TestFallbackFailIsTheDefault pins the stance a caller gets without asking.
// Anything else would be an implicit fallback, which is how a deploy silently
// lands in the wrong cluster.
func TestFallbackFailIsTheDefault(t *testing.T) {
	h := newMutableHub(t)
	warmCache(t, h.URL)
	h.Close()

	out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run")
	if err == nil {
		t.Fatalf("place = nil with a dead hub and no declared stance:\n%s", out)
	}
	if strings.Contains(out, "prod-euw1") {
		t.Errorf("a cell was named without a stance permitting it:\n%s", out)
	}
}

func TestFallbackLastKnown(t *testing.T) {
	h := newMutableHub(t)
	warmCache(t, h.URL)
	h.Close()

	out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--on-unavailable", "last-known")
	if err != nil {
		t.Fatalf("place = %v, want the pipeline to keep moving:\n%s", err, out)
	}

	for _, want := range []string{"prod-euw1", "last known", "no credential was minted"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to mention %q", out, want)
		}
	}
	// Whose decision was inherited is the first thing anyone asks.
	if !strings.Contains(out, "repo:acme/checkout") {
		t.Errorf("output does not say who the cached decision was made for:\n%s", out)
	}
}

func TestFallbackPinnedCell(t *testing.T) {
	h := newMutableHub(t)
	warmCache(t, h.URL)
	h.Close()

	// The cache holds prod-euw1. A pinned cell is a declaration, so it wins.
	out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--on-unavailable", "prod-euw3")
	if err != nil {
		t.Fatalf("place = %v, want the declared cell:\n%s", err, out)
	}
	if !strings.Contains(out, "prod-euw3") || strings.Contains(out, "prod-euw1") {
		t.Errorf("output = %q, want the declared cell rather than the cached one", out)
	}
}

func TestFallbackLastKnownWithNothingCached(t *testing.T) {
	h := newMutableHub(t)
	h.Close()
	t.Setenv(cacheDirEnv, t.TempDir())

	out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--on-unavailable", "last-known")
	if err == nil {
		t.Fatalf("place = nil with an empty cache:\n%s", out)
	}
	// Both halves. Either alone sends the reader to the wrong place.
	for _, want := range []string{"hub", "checkout-api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestAuthorizationRefusalIsNeverAnsweredFromAFallback is the control this
// whole feature turns on. If a stance could answer a refusal, the placement
// policy would be advice and `--on-unavailable` would be the way around it.
func TestAuthorizationRefusalIsNeverAnsweredFromAFallback(t *testing.T) {
	refusals := []struct {
		name   string
		status int
		reason refusal.Reason
	}{
		{"no policy matches", http.StatusForbidden, refusal.NoPolicy},
		{"dark targeting refused", http.StatusForbidden, refusal.DarkNotPermitted},
		{"the policy permits no cell", http.StatusConflict, refusal.NoPermittedCells},
		{"minting failed", http.StatusServiceUnavailable, refusal.MintFailed},
		{"the broker is gone", http.StatusServiceUnavailable, refusal.MintUnavailable},
	}
	stances := []string{"last-known", "prod-euw3"}

	for _, r := range refusals {
		for _, stance := range stances {
			t.Run(r.name+" with "+stance, func(t *testing.T) {
				h := newMutableHub(t)
				warmCache(t, h.URL)
				h.refuse(r.status, r.reason, "refused")

				out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--on-unavailable", stance)
				if err == nil {
					t.Fatalf("%s was answered by --on-unavailable=%s:\n%s", r.reason, stance, out)
				}
				if !strings.Contains(err.Error(), "--on-unavailable") {
					t.Errorf("error = %q, want it to say why the stance did not apply", err)
				}
			})
		}
	}
}

// TestOptimisationFailureIsAnsweredFromTheCache is the other half. Failing
// closed on everything would make the feature pointless.
func TestOptimisationFailureIsAnsweredFromTheCache(t *testing.T) {
	for _, reason := range []refusal.Reason{refusal.CapacityUnknown, refusal.NoEligibleCells, refusal.PlacementUnavailable} {
		t.Run(string(reason), func(t *testing.T) {
			h := newMutableHub(t)
			warmCache(t, h.URL)
			h.refuse(http.StatusServiceUnavailable, reason, "the hub cannot choose")

			out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--on-unavailable", "last-known")
			if err != nil {
				t.Fatalf("place = %v, want the cached decision:\n%s", err, out)
			}
			if !strings.Contains(out, "prod-euw1") {
				t.Errorf("output = %q, want the cached cell", out)
			}
		})
	}
}

// TestFallbackRemovesAStaleKubeconfig is the wrong-cluster hazard. An earlier
// run's credential left next to a fallback decision would be picked up by the
// next step and the deploy would land wherever that run chose.
func TestFallbackRemovesAStaleKubeconfig(t *testing.T) {
	h := newMutableHub(t)
	t.Setenv(cacheDirEnv, t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

	if _, err := run(t, h.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("the first placement failed: %v", err)
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		t.Fatalf("the first placement wrote no kubeconfig: %v", err)
	}

	h.Close()
	out, err := run(t, h.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig, "--on-unavailable", "last-known")
	if err != nil {
		t.Fatalf("place = %v, want the cached decision:\n%s", err, out)
	}

	if _, err := os.Stat(kubeconfig); !os.IsNotExist(err) {
		t.Errorf("stat(%s) = %v; a fallback left the previous run's credential in place", kubeconfig, err)
	}
}

// TestFallbackDoesNotRefreshTheCache keeps an entry from renewing its own
// lifetime. Written on every fallback, a decision would survive an outage of
// any length without the hub ever confirming it again.
func TestFallbackDoesNotRefreshTheCache(t *testing.T) {
	h := newMutableHub(t)
	dir := warmCache(t, h.URL)
	h.Close()

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%s) = %v, %v; want one cache entry", dir, entries, err)
	}
	path := filepath.Join(dir, entries[0].Name())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the entry: %v", err)
	}

	if _, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--on-unavailable", "last-known"); err != nil {
		t.Fatalf("place = %v, want the cached decision", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the entry after a fallback: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a fallback rewrote the cache entry:\nbefore %s\nafter  %s", before, after)
	}
}

// TestJSONResultNamesItsSource is what a pipeline branches on. Without it the
// only difference between a fresh decision and a replayed one is the absence of
// a credential, which is found at the kubectl call rather than here.
func TestJSONResultNamesItsSource(t *testing.T) {
	h := newMutableHub(t)
	warmCache(t, h.URL)

	decode := func(t *testing.T, out string) map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatalf("decoding %q: %v", out, err)
		}
		return body
	}

	out, err := run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("place = %v", err)
	}
	fresh := decode(t, out)
	if fresh["source"] != SourceHub || fresh["confidence"] != "high" {
		t.Errorf("a fresh decision reports source %v confidence %v, want hub/high", fresh["source"], fresh["confidence"])
	}
	if _, ok := fresh["unavailable"]; ok {
		t.Error("a fresh decision carries an unavailable field")
	}

	h.Close()
	out, err = run(t, h.URL, "--workload", "checkout-api", "--dry-run", "--json", "--on-unavailable", "last-known")
	if err != nil {
		t.Fatalf("place = %v", err)
	}
	cached := decode(t, out)
	if cached["source"] != SourceCache || cached["confidence"] != ConfidenceStale {
		t.Errorf("a cached decision reports source %v confidence %v, want cache/stale", cached["source"], cached["confidence"])
	}
	if cached["unavailable"] == nil {
		t.Error("a cached decision does not say why the hub was not used")
	}
	if cached["credential"] != nil {
		t.Error("a cached decision carries a credential")
	}
}

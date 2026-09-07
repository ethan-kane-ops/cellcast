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
		"ttl": map[string]any{"granted": "10m0s", "default": "10m0s", "max": "15m0s"},
	}
}

// run executes `cellcast place` with args, returning combined output.
func run(t *testing.T, hub string, args ...string) (string, error) {
	t.Helper()

	t.Setenv(tokenEnv, "caller-token")

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

// TestThereIsNoTokenFlag pins a deliberate omission.
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

	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T, want *Refusal", err)
	}
	if refusal.Reason != "NoPolicy" {
		t.Errorf("reason = %q, want NoPolicy", refusal.Reason)
	}
	if refusal.Retryable() {
		t.Error("an authorization refusal is reported as retryable")
	}
}

func TestRetryableRefusal(t *testing.T) {
	srv := hubStub(t, http.StatusServiceUnavailable, map[string]string{
		"reason": "CapacityUnknown",
		"error":  "no permitted cell has usable capacity",
	})

	_, err := run(t, srv.URL, "--workload", "checkout-api")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v, want *Refusal", err)
	}
	if !refusal.Retryable() {
		t.Error("a capacity blackout is reported as permanent")
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

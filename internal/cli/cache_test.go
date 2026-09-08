package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHub = "https://hub.example.test"

func testCache(t *testing.T, ttl time.Duration) *decisionCache {
	t.Helper()
	// A directory the cache has to create itself, which is the real case: the
	// mode it gets is the mode MkdirAll gives it.
	c := newDecisionCache(testHub, filepath.Join(t.TempDir(), "decisions"), ttl)
	if !c.enabled() {
		t.Fatal("newDecisionCache() produced a disabled cache")
	}
	return c
}

func seed(t *testing.T, c *decisionCache, workload, cell string) {
	t.Helper()
	err := c.save(&cachedDecision{
		Workload: workload, Cell: cell,
		Policy: "app-prod", Strategy: "LeastLoaded",
		DecidedFor: "repo:acme/checkout:ref:refs/heads/main",
	})
	if err != nil {
		t.Fatalf("save() = %v, want nil", err)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := testCache(t, time.Hour)
	seed(t, c, "checkout-api", "prod-euw1")

	got, err := c.load("checkout-api", false)
	if err != nil {
		t.Fatalf("load() = %v, want a hit", err)
	}
	if got.Cell != "prod-euw1" || got.Policy != "app-prod" || got.Strategy != "LeastLoaded" {
		t.Errorf("load() = %+v, want the decision that was saved", got)
	}
	if got.DecidedFor == "" {
		t.Error("the cached decision does not record who it was decided for")
	}
}

// TestCacheHoldsNoCredential is the ADR-006 invariant in the one place it could
// be broken by accident: the type that reaches disk.
func TestCacheHoldsNoCredential(t *testing.T) {
	c := testCache(t, time.Hour)
	seed(t, c, "checkout-api", "prod-euw1")

	raw, err := os.ReadFile(c.path("checkout-api", false))
	if err != nil {
		t.Fatalf("reading the entry: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decoding the entry: %v", err)
	}
	for _, forbidden := range []string{"token", "credential", "certificateAuthorityData", "serviceAccount"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("the cache entry carries %q; a cached placement is a decision, never a credential", forbidden)
		}
	}
}

func TestCacheEntryIsOwnerOnly(t *testing.T) {
	c := testCache(t, time.Hour)
	seed(t, c, "checkout-api", "prod-euw1")

	info, err := os.Stat(c.path("checkout-api", false))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != cacheFileMode {
		t.Errorf("entry mode = %O, want %O", got, cacheFileMode)
	}

	dir, err := os.Stat(c.dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != cacheDirMode {
		t.Errorf("cache directory mode = %O, want %O", got, cacheDirMode)
	}
}

func TestCacheExpiry(t *testing.T) {
	c := testCache(t, 30*time.Minute)
	seed(t, c, "checkout-api", "prod-euw1")

	c.now = func() time.Time { return time.Now().Add(29 * time.Minute) }
	if _, err := c.load("checkout-api", false); err != nil {
		t.Fatalf("load() inside the TTL = %v, want a hit", err)
	}

	c.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	_, err := c.load("checkout-api", false)
	if err == nil {
		t.Fatal("load() past the TTL returned a hit")
	}
	// The age has to be in the message. "no cached decision" sends an operator
	// looking for a cache that is right there and merely too old.
	if !strings.Contains(err.Error(), "old") {
		t.Errorf("expiry error = %q, want it to say how old the entry is", err)
	}
}

// TestCacheSeparatesRequests pins the key. Two different questions must not
// share an answer: a decision for one workload is not a decision for another,
// and a dark placement is a different question from an ordinary one.
func TestCacheSeparatesRequests(t *testing.T) {
	c := testCache(t, time.Hour)
	seed(t, c, "checkout-api", "prod-euw1")

	if _, err := c.load("payments-api", false); err == nil {
		t.Error("a decision for checkout-api was served for payments-api")
	}
	if _, err := c.load("checkout-api", true); err == nil {
		t.Error("an ordinary decision was served for a dark placement")
	}

	other := newDecisionCache("https://other-hub.example.test", c.dir, time.Hour)
	if _, err := other.load("checkout-api", false); err == nil {
		t.Error("a decision from one hub was served for another")
	}
}

// TestCacheEntryMustDescribeTheRequest guards the read side of the key. The
// filename is a digest, so the only thing standing between a mismatched entry
// and a deploy is this check.
func TestCacheEntryMustDescribeTheRequest(t *testing.T) {
	c := testCache(t, time.Hour)
	seed(t, c, "checkout-api", "prod-euw1")

	path := c.path("checkout-api", false)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the entry: %v", err)
	}

	var d cachedDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decoding the entry: %v", err)
	}
	d.Workload = "something-else"
	rewritten, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("encoding the entry: %v", err)
	}
	if err := os.WriteFile(path, rewritten, cacheFileMode); err != nil {
		t.Fatalf("rewriting the entry: %v", err)
	}

	if _, err := c.load("checkout-api", false); err == nil {
		t.Error("an entry describing a different request was served")
	}
}

// TestCorruptCacheEntryIsAMiss keeps the cache from becoming a way to fail a
// placement. Anything unreadable is simply not there.
func TestCorruptCacheEntryIsAMiss(t *testing.T) {
	c := testCache(t, time.Hour)
	seed(t, c, "checkout-api", "prod-euw1")

	if err := os.WriteFile(c.path("checkout-api", false), []byte("{not json"), cacheFileMode); err != nil {
		t.Fatalf("corrupting the entry: %v", err)
	}
	if _, err := c.load("checkout-api", false); err == nil {
		t.Error("an unreadable entry was served as a decision")
	}
}

func TestCacheDisabled(t *testing.T) {
	c := newDecisionCache(testHub, t.TempDir(), 0)
	if c.enabled() {
		t.Fatal("a zero TTL left the cache enabled")
	}

	// A disabled cache is silent rather than failing: `--cache-ttl=0` is a
	// caller opting out, not an error to report on every run.
	if err := c.save(&cachedDecision{Workload: "checkout-api", Cell: "prod-euw1"}); err != nil {
		t.Errorf("save() on a disabled cache = %v, want nil", err)
	}
	if entries, err := os.ReadDir(c.dir); err == nil && len(entries) > 0 {
		t.Errorf("a disabled cache wrote %d entries", len(entries))
	}
	if _, err := c.load("checkout-api", false); err == nil {
		t.Error("a disabled cache served a decision")
	}
}

// TestCacheDirResolution covers where entries land when nobody said.
func TestCacheDirResolution(t *testing.T) {
	t.Run("the flag wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(cacheDirEnv, filepath.Join(t.TempDir(), "from-env"))
		if got := newDecisionCache(testHub, dir, time.Hour).dir; got != dir {
			t.Errorf("dir = %q, want the flag value %q", got, dir)
		}
	})

	t.Run("the environment is next", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(cacheDirEnv, dir)
		if got := newDecisionCache(testHub, "", time.Hour).dir; got != dir {
			t.Errorf("dir = %q, want the environment value %q", got, dir)
		}
	})

	t.Run("otherwise the user cache directory", func(t *testing.T) {
		t.Setenv(cacheDirEnv, "")
		base, err := os.UserCacheDir()
		if err != nil {
			t.Skip("no user cache directory on this machine")
		}
		want := filepath.Join(base, "cellcast", "decisions")
		if got := newDecisionCache(testHub, "", time.Hour).dir; got != want {
			t.Errorf("dir = %q, want %q", got, want)
		}
	})
}

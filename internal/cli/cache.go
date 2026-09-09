package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultCacheTTL is how long a cached decision may still be used.
//
// An hour covers the outages a fallback is for and expires before the things a
// decision depends on plausibly change. A policy edit or a cell being drained
// is not visible to a client holding a cached answer, so the TTL is the only
// thing bounding how wrong that answer can get.
const DefaultCacheTTL = time.Hour

// cacheDirEnv overrides where decisions are cached.
const cacheDirEnv = "CELLCAST_CACHE_DIR"

// Permissions for the cache. Owner only, like everything else this client
// writes: the entries name the cells a caller is permitted to reach, which is
// not something to hand the next job on a shared runner.
const (
	cacheDirMode  os.FileMode = 0o700
	cacheFileMode os.FileMode = 0o600
)

// cachedDecision is one remembered placement.
//
// It holds a decision and never a credential (ADR-006). The client re-mints or
// fails, so there is nothing here worth stealing and nothing that can be
// replayed as access.
type cachedDecision struct {
	Hub          string `json:"hub"`
	Workload     string `json:"workload"`
	TargetedDark bool   `json:"targetedDark,omitempty"`

	Cell     string `json:"cell"`
	Policy   string `json:"policy"`
	Strategy string `json:"strategy"`
	// DecidedFor is the subject the hub resolved the caller to when it made
	// this decision. Recorded and displayed rather than enforced: with the hub
	// down the client cannot establish its own subject, so this is here to be
	// read by a human wondering whose decision they just inherited.
	DecidedFor string `json:"decidedFor,omitempty"`
	// CachedAt is on the client's clock, because the TTL is measured against
	// the client's clock. Using the hub's would make the lifetime of an entry
	// depend on the skew between two machines.
	CachedAt time.Time `json:"cachedAt"`
}

// age is how long ago the hub made this decision, on the client's clock.
func (d *cachedDecision) age(now time.Time) time.Duration {
	return now.Sub(d.CachedAt).Round(time.Second)
}

// decisionCache is the client's last-known-placement store for one hub.
//
// Scoped to a hub rather than global: two hubs are two different fleets under
// two different sets of policy, and a decision from one says nothing about the
// other.
type decisionCache struct {
	hub string
	dir string
	ttl time.Duration
	// now is injectable so expiry is tested rather than waited for.
	now func() time.Time
}

// newDecisionCache resolves where decisions are cached and for how long.
//
// A cache that cannot be located is disabled, never fatal. Refusing to place a
// workload because the cache directory is unusable would turn a convenience
// into a second single point of failure, which is the opposite of the point.
func newDecisionCache(hub, dir string, ttl time.Duration) *decisionCache {
	c := &decisionCache{hub: hub, ttl: ttl, now: time.Now}
	if ttl <= 0 {
		return c
	}

	switch {
	case dir != "":
		c.dir = dir
	case os.Getenv(cacheDirEnv) != "":
		c.dir = os.Getenv(cacheDirEnv)
	default:
		base, err := os.UserCacheDir()
		if err != nil {
			return c
		}
		c.dir = filepath.Join(base, "cellcast", "decisions")
	}
	return c
}

// enabled reports whether the cache can be read or written.
func (c *decisionCache) enabled() bool { return c.dir != "" && c.ttl > 0 }

// path is where one workload's decision lives.
//
// The name is a digest rather than the workload, because a workload name is
// caller-supplied and a caller-supplied path component is a traversal waiting
// to happen. The plaintext goes inside the file, where load checks it.
func (c *decisionCache) path(workload string, dark bool) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%t", c.hub, workload, dark))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".json")
}

// load returns the cached decision for a workload, or an error saying why there
// is none. Every failure to produce one is a miss rather than a fault: a cache
// is a best effort by definition, and one that could fail a placement would be
// worse than no cache at all.
func (c *decisionCache) load(workload string, dark bool) (*cachedDecision, error) {
	if !c.enabled() {
		return nil, fmt.Errorf("the decision cache is disabled")
	}

	raw, err := os.ReadFile(c.path(workload, dark))
	if err != nil {
		return nil, fmt.Errorf("no decision has been cached for %s on this hub", workload)
	}

	var d cachedDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("the cached decision for %s is unreadable", workload)
	}
	// The digest could only collide by accident, but an entry that does not
	// describe what was asked for is not an entry to deploy from.
	if d.Hub != c.hub || d.Workload != workload || d.TargetedDark != dark || d.Cell == "" {
		return nil, fmt.Errorf("the cached decision for %s does not match this request", workload)
	}

	if age := d.age(c.now()); age > c.ttl {
		return nil, fmt.Errorf("the decision cached for %s is %s old, past the %s cache lifetime",
			workload, age, c.ttl)
	}
	return &d, nil
}

// save records a decision the hub made.
//
// Only ever called with a fresh response. A fallback must never write here, or
// the cache would keep pushing its own expiry forward and a decision could
// outlive its TTL indefinitely without the hub ever confirming it again.
func (c *decisionCache) save(d *cachedDecision) error {
	if !c.enabled() {
		return nil
	}
	if err := os.MkdirAll(c.dir, cacheDirMode); err != nil {
		return fmt.Errorf("creating the cache directory: %w", err)
	}

	d.Hub, d.CachedAt = c.hub, c.now()
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encoding the decision: %w", err)
	}

	// Written to a temporary file and renamed, so a run interrupted mid-write
	// leaves the previous entry intact rather than a truncated one that every
	// later read treats as a miss.
	tmp, err := os.CreateTemp(c.dir, "decision-*")
	if err != nil {
		return fmt.Errorf("creating the cache entry: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing the cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the cache entry: %w", err)
	}
	if err := os.Chmod(tmp.Name(), cacheFileMode); err != nil {
		return fmt.Errorf("securing the cache entry: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.path(d.Workload, d.TargetedDark)); err != nil {
		return fmt.Errorf("storing the cache entry: %w", err)
	}
	return nil
}

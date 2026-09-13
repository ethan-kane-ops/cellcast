package placement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// Bounds on what the hub remembers about where workloads were placed.
const (
	// DefaultExpireAfter is how long a record survives without a placement.
	// Long enough that a service deployed once a quarter is still where it was:
	// it bounds storage, and is not a schedule for moving anything.
	DefaultExpireAfter = 90 * 24 * time.Hour

	// MinExpireAfter is the shortest expiry the hub accepts. A record is
	// refreshed at most hourly, so an expiry close to that forgets workloads
	// that are still deploying.
	MinExpireAfter = 24 * time.Hour

	// DefaultMaxPerPolicy bounds the records one policy can accumulate.
	DefaultMaxPerPolicy = 10000

	// refreshAfter is how old a record may get before a placement landing in
	// the same cell rewrites it. Without it every deploy would be an API server
	// write; with it a busy workload costs one an hour.
	refreshAfter = time.Hour

	// writeTimeout bounds the one write a placement waits on.
	writeTimeout = 2 * time.Second

	// sweepEvery is how often expired records are deleted.
	sweepEvery = time.Hour
)

// PolicyIndex is the cache index records are counted by, one policy at a time.
const PolicyIndex = "spec.policy"

// ErrMemoryFull means a policy already holds as many records as the hub will
// keep for it. New workloads under it are placed and not remembered; the ones
// already remembered are untouched.
var ErrMemoryFull = errors.New("policy holds the most workloads the hub will remember for it")

// MemoryOptions bounds a Memory.
type MemoryOptions struct {
	// ExpireAfter is how long a record survives without a placement.
	ExpireAfter time.Duration
	// MaxPerPolicy is how many workloads one policy may have remembered.
	MaxPerPolicy int
}

// Memory keeps the cell each workload was last placed in, as WorkloadPlacement
// objects in the hub's namespace.
//
// Capacity is held in process memory because it is written constantly and
// worthless once stale (ADR-002). This is the other way round: written rarely,
// and worthless if it does not survive the rolling update of the hub that
// happens in the middle of somebody's deploy. So it lives where everything else
// durable here lives, in the API server, and every replica reads it from the
// same informer cache it reads policy from (docs/architecture.md ADR-012).
type Memory struct {
	client    client.Client
	namespace string
	opts      MemoryOptions
	log       *slog.Logger
	now       func() time.Time
}

// NewMemory builds a Memory over c, which is expected to be the manager's
// cached client with PolicyIndex registered. SetupWithManager registers it.
func NewMemory(c client.Client, namespace string, opts MemoryOptions, log *slog.Logger) *Memory {
	return &Memory{client: c, namespace: namespace, opts: opts, log: log, now: time.Now}
}

// SetupWithManager registers the index remember counts by and the sweep that
// deletes expired records.
//
// Indexing starts the informer with the cache, so a replica is not ready until
// it can read what earlier replicas remembered.
func (m *Memory) SetupWithManager(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &cellcastv1alpha1.WorkloadPlacement{}, PolicyIndex, IndexPolicy); err != nil {
		return fmt.Errorf("indexing workload placements by policy: %w", err)
	}
	if err := mgr.Add(m); err != nil {
		return fmt.Errorf("registering the workload placement sweep: %w", err)
	}
	return nil
}

// IndexPolicy is the value PolicyIndex holds for a record.
func IndexPolicy(o client.Object) []string {
	wp, ok := o.(*cellcastv1alpha1.WorkloadPlacement)
	if !ok {
		return nil
	}
	return []string{wp.Spec.Policy}
}

// memo is what a decision carries so that remember can write it back.
type memo struct {
	workload  string
	policyUID types.UID
	// record is the object as the decision read it, nil when there was none.
	// An expired one is kept, so the write updates it rather than failing to
	// create it.
	record *cellcastv1alpha1.WorkloadPlacement
}

// recall returns the record for a workload under a policy, or nil.
//
// Read from the informer cache, so deciding still does not write. A record
// whose spec names another policy or workload is not this workload's, whatever
// its name, and reads as none.
func (m *Memory) recall(ctx context.Context, policy, workload string) *cellcastv1alpha1.WorkloadPlacement {
	var wp cellcastv1alpha1.WorkloadPlacement
	key := client.ObjectKey{Namespace: m.namespace, Name: recordName(policy, workload)}
	if err := m.client.Get(ctx, key, &wp); err != nil {
		if !apierrors.IsNotFound(err) {
			// Placing afresh is the answer to not knowing, as it was before
			// anything was remembered.
			m.log.WarnContext(ctx, "reading where a workload was last placed failed; placing it afresh",
				slog.String("policy", policy),
				slog.String("workload", workload),
				slog.Any("error", err),
			)
		}
		return nil
	}
	if wp.Spec.Policy != policy || wp.Spec.Workload != workload {
		return nil
	}
	return &wp
}

// live reports whether a record is inside its expiry. An expired record reads
// as absent whether or not the sweep has deleted it yet, so a stalled sweep
// cannot keep a workload somewhere past its expiry.
func (m *Memory) live(wp *cellcastv1alpha1.WorkloadPlacement) bool {
	return m.now().Sub(wp.Spec.LastPlacedAt.Time) < m.opts.ExpireAfter
}

// sanitizeForLog removes line breaks so caller-controlled values cannot forge
// additional log lines when included in error messages.
func sanitizeForLog(v string) string {
	v = strings.ReplaceAll(v, "\n", "")
	return strings.ReplaceAll(v, "\r", "")
}

// remember writes back the cell a decision chose, when anything changed.
func (m *Memory) remember(ctx context.Context, d *Decision) error {
	if m == nil || d.memo == nil {
		return nil
	}

	// Detached from the request: the credential has been issued, and a caller
	// hanging up does not change where it is about to deploy.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	ctx, span := startSpan(ctx, "remember")
	defer span.End()

	now := m.now()
	rec := d.memo.record
	if rec == nil {
		return m.create(ctx, d, now)
	}
	if rec.Spec.Cell == d.Cell && now.Sub(rec.Spec.LastPlacedAt.Time) < refreshAfter {
		return nil
	}

	updated := rec.DeepCopy()
	updated.Spec.Cell = d.Cell
	updated.Spec.LastPlacedAt = metav1.NewTime(now)
	err := m.client.Update(ctx, updated)
	switch {
	case apierrors.IsNotFound(err):
		// Swept, or deleted by an operator, since the decision read it.
		return m.create(ctx, d, now)
	case apierrors.IsConflict(err):
		// Another replica wrote it after this one read it. Both wrote a cell
		// that had just passed the filter, so neither answer is wrong.
		return nil
	case err != nil:
		return fmt.Errorf("updating where %s was last placed: %w", sanitizeForLog(d.memo.workload), err)
	}
	return nil
}

// create writes a workload's first record under a policy.
func (m *Memory) create(ctx context.Context, d *Decision, now time.Time) error {
	// Counted from the cache before the write, so replicas racing for the last
	// places can overshoot by one each. The cap bounds what callers can make the
	// hub store, and nobody is billed against it.
	var held cellcastv1alpha1.WorkloadPlacementList
	if err := m.client.List(ctx, &held,
		client.InNamespace(m.namespace),
		client.MatchingFields{PolicyIndex: d.Policy},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return fmt.Errorf("counting the workloads remembered under %s: %w", d.Policy, err)
	}
	if len(held.Items) >= m.opts.MaxPerPolicy {
		return fmt.Errorf("%w: %s holds %d", ErrMemoryFull, d.Policy, len(held.Items))
	}

	wp := &cellcastv1alpha1.WorkloadPlacement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      recordName(d.Policy, d.memo.workload),
			Namespace: m.namespace,
			// Deleting the policy deletes what was remembered under it. A
			// recreated policy of the same name starts with nothing, which is
			// right: it is not the same authorisation any more.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: cellcastv1alpha1.GroupVersion.String(),
				Kind:       "PlacementPolicy",
				Name:       d.Policy,
				UID:        d.memo.policyUID,
			}},
		},
		Spec: cellcastv1alpha1.WorkloadPlacementSpec{
			Policy:       d.Policy,
			Workload:     d.memo.workload,
			Cell:         d.Cell,
			LastPlacedAt: metav1.NewTime(now),
		},
	}
	// AlreadyExists is another replica placing the same new workload in the same
	// moment, and its record is as good as this one.
	if err := m.client.Create(ctx, wp); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("recording where %s was placed: %w", d.memo.workload, err)
	}
	return nil
}

// Start deletes expired records, then again every hour until ctx ends.
//
// An expired record already reads as absent, so the sweep bounds storage and
// changes no decision.
func (m *Memory) Start(ctx context.Context) error {
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	for {
		m.sweep(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// NeedLeaderElection runs the sweep on the leader alone. Two sweeps would
// delete the same objects harmlessly; one is quieter.
func (m *Memory) NeedLeaderElection() bool { return true }

// sweep deletes every expired record.
func (m *Memory) sweep(ctx context.Context) {
	var all cellcastv1alpha1.WorkloadPlacementList
	if err := m.client.List(ctx, &all, client.InNamespace(m.namespace)); err != nil {
		m.log.WarnContext(ctx, "listing workload placements to expire failed", slog.Any("error", err))
		return
	}
	for i := range all.Items {
		wp := &all.Items[i]
		if m.live(wp) {
			continue
		}
		// Preconditioned on the version that was read, so a placement that
		// refreshed the record in the meantime is not undone.
		err := m.client.Delete(ctx, wp, client.Preconditions{ResourceVersion: &wp.ResourceVersion})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			m.log.WarnContext(ctx, "expiring a workload placement failed",
				slog.String("name", wp.Name),
				slog.Any("error", err),
			)
		}
	}
}

// recordName is the object name for a workload under a policy.
//
// A digest rather than the names: a workload name is the caller's own string
// and need not be a valid object name, and joining two names with any
// separator a name may contain lets two different pairs meet in the middle.
// recall checks the spec regardless.
func recordName(policy, workload string) string {
	sum := sha256.Sum256([]byte(policy + "\x00" + workload))
	return "wp-" + hex.EncodeToString(sum[:16])
}

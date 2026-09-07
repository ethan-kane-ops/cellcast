package placement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// Placement outcomes that callers distinguish.
//
// They are separate errors because they call for different operator action and,
// under ENG-175, different client behaviour. Collapsing them into one "no
// placement" would make an authorization refusal indistinguishable from a
// fleet-wide capacity blackout, and those must never be handled the same way.
var (
	// ErrNoPolicy means no PlacementPolicy matches the caller. This is the
	// deny-by-default path and it is an authorization refusal.
	ErrNoPolicy = errors.New("no placement policy matches this caller")

	// ErrNoPermittedCells means the policy matched but its selector permits no
	// registered cell. Almost always a selector that does not match the labels
	// anyone actually applied.
	ErrNoPermittedCells = errors.New("policy permits no registered cell")

	// ErrNoEligibleCells means every permitted cell was refused on state:
	// draining, or dark without a dark-targeting request.
	ErrNoEligibleCells = errors.New("no permitted cell is currently accepting placements")

	// ErrCapacityUnknown means every permitted, eligible cell has stale or
	// missing capacity. Distinct on purpose (docs/architecture.md ADR-002): the
	// hub refuses to guess, and the client's declared fallback stance decides
	// what happens next (ENG-175). It is never resolved by picking one.
	ErrCapacityUnknown = errors.New("no permitted cell has usable capacity")

	// ErrDarkNotPermitted means the caller asked for a dark cell under a policy
	// that does not allow it. Refused rather than quietly downgraded to an
	// ordinary placement, because a QA smoke test landing on a live cell is the
	// opposite of what was asked for.
	ErrDarkNotPermitted = errors.New("policy does not permit dark targeting")
)

// Stages a candidate can be refused at, in the order they are applied.
const (
	StagePermission = "permission"
	StageState      = "state"
	StageCapacity   = "capacity"
	StageScore      = "score"
)

// Request is what a caller asked for.
//
// Deliberately small. There is no field here for a preferred cell or a scoring
// strategy: a caller that can pick either can steer itself into the cell it
// wants, which is the whole attack this package exists to prevent.
type Request struct {
	// Workload names what is being deployed. It appears in the trace and in
	// audit records and does not affect the decision.
	Workload string

	// TargetDark asks for a dark cell, for a QA smoke test against a cell
	// serving no production traffic. Honoured only when the caller's policy
	// sets allowDarkTargeting.
	TargetDark bool
}

// Candidate is one cell's journey through the filter.
//
// Every registered cell appears exactly once, admitted or not, so `--explain`
// (ENG-114) can show why a cell the caller expected was not chosen. A rejection
// nobody can read is a rejection somebody works around.
type Candidate struct {
	Cell string
	// Admitted is whether the cell survived every filter stage.
	Admitted bool
	// Stage is the filter stage that refused it, empty when admitted.
	Stage string
	// Reason is why that stage refused it, empty when admitted.
	Reason string
	// Utilisation is the score, meaningful only when Admitted.
	Utilisation float64
}

// Decision is a placement and the reasoning behind it.
type Decision struct {
	// Cell is the chosen cell.
	Cell string
	// Policy is the PlacementPolicy that governed the decision.
	Policy string
	// Strategy is the scoring strategy that policy selected.
	Strategy cellcastv1alpha1.ScoringStrategy
	// TargetedDark is whether the decision was made against dark cells.
	TargetedDark bool
	// TokenTTL is the credential lifetime the governing policy declared, nil
	// when it declared none.
	//
	// Carried on the decision so the broker is bounded by the same policy that
	// authorised the placement. Looking it up again afterwards would open a
	// window where the two could disagree.
	TokenTTL *cellcastv1alpha1.TokenTTLPolicy
	// Candidates is every registered cell with its verdict, ordered by name.
	Candidates []Candidate
}

// Admitted returns the candidates that survived the filter, best first.
func (d *Decision) Admitted() []Candidate {
	var out []Candidate
	for _, c := range d.Candidates {
		if c.Admitted {
			out = append(out, c)
		}
	}
	return out
}

// Engine answers placement requests.
type Engine struct {
	reader   client.Reader
	capacity *capacity.Registry
	ns       string
	log      *slog.Logger

	// cursors carry round-robin position per policy. In memory and per replica
	// on purpose: a shared cursor would need consensus on the deploy critical
	// path, and cellcast is advisory (ADR-006). Rotation across a multi-replica
	// hub is therefore approximate, which is the correct trade for a hint.
	mu      sync.Mutex
	cursors map[string]uint64
}

// NewEngine builds a placement engine.
//
// The capacity index may be nil, in which case no cell has usable capacity and
// every request fails with ErrCapacityUnknown. That is the fail-closed
// behaviour: a hub with no capacity view must not fall back to picking
// arbitrarily.
func NewEngine(reader client.Reader, index *capacity.Registry, namespace string, log *slog.Logger) *Engine {
	return &Engine{
		reader:   reader,
		capacity: index,
		ns:       namespace,
		log:      log,
		cursors:  make(map[string]uint64),
	}
}

// Place resolves a caller and a request to a cell.
//
// The order below is the security control. Permission is evaluated before
// eligibility, eligibility before scoring, and scoring only ever chooses among
// cells that already passed both.
func (e *Engine) Place(ctx context.Context, id *identity.Identity, req Request) (*Decision, error) {
	if id == nil {
		// Unreachable through the API, where the middleware rejects an
		// unauthenticated request before any handler runs. Guarded anyway
		// because "identity was nil" must never mean "match every policy".
		return nil, ErrNoPolicy
	}

	var policies cellcastv1alpha1.PlacementPolicyList
	if err := e.reader.List(ctx, &policies, client.InNamespace(e.ns)); err != nil {
		return nil, fmt.Errorf("listing placement policies: %w", err)
	}

	policy, ambiguous, err := selectPolicy(policies.Items, id)
	if err != nil {
		// The refusal returned to the caller is deliberately bare, so the
		// detail an operator needs to fix the policy goes here instead.
		e.log.WarnContext(ctx, "placement refused: no policy matches this caller",
			slog.String("issuer", id.Issuer),
			slog.String("subject", id.Subject),
			slog.Int("policies_in_namespace", len(policies.Items)),
		)
		return nil, err
	}
	if ambiguous {
		e.log.WarnContext(ctx, "more than one placement policy matches this caller at the same specificity",
			slog.String("policy", policy.Name),
			slog.String("issuer", id.Issuer),
			slog.String("subject", id.Subject),
		)
	}

	// Asking for a dark cell under a policy that forbids it is refused rather
	// than downgraded. A smoke test that silently lands on a live cell is worse
	// than one that fails.
	if req.TargetDark && !policy.Spec.AllowDarkTargeting {
		return nil, fmt.Errorf("%w: %s", ErrDarkNotPermitted, policy.Name)
	}

	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PermittedCells)
	if err != nil {
		return nil, fmt.Errorf("policy %s has an invalid permittedCells selector: %w", policy.Name, err)
	}

	var clusters cellcastv1alpha1.ClusterList
	if err := e.reader.List(ctx, &clusters, client.InNamespace(e.ns)); err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}

	var snapshot map[string]capacity.Entry
	if e.capacity != nil {
		snapshot = e.capacity.Snapshot()
	}

	decision := &Decision{
		Policy:       policy.Name,
		Strategy:     strategyOf(policy),
		TargetedDark: req.TargetDark,
		TokenTTL:     policy.Spec.TokenTTL,
		Candidates:   filter(clusters.Items, selector, req.TargetDark, snapshot),
	}

	admitted := decision.Admitted()
	if len(admitted) == 0 {
		return nil, refusalFor(decision.Candidates)
	}

	decision.Cell = e.score(policy, decision.Strategy, admitted)
	return decision, nil
}

func strategyOf(policy *cellcastv1alpha1.PlacementPolicy) cellcastv1alpha1.ScoringStrategy {
	if policy.Spec.Strategy == "" {
		return cellcastv1alpha1.ScoringLeastLoaded
	}
	return policy.Spec.Strategy
}

// filter runs the three filter stages over every registered cell.
//
// The stages are ordered cheapest-and-most-restrictive first, and a cell is
// recorded at the first stage that refuses it. Permission comes first so that
// a caller can never learn the state or utilisation of a cell it is not
// permitted to reach.
func filter(
	clusters []cellcastv1alpha1.Cluster,
	selector labels.Selector,
	wantDark bool,
	snapshot map[string]capacity.Entry,
) []Candidate {
	out := make([]Candidate, 0, len(clusters))

	for i := range clusters {
		cl := &clusters[i]
		c := Candidate{Cell: cl.Name}

		switch {
		case !selector.Matches(labels.Set(cl.Labels)):
			c.Stage, c.Reason = StagePermission, "cell is not permitted by this caller's policy"

		default:
			if reason := cl.Spec.State.RejectsPlacement(wantDark); reason != "" {
				c.Stage, c.Reason = StageState, reason
				break
			}
			entry, ok := snapshot[cl.Name]
			switch {
			case !ok:
				c.Stage, c.Reason = StageCapacity, "no capacity has ever been reported for this cell"
			case entry.Health != capacity.HealthFresh:
				// Never read as empty: a missing entry scores as zero
				// utilisation, which is the best possible score, so the cell
				// that broke badly enough to stop reporting would win every
				// placement (docs/architecture.md ADR-002).
				c.Stage, c.Reason = StageCapacity, "capacity is stale; the cell is Unknown and excluded"
			default:
				c.Admitted, c.Utilisation = true, entry.Utilisation
			}
		}

		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Cell < out[j].Cell })
	return out
}

// refusalFor turns an empty candidate set into the most specific error it
// supports, so an operator is pointed at the stage that actually stopped the
// request rather than at the last one.
func refusalFor(candidates []Candidate) error {
	var permitted, eligible bool
	for _, c := range candidates {
		if c.Stage != StagePermission {
			permitted = true
		}
		if c.Stage == StageCapacity {
			eligible = true
		}
	}

	switch {
	case eligible:
		return ErrCapacityUnknown
	case permitted:
		return ErrNoEligibleCells
	default:
		return ErrNoPermittedCells
	}
}

// score picks among cells that have already passed every filter.
//
// Strategy comes from the policy, never the request. A caller that can choose
// the strategy can choose the cell.
func (e *Engine) score(policy *cellcastv1alpha1.PlacementPolicy, strategy cellcastv1alpha1.ScoringStrategy, admitted []Candidate) string {
	// Admitted is already ordered by cell name, which is what makes both
	// strategies reproducible: least-loaded breaks ties by name, and
	// round-robin walks a stable sequence.
	if strategy == cellcastv1alpha1.ScoringRoundRobin {
		e.mu.Lock()
		defer e.mu.Unlock()
		n := e.cursors[policy.Name]
		e.cursors[policy.Name] = n + 1
		return admitted[n%uint64(len(admitted))].Cell
	}

	best := admitted[0]
	for _, c := range admitted[1:] {
		if c.Utilisation < best.Utilisation {
			best = c
		}
	}
	return best.Cell
}

package hub

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
)

// clusterEvents publishes audit records as Kubernetes Events on the cell they
// concern, so that `kubectl describe cluster prod-euw1` tells the operator who
// has been deploying there without leaving the terminal.
//
// This view is lossy and the JSON trail is the authoritative one.
// client-go's event machinery aggregates repeats and applies a per-object spam
// filter, so a hub placing hundreds of deploys an hour will have some of these
// collapsed or dropped. That is the right trade: Events are a convenience for a
// human at a terminal, and making them complete would mean writing to the API
// server on every deploy in the estate's critical path.
type clusterEvents struct {
	recorder events.EventRecorder

	// reader resolves the cell so the Event references the real object. A
	// reference built from a name alone carries no UID, and `kubectl describe`
	// selects events by the UID of the object it just fetched, so such an event
	// would be written and never shown.
	reader client.Reader

	namespace string
	log       *slog.Logger
}

// Notify publishes one record, if it concerns a cell and deserves an event.
func (c *clusterEvents) Notify(ctx context.Context, rec audit.Record) {
	if rec.Cell == "" {
		// A refusal that never reached a cell has no object to hang an event on.
		return
	}

	// Which records deserve an event is clusterEvent's decision and only its
	// decision. A second filter here would be a place for the two to disagree.
	eventType, reason, action, note, args := clusterEvent(rec)
	if reason == "" {
		return
	}

	// Detached from the request context: the record has been written and the
	// response may already be on its way, and an event lost because the caller
	// hung up is an event an operator needed.
	ctx = context.WithoutCancel(ctx)

	var cell cellcastv1alpha1.Cluster
	key := client.ObjectKey{Namespace: c.namespace, Name: rec.Cell}
	if err := c.reader.Get(ctx, key, &cell); err != nil {
		// Best effort by construction. The read is served from the informer
		// cache, so this is a cell that has been deleted since the decision, and
		// failing the deploy over a missing convenience view would be absurd.
		c.log.DebugContext(ctx, "no event recorded for cell",
			slog.String("cell", rec.Cell),
			slog.Any("error", err),
		)
		return
	}

	c.recorder.Eventf(&cell, nil, eventType, reason, action, note, args...)
}

// clusterEvent renders a record as an Event, or returns an empty reason for a
// record that should not produce one.
//
// Split out from Notify so the mapping is testable without a recorder, a client
// or a scheme.
func clusterEvent(rec audit.Record) (eventType, reason, action, note string, args []any) {
	switch {
	case rec.Event == audit.EventPlacement && rec.Outcome == audit.OutcomeGranted:
		return corev1.EventTypeNormal, "Placed", "Placement",
			"placed %s for %s under policy %s",
			[]any{rec.Workload, rec.Subject, rec.Policy}

	case rec.Event == audit.EventMint && rec.Outcome == audit.OutcomeGranted:
		return corev1.EventTypeNormal, "CredentialIssued", "Mint",
			"issued a %s credential to %s, scoped to %s/%s",
			[]any{rec.GrantedTTL.String(), rec.Subject, rec.Namespace, rec.ServiceAccount}

	case rec.Event == audit.EventMint && rec.Outcome == audit.OutcomeRefused:
		// A Warning rather than Normal: placement chose this cell and the hub
		// then could not reach it, which is a fault in the cell's trust
		// configuration and not in the caller's request.
		return corev1.EventTypeWarning, "MintFailed", "Mint",
			"could not mint a credential for %s: %s",
			[]any{rec.Subject, string(rec.Reason)}

	default:
		// A dry run lands here. It reached a decision and changed nothing about
		// the cell it named, so recording it as activity on that cell would
		// make "who deployed here" a list of who asked.
		return "", "", "", "", nil
	}
}

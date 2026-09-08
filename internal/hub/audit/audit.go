// Package audit records what the hub decided and what it issued.
//
// A credential broker with no audit trail is unadoptable. The first question in
// any security review is "show me every token this thing has ever minted, and
// who asked for it", and this package is the answer to it.
//
// Two properties are load-bearing.
//
// It holds no token material and has no field that could carry any. A minted
// token reaches this package only through HashToken, as a SHA-256 digest, which
// is enough to match a token found in a build log against the record that
// issued it and is not enough to use one (docs/threat-model.md T-05).
//
// It cannot be turned off by a log level. Records are written through a logger
// whose handler admits everything it is given, so a hub started with
// --log-level=error still produces a complete trail. A security log that a
// verbosity flag can silence is not a security log.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// Msg is the slog message every audit record carries.
//
// Fixed so that a log pipeline can select the audit trail out of the hub's
// ordinary output with an exact match on one field, rather than by pattern
// matching on prose that is free to change.
const Msg = "audit"

// Event is the kind of thing being recorded.
//
// Placement and mint are separate records for one request rather than one
// combined record, because they are separate facts and either can happen
// without the other: a dry run places and never mints, and a mint can fail
// against a cell that placement legitimately chose.
type Event string

const (
	// EventPlacement is a decision about which cell a caller may reach.
	EventPlacement Event = "placement"

	// EventMint is the issue of a credential for a cell already chosen.
	EventMint Event = "mint"
)

// Outcome is what happened.
type Outcome string

const (
	// OutcomeGranted means the hub did the thing that was asked of it.
	OutcomeGranted Outcome = "granted"

	// OutcomeDryRun means a decision was reached and deliberately not acted on.
	// Distinct from granted so that "which pipeline deployed here" is not
	// answered with a list of pipelines that only asked.
	OutcomeDryRun Outcome = "dry-run"

	// OutcomeRefused means the hub declined. Recorded with the same fields as a
	// success, because a trail that only holds what worked is not a trail.
	OutcomeRefused Outcome = "refused"
)

// Candidate is one cell's journey through the placement filter.
//
// A copy of the placement engine's own candidate rather than a reference to it,
// so that this package stays free of every other package in the hub and the
// audit schema can be read from one file.
type Candidate struct {
	Cell        string  `json:"cell"`
	Admitted    bool    `json:"admitted"`
	Stage       string  `json:"stage,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Utilisation float64 `json:"utilisation"`
}

// Record is one audited fact.
//
// Every field is either derived from a verified token, from operator-authored
// configuration, or from the hub's own decision. None of them can hold
// credential material: the closest is TokenSHA256, which is a digest.
type Record struct {
	// Event is which half of the request this record covers.
	Event Event
	// Outcome is what happened.
	Outcome Outcome
	// RequestID ties the placement and mint records for one request together,
	// and ties both to the request log line and to the X-Request-Id the caller
	// was handed.
	RequestID string

	// Issuer is the OIDC issuer that vouched for the caller.
	Issuer string
	// Subject is the caller's `sub` claim.
	Subject string
	// Claims are the provider claims the caller's identity was derived from,
	// which are also the only claims a PlacementPolicy could have matched on.
	Claims map[string]string

	// Workload is what the caller said it was deploying.
	Workload string
	// TargetedDark is whether the caller asked for a dark cell.
	TargetedDark bool
	// RequestedTTL is the credential lifetime the caller asked for, zero if it
	// asked for none.
	RequestedTTL time.Duration

	// Cell is the chosen cell, empty when the request was refused before one
	// was chosen.
	Cell string
	// Policy is the PlacementPolicy that governed the decision. Present on a
	// refusal too whenever a policy matched and then permitted nothing, which
	// is the case an operator most often has to debug.
	Policy string
	// Strategy is the scoring strategy that policy selected.
	Strategy string
	// Confidence is how much of the permitted fleet the hub could see.
	Confidence string
	// Candidates is every registered cell with its verdict. Carried on the
	// placement record only: repeating the table on the mint record would
	// double the volume of the trail and say nothing new.
	Candidates []Candidate

	// Provider is the trust mechanism the credential was issued through, taken
	// from the chosen cell's spec. Present on a mint record whenever the cell
	// was readable, including one that then failed to mint.
	Provider string
	// Env is the chosen cell's env label. It is on the record because it is not
	// decoration: the label selects the built-in TTL bounds, so it is part of
	// why the lifetime below is the lifetime it is.
	Env string
	// Namespace and ServiceAccount are the scope the credential was issued
	// against, which is the blast radius of the token this record describes.
	Namespace      string
	ServiceAccount string
	// GrantedTTL is the lifetime actually granted, which is not necessarily the
	// one requested.
	GrantedTTL time.Duration
	// ExpiresAt is when the credential stops working, as reported by the
	// issuing authority rather than as requested.
	ExpiresAt time.Time
	// TokenSHA256 is the correlation handle for the minted token. See
	// HashToken.
	TokenSHA256 string

	// Reason is the machine-readable refusal code, empty on success. It is the
	// same taxonomy the caller received, so an operator and a pipeline are
	// reading the same word for the same event.
	Reason refusal.Reason
	// Error is the internal detail behind a refusal, empty on success. It goes
	// here and not to the caller: a minting failure names service accounts and
	// namespaces inside the target cell.
	Error string
}

// Notifier publishes a record somewhere besides the log.
//
// It exists so that this package can stay free of Kubernetes: the hub supplies
// an implementation that turns a record into an Event on the Cluster it
// concerns, and neither half has to know how the other works.
//
// An implementation must be best-effort and must not block. A trail entry that
// could not be published a second way is not a reason to fail the deploy it
// describes, and the log record has already been written by the time Notify is
// called.
type Notifier interface {
	Notify(ctx context.Context, rec Record)
}

// Auditor writes audit records.
type Auditor struct {
	log       *slog.Logger
	notifiers []Notifier
}

// New builds an auditor writing through log.
//
// Any number of notifiers may be attached, including none, and a nil one is
// skipped. The log is not optional and there is no constructor that omits it:
// an auditor that writes nowhere would let the hub run with the appearance of
// an audit trail and none of the substance.
func New(log *slog.Logger, notifiers ...Notifier) *Auditor {
	return &Auditor{log: log, notifiers: notifiers}
}

// Record writes one record.
//
// The log is written first and unconditionally. A notifier that panics or
// blocks must not be able to cost the trail a record, which is why it is the
// notifier contract that forbids both rather than a recover here: swallowing a
// panic would hide a broken view indefinitely.
func (a *Auditor) Record(ctx context.Context, rec Record) {
	a.log.LogAttrs(ctx, slog.LevelInfo, Msg, rec.attrs()...)
	for _, n := range a.notifiers {
		if n != nil {
			n.Notify(ctx, rec)
		}
	}
}

// attrs renders a record as structured attributes.
//
// Empty fields are omitted so that a placement refusal does not carry eight
// zero values for a mint that never happened. Durations are rendered as Go
// duration strings rather than as slog.Duration, which a JSON handler writes as
// a nanosecond count that nobody reads correctly at a glance.
func (r Record) attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 20)
	attrs = append(attrs,
		slog.String("event", string(r.Event)),
		slog.String("outcome", string(r.Outcome)),
	)
	attrs = appendNonEmpty(attrs,
		"request_id", r.RequestID,
		"issuer", r.Issuer,
		"subject", r.Subject,
	)
	if len(r.Claims) > 0 {
		attrs = append(attrs, slog.Any("claims", r.Claims))
	}
	attrs = appendNonEmpty(attrs, "workload", r.Workload)
	if r.TargetedDark {
		attrs = append(attrs, slog.Bool("targeted_dark", true))
	}
	if r.RequestedTTL > 0 {
		attrs = append(attrs, slog.String("requested_ttl", r.RequestedTTL.String()))
	}
	attrs = appendNonEmpty(attrs,
		"cell", r.Cell,
		"policy", r.Policy,
		"strategy", r.Strategy,
		"confidence", r.Confidence,
	)
	if len(r.Candidates) > 0 {
		attrs = append(attrs, slog.Any("candidates", r.Candidates))
	}
	attrs = appendNonEmpty(attrs,
		"provider", r.Provider,
		"env", r.Env,
		"namespace", r.Namespace,
		"service_account", r.ServiceAccount,
	)
	if r.GrantedTTL > 0 {
		attrs = append(attrs, slog.String("granted_ttl", r.GrantedTTL.String()))
	}
	if !r.ExpiresAt.IsZero() {
		attrs = append(attrs, slog.Time("expires_at", r.ExpiresAt))
	}
	attrs = appendNonEmpty(attrs,
		"token_sha256", r.TokenSHA256,
		"reason", string(r.Reason),
		"error", r.Error,
	)
	return attrs
}

// appendNonEmpty appends key/value pairs, skipping the empty ones.
func appendNonEmpty(attrs []slog.Attr, kv ...string) []slog.Attr {
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			attrs = append(attrs, slog.String(kv[i], kv[i+1]))
		}
	}
	return attrs
}

// HashToken returns the correlation handle for a minted token.
//
// SHA-256 over the whole token, hex, labelled with the algorithm so that the
// digest stays readable if a second one is ever added. An operator holding a
// token that leaked into a build log can compute this and find the record that
// issued it; nobody holding the digest can reconstruct the token.
//
// A prefix of the token would correlate just as well and is forbidden. A JWT's
// leading bytes are its header and the segment boundaries move, so "just a
// prefix" is not a fixed amount of the secret, and every leak of this kind
// began as a debug line somebody thought was short enough to be safe.
func HashToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

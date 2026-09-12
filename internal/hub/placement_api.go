package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// maxPlacementBytes bounds a placement request body.
const maxPlacementBytes = 4 << 10

// placementRequest is the body of POST /api/v1/placement.
type placementRequest struct {
	// Workload names what is being deployed. It is recorded, not scored.
	Workload string `json:"workload"`
	// TargetDark asks for a dark cell, honoured only under a policy that
	// permits it.
	TargetDark bool `json:"targetDark,omitempty"`
	// TTL is the credential lifetime the caller would like. It may be shorter
	// than policy allows and is clamped if longer.
	TTL string `json:"ttl,omitempty"`
	// DryRun runs the whole decision and stops before minting.
	DryRun bool `json:"dryRun,omitempty"`
	// Explain returns the candidate table alongside the decision.
	Explain bool `json:"explain,omitempty"`
}

// candidateResponse is one cell's journey through the filter.
type candidateResponse struct {
	Cell        string  `json:"cell"`
	Admitted    bool    `json:"admitted"`
	Stage       string  `json:"stage,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	Utilisation float64 `json:"utilisation"`
	Chosen      bool    `json:"chosen,omitempty"`
}

// credentialResponse is the minted credential.
//
// The field names match kubeconfig's own so that assembling one client-side is
// transcription rather than interpretation.
type credentialResponse struct {
	Server                   string    `json:"server"`
	CertificateAuthorityData string    `json:"certificateAuthorityData,omitempty"`
	Namespace                string    `json:"namespace"`
	ServiceAccount           string    `json:"serviceAccount"`
	Token                    string    `json:"token"`
	ExpiresAt                time.Time `json:"expiresAt"`
}

// ttlResponse says what lifetime was granted and why.
//
// Always returned, because a caller that asked for thirty minutes and received
// ten has to find out now rather than when its deploy stops halfway through.
type ttlResponse struct {
	Granted   string `json:"granted"`
	Default   string `json:"default"`
	Max       string `json:"max"`
	Requested string `json:"requested,omitempty"`
	Clamped   bool   `json:"clamped,omitempty"`
}

// placementResponse is the body of a successful placement.
// identityResponse is the caller's own identity, as the hub parsed it.
//
// Echoed back because the commonest authorization failure is a policy naming a
// subject in a format the issuer does not actually produce, and the caller
// cannot see the difference from their side: the refusal carries a reason and
// nothing else, and only the hub's audit record names what was presented.
//
// It discloses nothing new. These are the caller's own claims, out of the token
// the caller just sent, and they are exactly the three fields audit.Record
// already carries and logs. That classification is the allowlist: anything
// added here has to be a field TestRecordHasNoFieldThatCouldHoldAToken has
// already ruled on. Policy contents are deliberately not here, because a
// PlacementPolicy is not the caller's to read.
type identityResponse struct {
	Issuer  string            `json:"issuer"`
	Subject string            `json:"subject"`
	Claims  map[string]string `json:"claims,omitempty"`
}

func identityOf(id *identity.Identity) *identityResponse {
	if id == nil {
		return nil
	}
	return &identityResponse{Issuer: id.Issuer, Subject: id.Subject, Claims: id.Claims}
}

type placementResponse struct {
	Cell     string `json:"cell"`
	Policy   string `json:"policy"`
	Strategy string `json:"strategy"`
	// Confidence says how much of the permitted fleet the hub could see when it
	// decided. Always sent, because a recommendation that does not state its
	// own confidence is being read as a command.
	Confidence string `json:"confidence"`
	// DecidedFor is the subject the hub resolved the caller's token to. It
	// answers "who did the hub think I was", and it is what lets a cached
	// decision record whose decision it was.
	DecidedFor   string              `json:"decidedFor"`
	TargetedDark bool                `json:"targetedDark,omitempty"`
	DryRun       bool                `json:"dryRun,omitempty"`
	Credential   *credentialResponse `json:"credential,omitempty"`
	TTL          *ttlResponse        `json:"ttl,omitempty"`
	Candidates   []candidateResponse `json:"candidates,omitempty"`
	// Identity is the caller as the hub read them, on success as well as on
	// a refusal: a placement that succeeded under the wrong policy is the same
	// question asked the other way round.
	Identity *identityResponse `json:"identity,omitempty"`
}

// handlePlacement serves POST /api/v1/placement.
//
// The whole request in one handler: authenticate, filter by permission, filter
// by eligibility, score, mint. The order is the security control and the engine
// enforces it, not this function. What this function owns is turning each
// outcome into a status and a reason a client can act on.
func (s *Server) handlePlacement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// rec accumulates the audit record as the request works its way through the
	// handler, so that whichever exit is taken has already gathered everything
	// known at that point.
	rec := audit.Record{Event: audit.EventPlacement, RequestID: requestIDFrom(ctx)}

	// Latency is measured over the whole handler, mint included, because that
	// is the number added to a deploy. Labelled from rec at return time, so a
	// request refused in three milliseconds is not averaged in with one that
	// waited on a spoke.
	start := time.Now()
	defer func() { s.metrics.ObservePlacement(string(rec.Outcome), time.Since(start)) }()

	// refuse audits the refusal and then writes it. Every early return below
	// goes through it, which is what makes "no refusal leaves the hub
	// unrecorded" a property of the code rather than of remembering.
	refuse := func(status int, reason refusal.Reason, msg string, cause error) {
		rec.Outcome = audit.OutcomeRefused
		rec.Reason = reason
		if cause != nil {
			rec.Error = cause.Error()
		}
		s.audit.Record(ctx, rec)
		// From rec rather than from the identity variable, which is not in
		// scope for the refusals above authentication. rec is empty then, and
		// an empty block is omitted rather than reported as a caller with no
		// subject.
		var echo *identityResponse
		if rec.Issuer != "" || rec.Subject != "" {
			echo = &identityResponse{Issuer: rec.Issuer, Subject: rec.Subject, Claims: rec.Claims}
		}
		writeRefusal(w, status, reason, msg, echo)
	}

	id, ok := IdentityFrom(ctx)
	if !ok {
		// Unreachable: the middleware rejects an unauthenticated request before
		// any handler runs. Guarded because a nil identity must never be read
		// as a caller who matches every policy.
		refuse(http.StatusUnauthorized, refusal.NoPolicy, "unauthenticated", nil)
		return
	}
	rec.Issuer, rec.Subject, rec.Claims = id.Issuer, id.Subject, id.Claims

	// After authentication, because the bucket is keyed on who is asking, and
	// before any other work, because not doing the work is the point. A
	// limited request is a refusal like any other: audited, counted under its
	// reason, and it names the caller back (docs/threat-model.md T-06).
	if ok, wait := s.limiter.allow(id.Issuer, id.Subject, time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
		refuse(http.StatusTooManyRequests, refusal.RateLimited, "too many placement requests from this caller", nil)
		return
	}

	if s.placer == nil {
		// Distinct from MintUnavailable, which looks identical on the wire and
		// is not. A hub that cannot decide may be answered by the caller's
		// fallback stance; a hub that cannot mint may never be, because a
		// cached placement is never a cached credential (ADR-006).
		refuse(http.StatusServiceUnavailable, refusal.PlacementUnavailable, "placement unavailable", nil)
		return
	}

	if !s.warm.Load() {
		// Fail closed rather than score an empty index. Every cell would be
		// Unknown, so the engine would refuse anyway; refusing here says why in
		// terms an operator can act on, and keeps a replica that went ready on
		// its warmup deadline from reporting the fleet as uniformly dead.
		//
		// PlacementUnavailable rather than CapacityUnknown because the two
		// resolve differently: this one is a property of the hub replica the
		// caller happened to reach and clears itself, so a retry is worth
		// making (ADR-006).
		refuse(http.StatusServiceUnavailable, refusal.PlacementUnavailable,
			"hub is still warming up", nil)
		return
	}

	var req placementRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPlacementBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// The decode error itself is not audited: it is derived from the body,
		// and the body of a placement request is the one thing on this route
		// that is caller-controlled prose.
		refuse(http.StatusBadRequest, refusal.InvalidRequest, "request body is not valid placement JSON", nil)
		return
	}
	rec.Workload, rec.TargetedDark = req.Workload, req.TargetDark

	if req.Workload == "" {
		refuse(http.StatusBadRequest, refusal.InvalidRequest, "workload must be set", nil)
		return
	}

	var requested time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			refuse(http.StatusBadRequest, refusal.InvalidRequest, "ttl must be a positive Go duration, for example 15m", nil)
			return
		}
		requested = d
		rec.RequestedTTL = requested
	}

	// Timing only. What was decided reaches the trace through the audit record
	// below, like every other attribute the hub exports.
	placeCtx, placeSpan := startSpan(ctx, "place")
	decision, err := s.placer.Place(placeCtx, id, placement.Request{
		Workload:   req.Workload,
		TargetDark: req.TargetDark,
	})
	placeSpan.End()
	if err != nil {
		status, reason := placementRefusal(err)
		var refused *placement.RefusedError
		if errors.As(err, &refused) {
			rec.Policy = refused.Policy
			rec.Candidates = auditCandidates(refused.Candidates)
		}
		s.log.InfoContext(ctx, "placement refused",
			slog.String("request_id", requestIDFrom(ctx)),
			slog.String("workload", req.Workload),
			slog.String("subject", id.Subject),
			slog.String("reason", string(reason)),
		)
		refuse(status, reason, refusalMessage(reason), err)
		return
	}

	rec.Cell = decision.Cell
	rec.Policy = decision.Policy
	rec.Strategy = string(decision.Strategy)
	rec.Confidence = string(decision.Confidence())
	rec.Candidates = auditCandidates(decision.Candidates)
	rec.Outcome = audit.OutcomeGranted
	if req.DryRun {
		rec.Outcome = audit.OutcomeDryRun
	}
	s.audit.Record(ctx, rec)

	resp := placementResponse{
		Cell:         decision.Cell,
		Policy:       decision.Policy,
		Strategy:     string(decision.Strategy),
		Confidence:   string(decision.Confidence()),
		DecidedFor:   id.Subject,
		TargetedDark: decision.TargetedDark,
		DryRun:       req.DryRun,
		Identity:     identityOf(id),
	}
	if req.Explain {
		resp.Candidates = explain(decision)
	}

	if req.DryRun {
		// Stops before minting, and after everything else. A dry run
		// that skipped the policy or capacity lookups would answer a different
		// question from the one the real call asks.
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// The mint is a second audited fact about the same request, tied to the
	// first by request_id. The candidate table belongs to the decision and is
	// dropped here: repeating it would double the size of the trail and say
	// nothing the placement record did not already say.
	rec.Event = audit.EventMint
	rec.Outcome, rec.Reason, rec.Error = "", "", ""
	rec.Candidates = nil

	// The one step that leaves the process: reading the cell, reading its trust
	// configuration and the TokenRequest round trip to the spoke all happen
	// inside it. ctx is reassigned rather than shadowed, so that refuse and the
	// audit record below land on this span rather than on the request's.
	ctx, span := startSpan(ctx, "mint")
	defer span.End()

	if s.minter == nil {
		// A cell name with no way to reach it looks like success and is not.
		refuse(http.StatusServiceUnavailable, refusal.MintUnavailable, "credential broker unavailable", nil)
		return
	}

	cluster, err := s.clusterByName(ctx, decision.Cell)
	if err != nil {
		s.log.ErrorContext(ctx, "reading the chosen cell failed",
			slog.String("request_id", requestIDFrom(ctx)),
			slog.String("cell", decision.Cell),
			slog.Any("error", err),
		)
		refuse(http.StatusInternalServerError, refusal.MintFailed, "internal error", err)
		return
	}

	// Recorded before the mint is attempted so that a failure is still audited
	// and counted against the provider that could not issue.
	rec.Provider = string(cluster.Spec.Provider)
	rec.Env = cluster.Labels[broker.EnvLabel]

	cred, ttl, err := s.minter.Mint(ctx, cluster, decision.TokenTTL, requested, id.Subject)
	if err != nil {
		// The detail goes to the operator's log, not to the caller: a minting
		// failure names service accounts and namespaces in the target cell.
		s.log.ErrorContext(ctx, "minting failed",
			slog.String("request_id", requestIDFrom(ctx)),
			slog.String("cell", decision.Cell),
			slog.String("subject", id.Subject),
			slog.Any("error", err),
		)
		refuse(http.StatusServiceUnavailable, refusal.MintFailed, "could not mint a credential for the chosen cell", err)
		return
	}

	// Audited before the response is written. A token that exists and was not
	// recorded is the one outcome this trail may never produce, and a client
	// that hangs up mid-response still holds a working credential.
	rec.Outcome = audit.OutcomeGranted
	rec.Namespace = cred.Namespace
	rec.ServiceAccount = cred.ServiceAccount
	rec.GrantedTTL = ttl.Granted
	rec.ExpiresAt = cred.ExpiresAt
	rec.TokenSHA256 = audit.HashToken(cred.Token)
	s.audit.Record(ctx, rec)

	resp.Credential = &credentialResponse{
		Server:         cred.Server,
		Namespace:      cred.Namespace,
		ServiceAccount: cred.ServiceAccount,
		Token:          cred.Token,
		ExpiresAt:      cred.ExpiresAt,
	}
	if len(cred.CABundle) > 0 {
		resp.Credential.CertificateAuthorityData = base64.StdEncoding.EncodeToString(cred.CABundle)
	}
	resp.TTL = &ttlResponse{
		Granted: ttl.Granted.String(),
		Default: ttl.Default.String(),
		Max:     ttl.Max.String(),
		Clamped: ttl.Clamped,
	}
	if ttl.Requested > 0 {
		resp.TTL.Requested = ttl.Requested.String()
	}

	writeJSON(w, http.StatusOK, resp)
}

// explain renders the candidate table, marking the winner.
//
// Every registered cell appears, including ones the caller's policy refused.
// That is a disclosure, and an accepted one: the same caller can already list
// the fleet through GET /api/v1/clusters, so withholding it here would hide the
// answer to "why did my deploy not land where I expected" without withholding
// anything an attacker could not already read.
func explain(d *placement.Decision) []candidateResponse {
	out := make([]candidateResponse, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		out = append(out, candidateResponse{
			Cell:        c.Cell,
			Admitted:    c.Admitted,
			Stage:       c.Stage,
			Reason:      c.Reason,
			Utilisation: c.Utilisation,
			Chosen:      c.Cell == d.Cell,
		})
	}
	return out
}

// auditCandidates renders the candidate table for the audit trail.
//
// A separate rendering from explain: that one answers the caller and marks the
// winner, this one is the operator's record of what was considered and is kept
// whether or not the caller asked to be told.
func auditCandidates(candidates []placement.Candidate) []audit.Candidate {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]audit.Candidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, audit.Candidate{
			Cell:        c.Cell,
			Admitted:    c.Admitted,
			Stage:       c.Stage,
			Reason:      c.Reason,
			Utilisation: c.Utilisation,
		})
	}
	return out
}

// placementRefusal maps an engine refusal to a status and a reason code.
//
// The split that matters is retryable against not. A caller refused on
// authorization must not retry; one refused because every permitted cell is
// draining should.
func placementRefusal(err error) (int, refusal.Reason) {
	switch {
	case errors.Is(err, placement.ErrNoPolicy):
		return http.StatusForbidden, refusal.NoPolicy
	case errors.Is(err, placement.ErrDarkNotPermitted):
		return http.StatusForbidden, refusal.DarkNotPermitted
	case errors.Is(err, placement.ErrNoPermittedCells):
		// Not 503: retrying cannot fix a selector that matches nothing, and
		// reporting it as unavailable would send a client into a backoff loop
		// waiting for an operator to edit a policy.
		return http.StatusConflict, refusal.NoPermittedCells
	case errors.Is(err, placement.ErrNoEligibleCells):
		return http.StatusServiceUnavailable, refusal.NoEligibleCells
	case errors.Is(err, placement.ErrCapacityUnknown):
		return http.StatusServiceUnavailable, refusal.CapacityUnknown
	default:
		return http.StatusInternalServerError, refusal.MintFailed
	}
}

// refusalMessage is the prose half of a refusal.
//
// Coarse. A caller learns that it was refused and whether retrying
// could help; which cells exist and which policy was consulted go to the hub
// log, which the operator reads and the caller does not.
func refusalMessage(reason refusal.Reason) string {
	switch reason {
	case refusal.NoPolicy:
		return "no placement policy permits this caller"
	case refusal.DarkNotPermitted:
		return "this caller may not target dark cells"
	case refusal.NoPermittedCells:
		return "the matching policy permits no registered cell"
	case refusal.NoEligibleCells:
		return "no permitted cell is currently accepting placements"
	case refusal.CapacityUnknown:
		return "no permitted cell has usable capacity"
	default:
		return "internal error"
	}
}

// writeRefusal writes a refusal carrying both a machine-readable reason and a
// human one.
func writeRefusal(w http.ResponseWriter, status int, reason refusal.Reason, msg string, id *identityResponse) {
	body := struct {
		Reason   string            `json:"reason"`
		Error    string            `json:"error"`
		Identity *identityResponse `json:"identity,omitempty"`
	}{Reason: string(reason), Error: msg, Identity: id}
	writeJSON(w, status, body)
}

// clusterByName reads one registered cell.
func (s *Server) clusterByName(ctx context.Context, name string) (*cellcastv1alpha1.Cluster, error) {
	if s.k8s == nil {
		return nil, fmt.Errorf("registry unavailable")
	}
	var cl cellcastv1alpha1.Cluster
	key := client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}
	if err := s.k8s.Get(ctx, key, &cl); err != nil {
		return nil, fmt.Errorf("reading cell %s: %w", name, err)
	}
	return &cl, nil
}

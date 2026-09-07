package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// maxPlacementBytes bounds a placement request body.
const maxPlacementBytes = 4 << 10

// Refusal reasons returned to the caller.
//
// These are the machine-readable half of a refusal, and they exist because the
// client's fallback stance (ENG-175) has to distinguish "you are not allowed
// there" from "nothing is available right now". Those call for opposite
// behaviour, and a client must never have to match on prose to tell them apart.
const (
	ReasonNoPolicy         = "NoPolicy"
	ReasonDarkNotPermitted = "DarkNotPermitted"
	ReasonNoPermittedCells = "NoPermittedCells"
	ReasonNoEligibleCells  = "NoEligibleCells"
	ReasonCapacityUnknown  = "CapacityUnknown"
	ReasonMintUnavailable  = "MintUnavailable"
	ReasonMintFailed       = "MintFailed"
	ReasonInvalidRequest   = "InvalidRequest"
)

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
type placementResponse struct {
	Cell         string              `json:"cell"`
	Policy       string              `json:"policy"`
	Strategy     string              `json:"strategy"`
	TargetedDark bool                `json:"targetedDark,omitempty"`
	DryRun       bool                `json:"dryRun,omitempty"`
	Credential   *credentialResponse `json:"credential,omitempty"`
	TTL          *ttlResponse        `json:"ttl,omitempty"`
	Candidates   []candidateResponse `json:"candidates,omitempty"`
}

// handlePlacement serves POST /api/v1/placement.
//
// This is the whole product in one handler: authenticate, filter by permission,
// filter by eligibility, score, mint. The order is the security control and it
// is enforced by the engine rather than here; what this function owns is
// turning each outcome into a status and a reason a client can act on.
func (s *Server) handlePlacement(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		// Unreachable: the middleware rejects an unauthenticated request before
		// any handler runs. Guarded because a nil identity must never be read
		// as a caller who matches every policy.
		writeRefusal(w, http.StatusUnauthorized, ReasonNoPolicy, "unauthenticated")
		return
	}
	if s.placer == nil {
		writeRefusal(w, http.StatusServiceUnavailable, ReasonMintUnavailable, "placement unavailable")
		return
	}

	var req placementRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPlacementBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeRefusal(w, http.StatusBadRequest, ReasonInvalidRequest, "request body is not valid placement JSON")
		return
	}
	if req.Workload == "" {
		writeRefusal(w, http.StatusBadRequest, ReasonInvalidRequest, "workload must be set")
		return
	}

	var requested time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			writeRefusal(w, http.StatusBadRequest, ReasonInvalidRequest, "ttl must be a positive Go duration, for example 15m")
			return
		}
		requested = d
	}

	decision, err := s.placer.Place(r.Context(), id, placement.Request{
		Workload:   req.Workload,
		TargetDark: req.TargetDark,
	})
	if err != nil {
		status, reason := placementRefusal(err)
		s.log.InfoContext(r.Context(), "placement refused",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("workload", req.Workload),
			slog.String("subject", id.Subject),
			slog.String("reason", reason),
		)
		writeRefusal(w, status, reason, refusalMessage(reason))
		return
	}

	resp := placementResponse{
		Cell:         decision.Cell,
		Policy:       decision.Policy,
		Strategy:     string(decision.Strategy),
		TargetedDark: decision.TargetedDark,
		DryRun:       req.DryRun,
	}
	if req.Explain {
		resp.Candidates = explain(decision)
	}

	if req.DryRun {
		// Stops before minting, deliberately after everything else. A dry run
		// that skipped the policy or capacity lookups would answer a different
		// question from the one the real call asks.
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if s.minter == nil {
		// A cell name with no way to reach it looks like success and is not.
		writeRefusal(w, http.StatusServiceUnavailable, ReasonMintUnavailable, "credential broker unavailable")
		return
	}

	cluster, err := s.clusterByName(r.Context(), decision.Cell)
	if err != nil {
		s.log.ErrorContext(r.Context(), "reading the chosen cell failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cell", decision.Cell),
			slog.Any("error", err),
		)
		writeRefusal(w, http.StatusInternalServerError, ReasonMintFailed, "internal error")
		return
	}

	cred, ttl, err := s.minter.Mint(r.Context(), cluster, decision.TokenTTL, requested, id.Subject)
	if err != nil {
		// The detail goes to the operator's log, not to the caller: a minting
		// failure names service accounts and namespaces in the target cell.
		s.log.ErrorContext(r.Context(), "minting failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cell", decision.Cell),
			slog.String("subject", id.Subject),
			slog.Any("error", err),
		)
		writeRefusal(w, http.StatusServiceUnavailable, ReasonMintFailed, "could not mint a credential for the chosen cell")
		return
	}

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
// That is a disclosure and it is deliberate: the same caller can already list
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

// placementRefusal maps an engine refusal to a status and a reason code.
//
// The split that matters is retryable against not. A caller refused on
// authorization must not retry; one refused because every permitted cell is
// draining should.
func placementRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, placement.ErrNoPolicy):
		return http.StatusForbidden, ReasonNoPolicy
	case errors.Is(err, placement.ErrDarkNotPermitted):
		return http.StatusForbidden, ReasonDarkNotPermitted
	case errors.Is(err, placement.ErrNoPermittedCells):
		// Not 503: retrying cannot fix a selector that matches nothing, and
		// reporting it as unavailable would send a client into a backoff loop
		// waiting for an operator to edit a policy.
		return http.StatusConflict, ReasonNoPermittedCells
	case errors.Is(err, placement.ErrNoEligibleCells):
		return http.StatusServiceUnavailable, ReasonNoEligibleCells
	case errors.Is(err, placement.ErrCapacityUnknown):
		return http.StatusServiceUnavailable, ReasonCapacityUnknown
	default:
		return http.StatusInternalServerError, ReasonMintFailed
	}
}

// refusalMessage is the prose half of a refusal.
//
// Coarse on purpose. A caller learns that it was refused and whether retrying
// could help; which cells exist and which policy was consulted go to the hub
// log, which the operator reads and the caller does not.
func refusalMessage(reason string) string {
	switch reason {
	case ReasonNoPolicy:
		return "no placement policy permits this caller"
	case ReasonDarkNotPermitted:
		return "this caller may not target dark cells"
	case ReasonNoPermittedCells:
		return "the matching policy permits no registered cell"
	case ReasonNoEligibleCells:
		return "no permitted cell is currently accepting placements"
	case ReasonCapacityUnknown:
		return "no permitted cell has usable capacity"
	default:
		return "internal error"
	}
}

// writeRefusal writes a refusal carrying both a machine-readable reason and a
// human one.
func writeRefusal(w http.ResponseWriter, status int, reason, msg string) {
	writeJSON(w, status, map[string]string{"reason": reason, "error": msg})
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

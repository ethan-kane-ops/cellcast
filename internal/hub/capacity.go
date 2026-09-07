package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/capacity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
)

// maxCapacityBytes bounds a heartbeat payload. Every field is a number.
const maxCapacityBytes = 4 << 10

// capacityReport is the POST /api/v1/clusters/{name}/capacity request body.
//
// The cell is taken from the path and is deliberately absent here, so a report
// cannot name one cell in its URL and a different one in its body. There is no
// version of that mismatch worth resolving.
type capacityReport struct {
	Nodes                  int   `json:"nodes"`
	CPUMilliAllocatable    int64 `json:"cpuMilliAllocatable"`
	CPUMilliCommitted      int64 `json:"cpuMilliCommitted"`
	MemoryBytesAllocatable int64 `json:"memoryBytesAllocatable"`
	MemoryBytesCommitted   int64 `json:"memoryBytesCommitted"`
	Pods                   int   `json:"pods"`
	PodCapacity            int   `json:"podCapacity"`
}

// capacityEntryResponse is the API view of one cell's capacity.
type capacityEntryResponse struct {
	Cell                   string    `json:"cell"`
	Health                 string    `json:"health"`
	Utilisation            float64   `json:"utilisation"`
	ObservedAt             time.Time `json:"observedAt"`
	Nodes                  int       `json:"nodes"`
	CPUMilliAllocatable    int64     `json:"cpuMilliAllocatable"`
	CPUMilliCommitted      int64     `json:"cpuMilliCommitted"`
	MemoryBytesAllocatable int64     `json:"memoryBytesAllocatable"`
	MemoryBytesCommitted   int64     `json:"memoryBytesCommitted"`
	Pods                   int       `json:"pods"`
	PodCapacity            int       `json:"podCapacity"`
}

func newCapacityEntryResponse(cell string, e capacity.Entry) capacityEntryResponse {
	return capacityEntryResponse{
		Cell:                   cell,
		Health:                 string(e.Health),
		Utilisation:            e.Utilisation,
		ObservedAt:             e.ObservedAt,
		Nodes:                  e.Nodes,
		CPUMilliAllocatable:    e.CPUMilliAllocatable,
		CPUMilliCommitted:      e.CPUMilliCommitted,
		MemoryBytesAllocatable: e.MemoryBytesAllocatable,
		MemoryBytesCommitted:   e.MemoryBytesCommitted,
		Pods:                   e.Pods,
		PodCapacity:            e.PodCapacity,
	}
}

// handleReportCapacity serves POST /api/v1/clusters/{name}/capacity.
//
// The report is refused unless the cell is already a registered Cluster. That
// is both the correctness rule (capacity for a cell nobody registered can never
// be placed on) and the bound on the index: without it, an authenticated
// caller could fill the hub's memory with heartbeats for names it invented
// (docs/threat-model.md T-06).
//
// It is refused again unless the caller is the identity the cell names in
// `spec.reporter`. Authentication alone is not enough here: every agent in the
// fleet holds a valid token, so without this an agent in any cell could report
// for any other (docs/threat-model.md T-07).
func (s *Server) handleReportCapacity(w http.ResponseWriter, r *http.Request) {
	if s.k8s == nil || s.capacity == nil {
		writeError(w, http.StatusServiceUnavailable, "registry unavailable")
		return
	}

	name := r.PathValue("name")

	var cl cellcastv1alpha1.Cluster
	key := client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}
	if err := s.k8s.Get(r.Context(), key, &cl); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "no such cluster")
			return
		}
		s.log.ErrorContext(r.Context(), "reading cluster failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.Any("error", err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	id, _ := IdentityFrom(r.Context())
	if err := authorizeReporter(&cl, id); err != nil {
		// Logged at warn with both identities spelled out, because the shape of
		// this failure is an operator who registered the cell with the wrong
		// issuer, and the only way they can see the value the agent actually
		// presents is here.
		s.log.WarnContext(r.Context(), "capacity report refused: caller is not this cell's reporter",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cell", name),
			slog.String("caller_issuer", identityIssuer(id)),
			slog.String("caller_subject", identitySubject(id)),
			slog.String("reason", err.Error()),
		)
		writeError(w, http.StatusForbidden, "caller is not the declared reporter for this cell")
		return
	}

	report, err := decodeCapacityReport(w, r, name)
	if err == nil {
		err = s.capacity.Report(report)
	}
	if err != nil {
		s.log.WarnContext(r.Context(), "capacity report rejected",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cell", name),
			slog.String("reason", err.Error()),
		)
		status := http.StatusBadRequest
		if errors.Is(err, capacity.ErrTooManyCells) {
			status = http.StatusInsufficientStorage
		}
		writeError(w, status, err.Error())
		return
	}

	entry, _ := s.capacity.Lookup(name)

	// 202 rather than 201: a heartbeat updates a view, it does not create a
	// resource, and the agent has nothing to fetch afterwards. The body echoes
	// what the hub derived so a misreporting agent shows up in its own logs.
	writeJSON(w, http.StatusAccepted, newCapacityEntryResponse(name, entry))
}

func decodeCapacityReport(w http.ResponseWriter, r *http.Request, cell string) (capacity.Report, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCapacityBytes))
	if err != nil {
		return capacity.Report{}, fmt.Errorf("reading request body: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var in capacityReport
	if err := dec.Decode(&in); err != nil {
		return capacity.Report{}, fmt.Errorf("decoding capacity report: %w", err)
	}
	if dec.More() {
		return capacity.Report{}, errors.New("body must contain exactly one JSON object")
	}

	return capacity.Report{
		Cell:                   cell,
		Nodes:                  in.Nodes,
		CPUMilliAllocatable:    in.CPUMilliAllocatable,
		CPUMilliCommitted:      in.CPUMilliCommitted,
		MemoryBytesAllocatable: in.MemoryBytesAllocatable,
		MemoryBytesCommitted:   in.MemoryBytesCommitted,
		Pods:                   in.Pods,
		PodCapacity:            in.PodCapacity,
	}, nil
}

// handleListCapacity serves GET /api/v1/capacity.
//
// Exists so that "why did my deploy not land on the empty cluster" has an
// answer that does not require hub logs. Ordered by cell name so two calls are
// diffable.
func (s *Server) handleListCapacity(w http.ResponseWriter, r *http.Request) {
	if s.capacity == nil {
		writeError(w, http.StatusServiceUnavailable, "capacity index unavailable")
		return
	}

	snapshot := s.capacity.Snapshot()
	out := make([]capacityEntryResponse, 0, len(snapshot))
	for cell, entry := range snapshot {
		out = append(out, newCapacityEntryResponse(cell, entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cell < out[j].Cell })

	writeJSON(w, http.StatusOK, map[string]any{
		"cells":           out,
		"stalenessWindow": s.capacity.Staleness().String(),
	})
}

// errNoReporterDeclared is the refusal for a cell that names no reporter.
var errNoReporterDeclared = errors.New("cell declares no reporter identity")

// authorizeReporter reports whether id is the agent entitled to speak for cl.
//
// An unset spec.reporter refuses every report rather than accepting any. The
// consequence is that a cell nobody has bound an agent to stays Unknown and
// drops out of scoring, which is the same place the staleness guard puts a cell
// whose agent died. Both are visible; a cell scored on numbers from an
// unentitled caller is not.
//
// Issuer and subject are both compared literally and both must match. The
// issuer is the half that separates cells, because the subject is the same
// string in every cell in the fleet.
func authorizeReporter(cl *cellcastv1alpha1.Cluster, id *identity.Identity) error {
	if id == nil {
		// Unreachable behind the auth middleware. Refusing rather than
		// dereferencing keeps that true if the route is ever mounted elsewhere.
		return identity.ErrUnauthenticated
	}
	reporter := cl.Spec.Reporter
	if reporter == nil {
		return errNoReporterDeclared
	}
	if id.Issuer != reporter.Issuer || id.Subject != reporter.Subject {
		return fmt.Errorf("cell expects %s from %s", reporter.Subject, reporter.Issuer)
	}
	return nil
}

func identityIssuer(id *identity.Identity) string {
	if id == nil {
		return ""
	}
	return id.Issuer
}

func identitySubject(id *identity.Identity) string {
	if id == nil {
		return ""
	}
	return id.Subject
}

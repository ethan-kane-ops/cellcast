package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// maxStateChangeBytes bounds a state change payload. The body is one short
// enum value.
const maxStateChangeBytes = 1 << 10

// stateChange is the PATCH /api/v1/clusters/{name}/state request body.
type stateChange struct {
	State string `json:"state"`
}

func (sc *stateChange) validate() (cellcastv1alpha1.ClusterState, error) {
	switch state := cellcastv1alpha1.ClusterState(sc.State); state {
	case cellcastv1alpha1.ClusterStateLive,
		cellcastv1alpha1.ClusterStateDark,
		cellcastv1alpha1.ClusterStateDraining:
		return state, nil
	case "":
		// An omitted state is not read as "reset to the default". Draining a
		// cell and un-draining it are both deliberate acts and both have to be
		// spelled out, because the request that gets this wrong is the one
		// issued from a script during an incident.
		return "", errors.New("state is required and must be one of LIVE, DARK, DRAINING")
	default:
		return "", fmt.Errorf("state must be one of LIVE, DARK, DRAINING, got %q", sc.State)
	}
}

// handleSetClusterState serves PATCH /api/v1/clusters/{name}/state.
//
// State is spec, not status, so this writes the same field `kubectl patch`
// writes and lands in the same API server audit log. The HTTP route exists
// because the operators who drain a cell during an upgrade window are running a
// pipeline, not a kubectl session, and giving that pipeline cluster credentials
// to flip one enum would undo the point of the project.
//
// Draining a cell cannot fail a placement that has already been issued. There
// is nothing to fail: a placement is an advisory decision returned to the
// caller and never retained by the hub (docs/architecture.md ADR-006), so a
// state change affects the next decision and no earlier one.
func (s *Server) handleSetClusterState(w http.ResponseWriter, r *http.Request) {
	if s.k8s == nil {
		writeError(w, http.StatusServiceUnavailable, "registry unavailable")
		return
	}

	name := r.PathValue("name")
	desired, err := decodeStateChange(w, r)
	if err != nil {
		s.log.WarnContext(r.Context(), "cluster state change rejected",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cluster", name),
			slog.String("reason", err.Error()),
		)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var cl cellcastv1alpha1.Cluster
	key := client.ObjectKey{Namespace: s.cfg.Namespace, Name: name}
	if err := s.k8s.Get(r.Context(), key, &cl); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "no such cluster")
			return
		}
		s.log.ErrorContext(r.Context(), "reading cluster failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cluster", name),
			slog.Any("error", err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	previous := cl.Spec.State.Normalize()

	// A merge patch computed from the object read above carries only the state
	// field, so a concurrent label or endpoint edit is not clobbered even
	// though this read came from a cache that may be behind.
	patch := client.MergeFrom(cl.DeepCopy())
	cl.Spec.State = desired
	if err := s.k8s.Patch(r.Context(), &cl, patch); err != nil {
		s.log.ErrorContext(r.Context(), "cluster state change failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cluster", name),
			slog.Any("error", err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.log.InfoContext(r.Context(), "cluster state changed",
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("cluster", name),
		slog.String("from", string(previous)),
		slog.String("to", string(desired)),
	)
	writeJSON(w, http.StatusOK, newClusterResponse(&cl))
}

func decodeStateChange(w http.ResponseWriter, r *http.Request) (cellcastv1alpha1.ClusterState, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStateChangeBytes))
	if err != nil {
		return "", fmt.Errorf("reading request body: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var sc stateChange
	if err := dec.Decode(&sc); err != nil {
		return "", fmt.Errorf("decoding state change: %w", err)
	}
	if dec.More() {
		return "", errors.New("body must contain exactly one JSON object")
	}
	return sc.validate()
}

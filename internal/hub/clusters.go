package hub

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// maxRegistrationBytes bounds a registration payload. A CA bundle is the only
// field that can be large and a full chain is a few kilobytes.
const maxRegistrationBytes = 64 << 10

// reservedLabelPrefix is owned by cellcast and may not be set by a registrant.
//
// PlacementPolicy selectors match on cluster labels (ENG-173), so a registrant
// who could set a label under the system prefix could forge whatever a policy
// is configured to trust. Registration is already privileged (T-04); this keeps
// the blast radius of that privilege inside the operator's own label space.
const reservedLabelPrefix = "cellcast.io"

// errCredentialInPayload reports a registration that tried to supply
// authentication material.
var errCredentialInPayload = errors.New("registration must not carry credentials; cellcast stores trust configuration only")

// credentialFields are payload keys that would carry authentication material.
//
// clusterRegistration has no field for any of them, so DisallowUnknownFields
// already refuses a payload containing one. This list exists so the rejection
// says what is actually wrong: an operator who pasted a kubeconfig stanza needs
// to be told that cellcast never stores credentials, not that field 7 is
// unknown. Nested occurrences are still caught by the decoder, because the
// object containing them is itself an unknown field.
//
// See docs/threat-model.md T-04 and docs/architecture.md ADR-004.
var credentialFields = []string{
	"authProvider",
	"bearerToken",
	"clientCertificate",
	"clientCertificateData",
	"clientKey",
	"clientKeyData",
	"execProvider",
	"kubeconfig",
	"password",
	"token",
	"username",
}

// clusterRegistration is the POST /api/v1/clusters request body.
//
// It is deliberately a hand-written type rather than the CRD serialised
// directly. The wire format is a contract with pipelines and must not shift
// every time an internal field does, and writing it out by hand is what makes
// DisallowUnknownFields a security control rather than a formality: there is no
// field here capable of carrying a credential, so a payload offering one cannot
// decode at all.
type clusterRegistration struct {
	Name           string            `json:"name"`
	Endpoint       string            `json:"endpoint"`
	CABundle       []byte            `json:"caBundle,omitempty"`
	Provider       string            `json:"provider"`
	TrustConfigRef string            `json:"trustConfigRef"`
	Labels         map[string]string `json:"labels,omitempty"`
	State          string            `json:"state,omitempty"`
}

// clusterResponse is the API representation of a registered cell.
//
// The CA bundle is echoed back deliberately: it is a public certificate, and a
// caller comparing what it sent against what was stored is a cheap way to catch
// a truncated paste.
type clusterResponse struct {
	Name           string            `json:"name"`
	Endpoint       string            `json:"endpoint"`
	CABundle       []byte            `json:"caBundle,omitempty"`
	Provider       string            `json:"provider"`
	TrustConfigRef string            `json:"trustConfigRef"`
	Labels         map[string]string `json:"labels,omitempty"`
	State          string            `json:"state"`
	ObservedState  string            `json:"observedState,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
}

// decodeRegistration reads and validates the shape of a registration payload.
func decodeRegistration(w http.ResponseWriter, r *http.Request) (*clusterRegistration, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRegistrationBytes))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}

	if field, ok := findCredentialField(body); ok {
		return nil, fmt.Errorf("%w (found %q)", errCredentialInPayload, field)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var reg clusterRegistration
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("decoding registration: %w", err)
	}
	if dec.More() {
		return nil, errors.New("body must contain exactly one JSON object")
	}
	return &reg, nil
}

// findCredentialField reports the first top-level key that would carry
// authentication material. A body that does not parse is left for the decoder
// to reject with a better message.
func findCredentialField(body []byte) (string, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", false
	}
	for key := range raw {
		for _, banned := range credentialFields {
			if strings.EqualFold(key, banned) {
				return key, true
			}
		}
	}
	return "", false
}

// validate reports every problem with the payload at once.
//
// Validation messages are specific, unlike the deliberately coarse errors
// elsewhere in this package. They describe the caller's own input and reveal
// nothing about the fleet, and an operator registering a cell needs to be told
// which field is wrong.
func (reg *clusterRegistration) validate() error {
	var problems []string

	if reg.Name == "" {
		problems = append(problems, "name is required")
	} else {
		for _, msg := range validation.IsDNS1123Subdomain(reg.Name) {
			problems = append(problems, "name: "+msg)
		}
	}

	problems = append(problems, validateEndpoint(reg.Endpoint)...)
	problems = append(problems, validateCABundle(reg.CABundle)...)

	switch cellcastv1alpha1.Provider(reg.Provider) {
	case cellcastv1alpha1.ProviderEKS, cellcastv1alpha1.ProviderGKE,
		cellcastv1alpha1.ProviderAKS, cellcastv1alpha1.ProviderGeneric:
	default:
		problems = append(problems, "provider must be one of eks, gke, aks, generic")
	}

	if reg.TrustConfigRef == "" {
		problems = append(problems, "trustConfigRef is required; a cell with no trust configuration can never be minted for")
	}

	switch cellcastv1alpha1.ClusterState(reg.State) {
	case "", cellcastv1alpha1.ClusterStateLive,
		cellcastv1alpha1.ClusterStateDark, cellcastv1alpha1.ClusterStateDraining:
	default:
		problems = append(problems, "state must be one of LIVE, DARK, DRAINING")
	}

	problems = append(problems, validateLabels(reg.Labels)...)

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validateEndpoint(endpoint string) []string {
	if endpoint == "" {
		return []string{"endpoint is required"}
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return []string{"endpoint is not a valid URL"}
	}

	var problems []string
	if u.Scheme != "https" {
		problems = append(problems, "endpoint must use https")
	}
	if u.Host == "" {
		problems = append(problems, "endpoint must include a host")
	}
	if u.User != nil {
		// Deliberately does not echo the URL back: the userinfo section is the
		// credential, and the rejection reason is logged (T-05).
		problems = append(problems, "endpoint must not embed credentials in the URL")
	}
	return problems
}

func validateCABundle(bundle []byte) []string {
	if len(bundle) == 0 {
		return nil
	}

	var certs int
	for rest := bundle; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return []string{"caBundle is not valid PEM"}
		}
		if block.Type == "CERTIFICATE" {
			certs++
		}
	}
	if certs == 0 {
		return []string{"caBundle contains no CERTIFICATE block"}
	}
	return nil
}

func validateLabels(in map[string]string) []string {
	var problems []string
	for key, value := range in {
		for _, msg := range validation.IsQualifiedName(key) {
			problems = append(problems, fmt.Sprintf("label key %q: %s", key, msg))
		}
		for _, msg := range validation.IsValidLabelValue(value) {
			problems = append(problems, fmt.Sprintf("label %q: %s", key, msg))
		}
		if isReservedLabel(key) {
			problems = append(problems, fmt.Sprintf("label key %q uses the reserved %s prefix", key, reservedLabelPrefix))
		}
	}
	return problems
}

// isReservedLabel reports whether key sits in cellcast's own label namespace.
func isReservedLabel(key string) bool {
	domain, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	return domain == reservedLabelPrefix || strings.HasSuffix(domain, "."+reservedLabelPrefix)
}

// toCluster converts a validated registration into the CRD that is actually
// stored. Nothing in the payload reaches the object except through this
// function, so a field cannot be persisted without appearing here.
func (reg *clusterRegistration) toCluster(namespace string) *cellcastv1alpha1.Cluster {
	state := cellcastv1alpha1.ClusterState(reg.State)
	if state == "" {
		state = cellcastv1alpha1.ClusterStateLive
	}

	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reg.Name,
			Namespace: namespace,
			Labels:    reg.Labels,
		},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       reg.Endpoint,
			CABundle:       reg.CABundle,
			Provider:       cellcastv1alpha1.Provider(reg.Provider),
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: reg.TrustConfigRef},
			State:          state,
		},
	}
}

func newClusterResponse(cl *cellcastv1alpha1.Cluster) clusterResponse {
	return clusterResponse{
		Name:           cl.Name,
		Endpoint:       cl.Spec.Endpoint,
		CABundle:       cl.Spec.CABundle,
		Provider:       string(cl.Spec.Provider),
		TrustConfigRef: cl.Spec.TrustConfigRef.Name,
		Labels:         cl.Labels,
		State:          string(cl.Spec.State),
		ObservedState:  string(cl.Status.ObservedState),
		CreatedAt:      cl.CreationTimestamp.Time,
	}
}

// handleRegisterCluster serves POST /api/v1/clusters.
//
// Registering a cell is a privileged action: whoever can do it can point
// cellcast at an endpoint they control and attract real deploys to it. The
// authoritative control is RBAC on the hub's own service account, not this
// handler (docs/threat-model.md T-04). The endpoint is deliberately not probed
// for reachability, because doing so would make the hub issue outbound requests
// to a URL the caller chose.
func (s *Server) handleRegisterCluster(w http.ResponseWriter, r *http.Request) {
	if s.k8s == nil {
		writeError(w, http.StatusServiceUnavailable, "registry unavailable")
		return
	}

	reg, err := decodeRegistration(w, r)
	if err == nil {
		err = reg.validate()
	}
	if err != nil {
		// The reason is built from field names and fixed strings, never from
		// payload values, so it is safe to log (T-05).
		s.log.WarnContext(r.Context(), "cluster registration rejected",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("reason", err.Error()),
		)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cl := reg.toCluster(s.cfg.Namespace)
	if err := s.k8s.Create(r.Context(), cl); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, "a cluster with that name is already registered")
			return
		}
		s.log.ErrorContext(r.Context(), "storing cluster registration failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("cluster", cl.Name),
			slog.Any("error", err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.log.InfoContext(r.Context(), "cluster registered",
		slog.String("request_id", requestIDFrom(r.Context())),
		slog.String("cluster", cl.Name),
		slog.String("provider", string(cl.Spec.Provider)),
		slog.String("state", string(cl.Spec.State)),
	)
	writeJSON(w, http.StatusCreated, newClusterResponse(cl))
}

// handleListClusters serves GET /api/v1/clusters.
//
// Filtering uses Kubernetes label selector syntax rather than a bespoke query
// language, so `?labelSelector=env=prd,group=devstacks` means exactly what the
// equivalent `kubectl get -l` means.
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if s.k8s == nil {
		writeError(w, http.StatusServiceUnavailable, "registry unavailable")
		return
	}

	selector := labels.Everything()
	if raw := r.URL.Query().Get("labelSelector"); raw != "" {
		parsed, err := labels.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "labelSelector is not a valid Kubernetes label selector")
			return
		}
		selector = parsed
	}

	var list cellcastv1alpha1.ClusterList
	err := s.k8s.List(r.Context(), &list,
		client.InNamespace(s.cfg.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	)
	if err != nil {
		s.log.ErrorContext(r.Context(), "listing clusters failed",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.Any("error", err),
		)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := make([]clusterResponse, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, newClusterResponse(&list.Items[i]))
	}

	// Wrapped in an object rather than returned as a bare array so that
	// pagination can be added without breaking every existing client.
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

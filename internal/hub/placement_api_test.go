package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
	"github.com/ethan-kane-ops/cellcast/internal/hub/broker"
	"github.com/ethan-kane-ops/cellcast/internal/hub/identity"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// mintedToken is a sentinel so a leak into any response is unmistakable.
const mintedToken = "SENTINEL-PLACEMENT-TOKEN-4f9a2c"

type stubPlacer struct {
	decision *placement.Decision
	err      error
	gotReq   placement.Request
	gotID    *identity.Identity
}

func (p *stubPlacer) Place(_ context.Context, id *identity.Identity, req placement.Request) (*placement.Decision, error) {
	p.gotID, p.gotReq = id, req
	if p.err != nil {
		return nil, p.err
	}
	return p.decision, nil
}

type stubMinter struct {
	err         error
	gotTTL      time.Duration
	gotSubject  string
	gotPolicy   *cellcastv1alpha1.TokenTTLPolicy
	resolution  broker.Resolution
	calledTimes int
}

func (m *stubMinter) Mint(
	_ context.Context,
	cluster *cellcastv1alpha1.Cluster,
	ttlPolicy *cellcastv1alpha1.TokenTTLPolicy,
	requested time.Duration,
	subject string,
) (*broker.Credential, broker.Resolution, error) {
	m.calledTimes++
	m.gotTTL, m.gotSubject, m.gotPolicy = requested, subject, ttlPolicy
	if m.err != nil {
		return nil, broker.Resolution{}, m.err
	}
	res := m.resolution
	if res.Granted == 0 {
		res = broker.Resolution{Granted: 10 * time.Minute, Default: 10 * time.Minute, Max: 15 * time.Minute}
	}
	return &broker.Credential{
		Token:          mintedToken,
		ExpiresAt:      time.Now().Add(res.Granted),
		Server:         cluster.Spec.Endpoint,
		CABundle:       []byte("ca"),
		Namespace:      "apps",
		ServiceAccount: "deployer",
	}, res, nil
}

// placementServer builds a server wired with the given placer and minter, and
// an authenticated caller already in context.
func placementServer(t *testing.T, placer Placer, minter Minter, cells ...client.Object) http.Handler {
	t.Helper()

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cells...).Build()

	opts := []Option{
		WithClusterClient(k8s),
		WithAuthenticator(fixedIdentity{}),
	}
	if placer != nil {
		opts = append(opts, WithPlacer(placer))
	}
	if minter != nil {
		opts = append(opts, WithMinter(minter))
	}
	return testServer(t, opts...).apiHandler()
}

// fixedIdentity authenticates every caller as the same pipeline.
type fixedIdentity struct{}

func (fixedIdentity) Authenticate(context.Context, *http.Request) (*identity.Identity, error) {
	return &identity.Identity{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: "repo:acme/app:ref:refs/heads/main",
		Claims:  map[string]string{"repository": "acme/app"},
	}, nil
}

func testDecision() *placement.Decision {
	return &placement.Decision{
		Cell:     "prod-euw1",
		Policy:   "app-prod",
		Strategy: cellcastv1alpha1.ScoringLeastLoaded,
		Candidates: []placement.Candidate{
			{Cell: "dev-euw1", Stage: placement.StagePermission, Reason: "cell is not permitted by this caller's policy"},
			{Cell: "prod-euw1", Admitted: true, Utilisation: 0.42},
			{Cell: "prod-euw2", Admitted: true, Utilisation: 0.77},
		},
	}
}

func registeredCell(name string) *cellcastv1alpha1.Cluster {
	return &cellcastv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{"env": "prod"}},
		Spec: cellcastv1alpha1.ClusterSpec{
			Endpoint:       "https://" + name + ".example.test",
			Provider:       cellcastv1alpha1.ProviderEKS,
			TrustConfigRef: cellcastv1alpha1.TrustConfigReference{Name: "trust"},
		},
	}
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/placement", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPlacementReturnsACredential(t *testing.T) {
	minter := &stubMinter{}
	h := placementServer(t, &stubPlacer{decision: testDecision()}, minter, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api","ttl":"12m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}

	var got placementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Cell != "prod-euw1" || got.Policy != "app-prod" {
		t.Errorf("cell/policy = %s/%s, want prod-euw1/app-prod", got.Cell, got.Policy)
	}
	if got.Credential == nil || got.Credential.Token != mintedToken {
		t.Fatalf("credential = %+v, want the minted token", got.Credential)
	}
	if got.Credential.Server != "https://prod-euw1.example.test" {
		t.Errorf("server = %q, want the chosen cell's endpoint", got.Credential.Server)
	}
	if minter.gotTTL != 12*time.Minute {
		t.Errorf("minter received ttl %s, want the requested 12m", minter.gotTTL)
	}
	if minter.gotSubject != "repo:acme/app:ref:refs/heads/main" {
		t.Errorf("minter received subject %q, want the caller's", minter.gotSubject)
	}
	// Candidates are not returned unless asked for: the table names cells this
	// caller cannot reach.
	if len(got.Candidates) != 0 {
		t.Errorf("candidates returned without --explain: %+v", got.Candidates)
	}
}

// TestDryRunNeverMints is the point of the flag. Everything else about the
// request runs, including policy and capacity, so the answer is the same one
// the real call would give.
func TestDryRunNeverMints(t *testing.T) {
	minter := &stubMinter{}
	placer := &stubPlacer{decision: testDecision()}
	h := placementServer(t, placer, minter, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api","dryRun":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}

	var got placementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if minter.calledTimes != 0 {
		t.Errorf("minter called %d times during a dry run, want 0", minter.calledTimes)
	}
	if got.Credential != nil {
		t.Errorf("dry run returned a credential: %+v", got.Credential)
	}
	if got.Cell != "prod-euw1" {
		t.Errorf("cell = %q, want the decision a real call would have made", got.Cell)
	}
	if strings.Contains(rec.Body.String(), mintedToken) {
		t.Error("dry run response contains token material")
	}
}

func TestExplainMarksTheWinnerAndTheRefusals(t *testing.T) {
	h := placementServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api","explain":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}

	var got placementResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Candidates) != 3 {
		t.Fatalf("candidates = %d, want every registered cell", len(got.Candidates))
	}

	byCell := map[string]candidateResponse{}
	for _, c := range got.Candidates {
		byCell[c.Cell] = c
	}
	if !byCell["prod-euw1"].Chosen {
		t.Error("the chosen cell is not marked chosen")
	}
	if byCell["prod-euw2"].Chosen {
		t.Error("a cell that was merely eligible is marked chosen")
	}
	if dev := byCell["dev-euw1"]; dev.Admitted || dev.Stage != placement.StagePermission {
		t.Errorf("dev-euw1 = %+v, want refused at the permission stage", dev)
	}
	// A refused cell must not carry a score. Publishing one would mean it had
	// been scored, which is the ordering bug the engine exists to prevent.
	if byCell["dev-euw1"].Utilisation != 0 {
		t.Errorf("a cell refused on permission has utilisation %v, want 0", byCell["dev-euw1"].Utilisation)
	}
}

func TestPlacementRefusals(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantReason string
	}{
		{"no policy matches", placement.ErrNoPolicy, http.StatusForbidden, ReasonNoPolicy},
		{"dark targeting refused", placement.ErrDarkNotPermitted, http.StatusForbidden, ReasonDarkNotPermitted},
		{"policy permits nothing", placement.ErrNoPermittedCells, http.StatusConflict, ReasonNoPermittedCells},
		{"everything is draining", placement.ErrNoEligibleCells, http.StatusServiceUnavailable, ReasonNoEligibleCells},
		{"capacity is unknown", placement.ErrCapacityUnknown, http.StatusServiceUnavailable, ReasonCapacityUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := placementServer(t, &stubPlacer{err: tt.err}, &stubMinter{})
			rec := post(t, h, `{"workload":"checkout-api"}`)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding refusal: %v", err)
			}
			if body["reason"] != tt.wantReason {
				t.Errorf("reason = %q, want %q", body["reason"], tt.wantReason)
			}
		})
	}
}

// TestAuthorizationRefusalsAreNotRetryable pins the status split ENG-175
// depends on. A caller told it may not reach a cell must not be told to wait
// and try again, and a fleet-wide capacity blackout must not look permanent.
func TestAuthorizationRefusalsAreNotRetryable(t *testing.T) {
	permanent := []error{placement.ErrNoPolicy, placement.ErrDarkNotPermitted, placement.ErrNoPermittedCells}
	transient := []error{placement.ErrNoEligibleCells, placement.ErrCapacityUnknown}

	for _, err := range permanent {
		h := placementServer(t, &stubPlacer{err: err}, &stubMinter{})
		if rec := post(t, h, `{"workload":"w"}`); rec.Code >= 500 {
			t.Errorf("%v returned %d, which a client reads as retryable", err, rec.Code)
		}
	}
	for _, err := range transient {
		h := placementServer(t, &stubPlacer{err: err}, &stubMinter{})
		if rec := post(t, h, `{"workload":"w"}`); rec.Code < 500 {
			t.Errorf("%v returned %d, which a client reads as permanent", err, rec.Code)
		}
	}
}

func TestPlacementBadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"no workload", `{"ttl":"10m"}`},
		{"unknown field", `{"workload":"w","cell":"prod-euw1"}`},
		{"unparseable ttl", `{"workload":"w","ttl":"soon"}`},
		{"negative ttl", `{"workload":"w","ttl":"-5m"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := placementServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))
			if rec := post(t, h, tt.body); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestUnknownFieldIsRejected covers the case that matters most in that table.
//
// A caller sending {"cell":"prod-euw1"} is trying to choose its own cell. If the
// decoder ignored unknown fields it would be told the request succeeded, and
// would reasonably believe it had been honoured.
func TestUnknownFieldIsRejected(t *testing.T) {
	h := placementServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"w","cell":"prod-euw1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestPlacementWithoutAMinter(t *testing.T) {
	h := placementServer(t, &stubPlacer{decision: testDecision()}, nil, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no broker is wired", rec.Code)
	}
	// Not a cell name with no way to reach it, which would read as success.
	if strings.Contains(rec.Body.String(), "prod-euw1") {
		t.Errorf("body names a cell it cannot mint for: %s", rec.Body)
	}
}

// TestMintFailureTellsTheCallerNothingUseful keeps target-cluster detail out of
// the response. A minting failure names service accounts and namespaces inside
// the cell, which the caller has no business learning from a failed request.
func TestMintFailureTellsTheCallerNothingUseful(t *testing.T) {
	minter := &stubMinter{err: broker.ErrTrustConfigMissing}
	h := placementServer(t, &stubPlacer{decision: testDecision()}, minter, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	for _, leak := range []string{"trust", "deployer", "apps"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), leak) {
			t.Errorf("response leaks target-cell detail %q: %s", leak, rec.Body)
		}
	}
}

// TestPlacementPassesThePolicyTTLToTheBroker pins the link between the two
// halves of the decision. The broker must be bounded by the same policy that
// authorised the placement.
func TestPlacementPassesThePolicyTTLToTheBroker(t *testing.T) {
	decision := testDecision()
	decision.TokenTTL = &cellcastv1alpha1.TokenTTLPolicy{
		Max: &metav1.Duration{Duration: 20 * time.Minute},
	}

	minter := &stubMinter{}
	h := placementServer(t, &stubPlacer{decision: decision}, minter, registeredCell("prod-euw1"))

	if rec := post(t, h, `{"workload":"checkout-api"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}
	if minter.gotPolicy == nil || minter.gotPolicy.Max.Duration != 20*time.Minute {
		t.Errorf("broker received ttl policy %+v, want the governing policy's", minter.gotPolicy)
	}
}

func TestPlacementRequiresAuthentication(t *testing.T) {
	srv := testServer(t, WithPlacer(&stubPlacer{decision: testDecision()}), WithMinter(&stubMinter{}))

	rec := post(t, srv.apiHandler(), `{"workload":"checkout-api"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 under the default deny-all authenticator", rec.Code)
	}
}

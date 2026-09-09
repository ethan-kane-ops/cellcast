package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ethan-kane-ops/cellcast/internal/hub/audit"
	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// trail captures the audit records a server writes.
type trail struct{ buf bytes.Buffer }

func (tr *trail) auditor() *audit.Auditor {
	return audit.New(slog.New(slog.NewJSONHandler(&tr.buf, nil)), nil)
}

// records returns the audit records, in order. Lines that are not audit records
// are skipped, so the assertions do not depend on what else shares the stream.
func (tr *trail) records(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any
	scan := bufio.NewScanner(bytes.NewReader(tr.buf.Bytes()))
	for scan.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			t.Fatalf("decoding a trail line: %v\n%s", err, scan.Text())
		}
		if rec["msg"] == audit.Msg {
			out = append(out, rec)
		}
	}
	return out
}

func (tr *trail) raw() string { return tr.buf.String() }

// auditedServer is placementServer with the trail captured.
func auditedServer(t *testing.T, placer Placer, minter Minter, cells ...client.Object) (http.Handler, *trail) {
	t.Helper()

	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cells...).Build()

	tr := &trail{}
	opts := []Option{
		WithClusterClient(k8s),
		WithAuthenticator(fixedIdentity{}),
		WithAuditor(tr.auditor()),
	}
	if placer != nil {
		opts = append(opts, WithPlacer(placer))
	}
	if minter != nil {
		opts = append(opts, WithMinter(minter))
	}
	return testServer(t, opts...).apiHandler(), tr
}

// only returns the single record of the given event, failing if there is not
// exactly one.
func only(t *testing.T, records []map[string]any, event string) map[string]any {
	t.Helper()

	var found []map[string]any
	for _, rec := range records {
		if rec["event"] == event {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d %s records, want exactly 1: %+v", len(found), event, records)
	}
	return found[0]
}

// TestAFullMintIsAuditedWithNoTokenMaterial checks the trail at the HTTP
// boundary. The same assertion runs against a real API server and a real token
// in TestLiveEndToEnd.
func TestAFullMintIsAuditedWithNoTokenMaterial(t *testing.T) {
	h, tr := auditedServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))

	rec := post(t, h, `{"workload":"checkout-api","ttl":"12m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}
	// The sentinel is in the response, so a trail containing it would be a real
	// leak rather than a test that could never fail.
	if !strings.Contains(rec.Body.String(), mintedToken) {
		t.Fatal("the response does not carry the minted token; this test cannot detect a leak")
	}

	if got := tr.raw(); strings.Contains(got, mintedToken) {
		t.Fatalf("the audit trail contains the minted token:\n%s", got)
	}

	records := tr.records(t)
	if len(records) != 2 {
		t.Fatalf("got %d records for one placement and mint, want 2: %+v", len(records), records)
	}

	placed, minted := only(t, records, "placement"), only(t, records, "mint")
	if placed["request_id"] == "" || placed["request_id"] != minted["request_id"] {
		t.Errorf("request_id %v and %v do not tie the two records together", placed["request_id"], minted["request_id"])
	}
	if placed["outcome"] != "granted" || minted["outcome"] != "granted" {
		t.Errorf("outcomes = %v and %v, want both granted", placed["outcome"], minted["outcome"])
	}
	if minted["token_sha256"] != audit.HashToken(mintedToken) {
		t.Errorf("token_sha256 = %v, want the digest of the token actually issued", minted["token_sha256"])
	}
	if minted["service_account"] != "deployer" || minted["namespace"] != "apps" {
		t.Errorf("mint record scope = %v/%v, want apps/deployer", minted["namespace"], minted["service_account"])
	}
	if minted["granted_ttl"] != "10m0s" || minted["requested_ttl"] != "12m0s" {
		t.Errorf("ttl granted/requested = %v/%v, want 10m0s/12m0s", minted["granted_ttl"], minted["requested_ttl"])
	}

	// The decision's reasoning is on the placement record and not repeated on
	// the mint one.
	if _, ok := placed["candidates"]; !ok {
		t.Error("the placement record carries no candidate table")
	}
	if _, ok := minted["candidates"]; ok {
		t.Error("the mint record repeats the candidate table")
	}
}

// TestEveryRefusalIsAudited walks each way the placement route can say no. A
// trail that only records what worked is not a trail, and the table is the only
// thing that keeps a new early return from skipping the record.
func TestEveryRefusalIsAudited(t *testing.T) {
	tests := []struct {
		name        string
		placer      Placer
		minter      Minter
		body        string
		wantEvent   string
		wantReason  refusal.Reason
		wantRecords int
	}{
		{
			name: "no placement engine", body: `{"workload":"checkout-api"}`,
			wantEvent: "placement", wantReason: refusal.PlacementUnavailable, wantRecords: 1,
		},
		{
			name: "malformed body", placer: &stubPlacer{decision: testDecision()}, body: `{`,
			wantEvent: "placement", wantReason: refusal.InvalidRequest, wantRecords: 1,
		},
		{
			name: "no workload", placer: &stubPlacer{decision: testDecision()}, body: `{"workload":""}`,
			wantEvent: "placement", wantReason: refusal.InvalidRequest, wantRecords: 1,
		},
		{
			name: "unparseable ttl", placer: &stubPlacer{decision: testDecision()}, body: `{"workload":"c","ttl":"soon"}`,
			wantEvent: "placement", wantReason: refusal.InvalidRequest, wantRecords: 1,
		},
		{
			name: "no policy matches", placer: &stubPlacer{err: placement.ErrNoPolicy}, body: `{"workload":"c"}`,
			wantEvent: "placement", wantReason: refusal.NoPolicy, wantRecords: 1,
		},
		{
			name: "every cell is draining", placer: &stubPlacer{err: placement.ErrNoEligibleCells}, body: `{"workload":"c"}`,
			wantEvent: "placement", wantReason: refusal.NoEligibleCells, wantRecords: 1,
		},
		{
			name:      "no broker",
			placer:    &stubPlacer{decision: testDecision()},
			body:      `{"workload":"checkout-api"}`,
			wantEvent: "mint", wantReason: refusal.MintUnavailable, wantRecords: 2,
		},
		{
			name:      "the chosen cell is gone",
			placer:    &stubPlacer{decision: testDecision()},
			minter:    &stubMinter{},
			body:      `{"workload":"checkout-api"}`,
			wantEvent: "mint", wantReason: refusal.MintFailed, wantRecords: 2,
		},
		{
			name:      "minting fails",
			placer:    &stubPlacer{decision: testDecision()},
			minter:    &stubMinter{err: errors.New("trust config not found")},
			body:      `{"workload":"checkout-api"}`,
			wantEvent: "mint", wantReason: refusal.MintFailed, wantRecords: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// "the chosen cell is gone" is the case with no registered cell, so
			// the cell is registered for every other case that gets that far.
			var cells []client.Object
			if tt.name != "the chosen cell is gone" {
				cells = append(cells, registeredCell("prod-euw1"))
			}
			h, tr := auditedServer(t, tt.placer, tt.minter, cells...)

			post(t, h, tt.body)

			records := tr.records(t)
			if len(records) != tt.wantRecords {
				t.Fatalf("got %d records, want %d: %+v", len(records), tt.wantRecords, records)
			}

			last := records[len(records)-1]
			if last["event"] != tt.wantEvent {
				t.Errorf("event = %v, want %s", last["event"], tt.wantEvent)
			}
			if last["outcome"] != "refused" {
				t.Errorf("outcome = %v, want refused", last["outcome"])
			}
			// The reason recorded is the same word the caller was given, so an
			// operator and a pipeline can talk to each other about one event.
			if last["reason"] != string(tt.wantReason) {
				t.Errorf("reason = %v, want %s", last["reason"], tt.wantReason)
			}
			if last["subject"] != "repo:acme/app:ref:refs/heads/main" {
				t.Errorf("subject = %v, want the refused caller recorded", last["subject"])
			}
		})
	}
}

// TestARefusedPlacementRecordsWhatWasConsidered pins the half of a refusal that
// answers "why not". The engine discards its candidate table on the error path,
// so it travels on placement.RefusedError.
func TestARefusedPlacementRecordsWhatWasConsidered(t *testing.T) {
	refused := &placement.RefusedError{
		Err:    placement.ErrCapacityUnknown,
		Policy: "app-prod",
		Candidates: []placement.Candidate{
			{Cell: "dev-euw1", Stage: placement.StagePermission, Reason: "cell is not permitted by this caller's policy"},
			{Cell: "prod-euw1", Stage: placement.StageCapacity, Reason: "capacity is stale; the cell is Unknown and excluded"},
		},
	}
	h, tr := auditedServer(t, &stubPlacer{err: refused}, &stubMinter{})

	if rec := post(t, h, `{"workload":"checkout-api"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST = %d, want 503", rec.Code)
	}

	got := only(t, tr.records(t), "placement")
	if got["policy"] != "app-prod" {
		t.Errorf("policy = %v, want the policy that permitted nothing", got["policy"])
	}
	candidates, ok := got["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("candidates = %v, want the two cells that were considered", got["candidates"])
	}
	stale, _ := candidates[1].(map[string]any)
	if stale["cell"] != "prod-euw1" || stale["stage"] != placement.StageCapacity {
		t.Errorf("second candidate = %v, want prod-euw1 refused at the capacity stage", stale)
	}
}

// TestADryRunIsNotRecordedAsADeploy keeps "which pipeline deployed here" from
// being answered with a list of pipelines that only asked.
func TestADryRunIsNotRecordedAsADeploy(t *testing.T) {
	h, tr := auditedServer(t, &stubPlacer{decision: testDecision()}, &stubMinter{}, registeredCell("prod-euw1"))

	if rec := post(t, h, `{"workload":"checkout-api","dryRun":true}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}

	records := tr.records(t)
	if len(records) != 1 {
		t.Fatalf("got %d records for a dry run, want 1: %+v", len(records), records)
	}
	if records[0]["outcome"] != "dry-run" {
		t.Errorf("outcome = %v, want dry-run", records[0]["outcome"])
	}
}

// TestTheAuditTrailSurvivesAQuietLogLevel is the property that stops the trail
// from being switchable. Audit records are emitted at Info, so a hub started at
// --log-level=error would lose all of them if the two shared a handler.
func TestTheAuditTrailSurvivesAQuietLogLevel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogLevel = "error"

	if NewLogger(cfg).Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("the ordinary logger admits Info at --log-level=error; this test proves nothing")
	}
	if !NewAuditLogger(cfg).Enabled(context.Background(), slog.LevelInfo) {
		t.Error("the audit trail is silenced by --log-level=error")
	}
}

// fakeRecorder captures the Kubernetes Events the hub would write.
type fakeRecorder struct{ events []string }

func (f *fakeRecorder) Eventf(
	regarding runtime.Object, _ runtime.Object,
	eventtype, reason, _, note string, args ...any,
) {
	obj, ok := regarding.(client.Object)
	if !ok {
		return
	}
	f.events = append(f.events, fmt.Sprintf("%s %s %s: %s",
		obj.GetName(), eventtype, reason, fmt.Sprintf(note, args...)))
}

func TestClusterEventMapping(t *testing.T) {
	tests := []struct {
		name       string
		rec        audit.Record
		wantReason string
		wantType   string
	}{
		{
			name:       "a placement is news about the cell",
			rec:        audit.Record{Event: audit.EventPlacement, Outcome: audit.OutcomeGranted, Cell: "prod-euw1"},
			wantReason: "Placed", wantType: corev1.EventTypeNormal,
		},
		{
			name:       "so is a credential",
			rec:        audit.Record{Event: audit.EventMint, Outcome: audit.OutcomeGranted, Cell: "prod-euw1"},
			wantReason: "CredentialIssued", wantType: corev1.EventTypeNormal,
		},
		{
			name:       "a failed mint is a warning about the cell, not the caller",
			rec:        audit.Record{Event: audit.EventMint, Outcome: audit.OutcomeRefused, Cell: "prod-euw1"},
			wantReason: "MintFailed", wantType: corev1.EventTypeWarning,
		},
		{
			name: "a refused placement names no cell to hang an event on",
			rec:  audit.Record{Event: audit.EventPlacement, Outcome: audit.OutcomeRefused},
		},
		{
			name: "a dry run changed nothing about the cell it named",
			rec:  audit.Record{Event: audit.EventPlacement, Outcome: audit.OutcomeDryRun, Cell: "prod-euw1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventType, reason, action, _, _ := clusterEvent(tt.rec)
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if eventType != tt.wantType {
				t.Errorf("type = %q, want %q", eventType, tt.wantType)
			}
			if reason != "" && action == "" {
				t.Error("an event with a reason and no action is rejected by the API server")
			}
		})
	}
}

func TestClusterEventsAttachToTheCellTheyConcern(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(registeredCell("prod-euw1")).Build()
	rec := &fakeRecorder{}
	events := &clusterEvents{
		recorder:  rec,
		reader:    k8s,
		namespace: testNamespace,
		log:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}

	events.Notify(t.Context(), audit.Record{
		Event: audit.EventMint, Outcome: audit.OutcomeGranted, Cell: "prod-euw1",
		Subject: "repo:acme/app", Namespace: "apps", ServiceAccount: "deployer",
	})
	// Neither of these concerns a cell that exists, and neither may produce an
	// event or an error.
	events.Notify(t.Context(), audit.Record{Event: audit.EventPlacement, Outcome: audit.OutcomeDryRun, Cell: "prod-euw1"})
	events.Notify(t.Context(), audit.Record{Event: audit.EventPlacement, Outcome: audit.OutcomeRefused})
	events.Notify(t.Context(), audit.Record{Event: audit.EventMint, Outcome: audit.OutcomeGranted, Cell: "decommissioned"})

	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want 1: %v", len(rec.events), rec.events)
	}
	want := "prod-euw1 Normal CredentialIssued: issued a 0s credential to repo:acme/app, scoped to apps/deployer"
	if rec.events[0] != want {
		t.Errorf("event =\n%s\nwant\n%s", rec.events[0], want)
	}
}

// TestTheEventsViewIsWiredOnlyWhenItCanWork guards the nil-in-an-interface
// mistake: a typed nil notifier passes the auditor's nil check and then panics
// on the first record.
func TestTheEventsViewIsWiredOnlyWhenItCanWork(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	tests := []struct {
		name string
		opts []Option
		want bool
	}{
		{name: "no recorder and no client", want: false},
		{name: "a client but no recorder", opts: []Option{WithClusterClient(k8s)}, want: false},
		{name: "a recorder but no client", opts: []Option{WithEventRecorder(&fakeRecorder{})}, want: false},
		{
			name: "both",
			opts: []Option{WithClusterClient(k8s), WithEventRecorder(&fakeRecorder{})},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
			srv, err := NewServer(DefaultConfig(), log, tt.opts...)
			if err != nil {
				t.Fatalf("NewServer() = %v, want nil", err)
			}
			if got := srv.clusterNotifier() != nil; got != tt.want {
				t.Errorf("clusterNotifier() != nil = %v, want %v", got, tt.want)
			}
		})
	}
}

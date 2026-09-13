package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ethan-kane-ops/cellcast/internal/hub/placement"
)

// stickyServer is placementServer with the audit trail captured.
func stickyServer(t *testing.T, placer Placer, minter Minter) (http.Handler, *trail) {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(registeredCell("prod-euw1")).Build()
	tr := &trail{}
	srv := testServer(t,
		WithClusterClient(k8s),
		WithAuthenticator(fixedIdentity{}),
		WithPlacer(placer),
		WithMinter(minter),
		WithAuditor(tr.auditor()),
	)
	return srv.apiHandler(), tr
}

func decodePlacement(t *testing.T, body []byte) placementResponse {
	t.Helper()
	var resp placementResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return resp
}

// TestOnlyAMintedPlacementIsRemembered: after the mint, so that a cell which
// could not issue a credential is not where a workload is kept, and never for
// a dry run, which changes nothing.
func TestOnlyAMintedPlacementIsRemembered(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		minter *stubMinter
		want   int
	}{
		{"a minted placement", `{"workload":"checkout-api"}`, &stubMinter{}, 1},
		{"a dry run", `{"workload":"checkout-api","dryRun":true}`, &stubMinter{}, 0},
		{"a mint that failed", `{"workload":"checkout-api"}`, &stubMinter{err: errors.New("the spoke is unreachable")}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			placer := &stubPlacer{decision: testDecision()}
			h, _ := stickyServer(t, placer, tt.minter)
			post(t, h, tt.body)
			if got := len(placer.remembered); got != tt.want {
				t.Errorf("Remember was called %d times, want %d", got, tt.want)
			}
		})
	}
}

// TestAPlacementThatCannotBeRememberedStillSucceeds: placement is advisory,
// and the whole cost of a write that failed is that the next placement is
// decided afresh.
func TestAPlacementThatCannotBeRememberedStillSucceeds(t *testing.T) {
	placer := &stubPlacer{decision: testDecision(), rememberErr: errors.New("the API server is unavailable")}
	h, _ := stickyServer(t, placer, &stubMinter{})

	rec := post(t, h, `{"workload":"checkout-api"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}
	if resp := decodePlacement(t, rec.Body.Bytes()); resp.Credential == nil || resp.Credential.Token == "" {
		t.Error("the credential was withheld because the placement could not be remembered")
	}
}

// TestAKeptPlacementSaysWhy: the response names the previous cell, the explain
// table marks the kept row with the stage that chose it, and the audit trail
// records it. A kept cell shown with only its utilisation would read as if it
// had won on its score.
func TestAKeptPlacementSaysWhy(t *testing.T) {
	d := testDecision()
	d.Previous = "prod-euw1"
	h, tr := stickyServer(t, &stubPlacer{decision: d}, &stubMinter{})

	rec := post(t, h, `{"workload":"checkout-api","explain":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d (%s), want 200", rec.Code, rec.Body)
	}
	resp := decodePlacement(t, rec.Body.Bytes())
	if resp.PreviousCell != "prod-euw1" {
		t.Errorf("previousCell = %q, want prod-euw1", resp.PreviousCell)
	}
	for _, c := range resp.Candidates {
		switch {
		case c.Cell == "prod-euw1":
			if !c.Chosen || c.Stage != placement.StageStickiness || c.Reason == "" {
				t.Errorf("the kept row = %+v, want it chosen at stage %s with a reason", c, placement.StageStickiness)
			}
		case c.Stage == placement.StageStickiness:
			t.Errorf("%s is marked %s and was not kept", c.Cell, c.Stage)
		}
	}

	records := tr.records(t)
	if len(records) == 0 {
		t.Fatal("nothing was audited")
	}
	for _, r := range records {
		if r["previous_cell"] != "prod-euw1" {
			t.Errorf("the %v record has previous_cell = %v, want prod-euw1", r["event"], r["previous_cell"])
		}
	}
}

// TestAMovedPlacementNamesWhereItWas: when the remembered cell could not take
// the workload, the response still says where it was. That is how a caller
// learns its service is now running in two cells.
func TestAMovedPlacementNamesWhereItWas(t *testing.T) {
	d := testDecision()
	d.Previous = "prod-euw2"
	h, _ := stickyServer(t, &stubPlacer{decision: d}, &stubMinter{})

	resp := decodePlacement(t, post(t, h, `{"workload":"checkout-api","explain":true}`).Body.Bytes())
	if resp.Cell != "prod-euw1" || resp.PreviousCell != "prod-euw2" {
		t.Errorf("cell = %q, previousCell = %q; want prod-euw1, moved from prod-euw2", resp.Cell, resp.PreviousCell)
	}
	for _, c := range resp.Candidates {
		if c.Stage == placement.StageStickiness {
			t.Errorf("%s is marked %s on a placement that moved", c.Cell, c.Stage)
		}
	}
}

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// emit writes rec through a JSON auditor and returns the decoded record.
func emit(t *testing.T, rec Record) (map[string]any, string) {
	t.Helper()

	var buf bytes.Buffer
	New(slog.New(slog.NewJSONHandler(&buf, nil)), nil).Record(t.Context(), rec)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decoding the audit line: %v\n%s", err, buf.String())
	}
	return got, buf.String()
}

func fullMint() Record {
	return Record{
		Event:          EventMint,
		Outcome:        OutcomeGranted,
		RequestID:      "8f2c1d4a",
		Issuer:         "https://token.actions.githubusercontent.com",
		Subject:        "repo:acme/app:ref:refs/heads/main",
		Claims:         map[string]string{"repository": "acme/app", "ref": "refs/heads/main"},
		Workload:       "checkout-api",
		RequestedTTL:   20 * time.Minute,
		Cell:           "prod-euw1",
		Policy:         "app-prod",
		Strategy:       "LeastLoaded",
		Confidence:     "high",
		Namespace:      "apps",
		ServiceAccount: "deployer",
		GrantedTTL:     15 * time.Minute,
		ExpiresAt:      time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		TokenSHA256:    HashToken("a-token"),
	}
}

// TestRecordAnswersTheSecurityReviewQuestion checks that a granted mint carries
// every field the ticket's opening question needs: who asked, from where, for
// what, against which policy, scoped to what, and for how long.
func TestRecordAnswersTheSecurityReviewQuestion(t *testing.T) {
	got, raw := emit(t, fullMint())

	want := map[string]any{
		"msg":             Msg,
		"event":           "mint",
		"outcome":         "granted",
		"request_id":      "8f2c1d4a",
		"issuer":          "https://token.actions.githubusercontent.com",
		"subject":         "repo:acme/app:ref:refs/heads/main",
		"workload":        "checkout-api",
		"requested_ttl":   "20m0s",
		"cell":            "prod-euw1",
		"policy":          "app-prod",
		"strategy":        "LeastLoaded",
		"confidence":      "high",
		"namespace":       "apps",
		"service_account": "deployer",
		"granted_ttl":     "15m0s",
		"token_sha256":    HashToken("a-token"),
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v\n%s", key, got[key], value, raw)
		}
	}

	claims, ok := got["claims"].(map[string]any)
	if !ok {
		t.Fatalf("claims = %T, want an object\n%s", got["claims"], raw)
	}
	if claims["repository"] != "acme/app" {
		t.Errorf("claims.repository = %v, want acme/app", claims["repository"])
	}
	if got["expires_at"] != "2026-09-08T12:00:00Z" {
		t.Errorf("expires_at = %v, want an RFC3339 timestamp", got["expires_at"])
	}
	if _, ok := got["time"]; !ok {
		t.Errorf("no time on the record\n%s", raw)
	}
}

// TestRefusalIsRecordedWithTheSameRigour pins the half that is usually missing.
// A refusal carries the caller, the request, the policy that governed it and the
// candidate table; it carries none of the mint fields, because no mint happened.
func TestRefusalIsRecordedWithTheSameRigour(t *testing.T) {
	got, raw := emit(t, Record{
		Event:     EventPlacement,
		Outcome:   OutcomeRefused,
		RequestID: "8f2c1d4a",
		Issuer:    "https://token.actions.githubusercontent.com",
		Subject:   "repo:acme/app:ref:refs/heads/main",
		Workload:  "checkout-api",
		Policy:    "app-prod",
		Reason:    refusal.NoEligibleCells,
		Error:     "no permitted cell is currently accepting placements: policy app-prod",
		Candidates: []Candidate{
			{Cell: "prod-euw1", Stage: "state", Reason: "cell is DRAINING"},
			{Cell: "prod-euw2", Admitted: true, Utilisation: 0.42},
		},
	})

	if got["reason"] != string(refusal.NoEligibleCells) {
		t.Errorf("reason = %v, want %s\n%s", got["reason"], refusal.NoEligibleCells, raw)
	}
	if got["policy"] != "app-prod" {
		t.Errorf("policy = %v, want app-prod on a refusal that got past policy selection", got["policy"])
	}

	candidates, ok := got["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("candidates = %v, want two entries\n%s", got["candidates"], raw)
	}
	first, _ := candidates[0].(map[string]any)
	if first["cell"] != "prod-euw1" || first["stage"] != "state" {
		t.Errorf("first candidate = %v, want prod-euw1 refused at the state stage", first)
	}

	// A refusal must not carry the shape of a mint that never happened.
	for _, absent := range []string{"token_sha256", "granted_ttl", "expires_at", "service_account", "namespace"} {
		if _, ok := got[absent]; ok {
			t.Errorf("refusal carries %s = %v, want it omitted\n%s", absent, got[absent], raw)
		}
	}
}

// TestRecordHasNoFieldThatCouldHoldAToken is the guard that outlives everyone
// who read the ticket.
//
// The trail's whole value rests on it holding no credential material, and the
// way that gets broken is somebody adding a field to Record for a reason that
// looks good at the time. Every field is classified here once. A new one fails
// this test until it has been thought about.
func TestRecordHasNoFieldThatCouldHoldAToken(t *testing.T) {
	// Why each field cannot carry credential material.
	classified := map[string]string{
		"Event":          "a constant from this package",
		"Outcome":        "a constant from this package",
		"RequestID":      "hub-generated correlation id",
		"Issuer":         "verified iss claim",
		"Subject":        "verified sub claim",
		"Claims":         "the allowlisted flat claims oidc extracts, never the raw token",
		"Workload":       "caller-supplied name, recorded not interpreted",
		"TargetedDark":   "a bool",
		"RequestedTTL":   "a duration",
		"Cell":           "a registered Cluster name",
		"Policy":         "a PlacementPolicy name",
		"Strategy":       "a scoring strategy name",
		"Confidence":     "a confidence level",
		"Candidates":     "cell names, filter stages and utilisation",
		"Namespace":      "the scope the credential was issued against",
		"ServiceAccount": "the identity the credential acts as",
		"GrantedTTL":     "a duration",
		"ExpiresAt":      "a timestamp",
		"TokenSHA256":    "a digest, produced only by HashToken",
		"Reason":         "a refusal code",
		"Error":          "an internal error string from the hub, never a response body",
	}

	rt := reflect.TypeOf(Record{})
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if _, ok := classified[name]; !ok {
			t.Errorf("Record.%s is not classified: add it above with why it cannot carry credential material, "+
				"or do not put it on the audit record", name)
		}
		delete(classified, name)
	}
	for name := range classified {
		t.Errorf("Record.%s is classified but no longer exists; drop the entry", name)
	}
}

func TestHashToken(t *testing.T) {
	const token = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImtleS0xIn0.payload.signature"

	got := HashToken(token)
	if got != HashToken(token) {
		t.Error("HashToken is not stable; the digest cannot correlate anything")
	}
	if got == HashToken(token+"x") {
		t.Error("two different tokens share a digest")
	}
	if HashToken("") != "" {
		t.Errorf(`HashToken("") = %q, want empty so an unminted record carries no field`, HashToken(""))
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("HashToken() = %q, want the algorithm labelled", got)
	}

	// The point of the whole exercise: the digest is not the token, and it does
	// not contain any run of the token either.
	digest := strings.TrimPrefix(got, "sha256:")
	for _, segment := range strings.Split(token, ".") {
		for n := 8; n <= len(segment); n += 8 {
			if strings.Contains(digest, segment[:n]) {
				t.Fatalf("the digest contains %d bytes of the token", n)
			}
		}
	}
}

// TestNotifierSeesEveryRecord pins that the second view is not a filtered one.
// Deciding which records deserve an Event is the notifier's job, so that the
// decision lives in one place and the log is never the thing that drops one.
func TestNotifierSeesEveryRecord(t *testing.T) {
	seen := &countingNotifier{}
	a := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), seen)

	a.Record(t.Context(), Record{Event: EventPlacement, Outcome: OutcomeRefused})
	a.Record(t.Context(), Record{Event: EventPlacement, Outcome: OutcomeDryRun})
	a.Record(t.Context(), Record{Event: EventMint, Outcome: OutcomeGranted})

	if len(seen.records) != 3 {
		t.Fatalf("notifier saw %d records, want all 3", len(seen.records))
	}
	if seen.records[0].Outcome != OutcomeRefused {
		t.Errorf("first record = %s, want the refusal to reach the notifier too", seen.records[0].Outcome)
	}
}

type countingNotifier struct{ records []Record }

func (c *countingNotifier) Notify(_ context.Context, rec Record) {
	c.records = append(c.records, rec)
}

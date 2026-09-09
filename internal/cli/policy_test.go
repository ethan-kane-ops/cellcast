package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// runPolicyTestCmd runs `cellcast policy test` against a stub hub and returns
// everything it wrote.
func runPolicyTestCmd(t *testing.T, hub string, args ...string) (string, error) {
	t.Helper()
	t.Setenv(tokenEnv, "caller-token")

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"policy", "test", "--hub", hub, "--workload", "checkout-api"}, args...))

	err := cmd.Execute()
	return out.String(), err
}

// immutableSubject is the format every repository created after 15 July 2026
// gets, and the one a policy written against the documented older form does not
// match. It is the exact failure this command exists to make visible.
const immutableSubject = "repo:acme@56138094/app@1332281435:pull_request"

func refusedBody(reason refusal.Reason) map[string]any {
	return map[string]any{
		"reason": string(reason),
		"error":  "no placement policy permits this caller",
		"identity": map[string]any{
			"issuer":  "https://token.actions.githubusercontent.com",
			"subject": immutableSubject,
			"claims":  map[string]string{"repository": "acme/app", "ref": "refs/heads/main"},
		},
	}
}

func TestPolicyTestShowsTheSubjectTheHubActuallyRead(t *testing.T) {
	// The whole point. A policy pinned to repo:acme/app:pull_request refuses a
	// caller presenting the immutable form, and the refusal on its own says
	// only NoPolicy. Reading the subject back is the difference between a
	// one-character fix and an afternoon.
	srv := hubStub(t, http.StatusForbidden, refusedBody(refusal.NoPolicy))
	out, err := runPolicyTestCmd(t, srv.URL)

	if err == nil {
		t.Error("a refusal exited zero; in a pipeline that is a check that never fails")
	}
	for _, want := range []string{
		"refused: NoPolicy",
		immutableSubject,
		"https://token.actions.githubusercontent.com",
		"repository=acme/app",
		"kubectl",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestPolicyTestSortsTheClaimsItPrints(t *testing.T) {
	// Two runs of this command get compared by eye, or piped through diff. Go
	// iterates a map in a different order every time, which would make the same
	// answer look like a different one.
	srv := hubStub(t, http.StatusForbidden, refusedBody(refusal.NoPolicy))
	out, _ := runPolicyTestCmd(t, srv.URL)

	ref, repository := strings.Index(out, "ref=refs/heads/main"), strings.Index(out, "repository=acme/app")
	if ref < 0 || repository < 0 {
		t.Fatalf("output is missing a claim:\n%s", out)
	}
	if ref > repository {
		t.Errorf("claims are printed in map order, so two runs cannot be diffed:\n%s", out)
	}
}

func TestPolicyTestReportsAnAdmittedCaller(t *testing.T) {
	body := successBody("prod-euw1")
	delete(body, "credential")
	body["dryRun"] = true
	body["identity"] = map[string]any{
		"issuer":  "https://token.actions.githubusercontent.com",
		"subject": immutableSubject,
	}

	srv := hubStub(t, http.StatusOK, body)
	out, err := runPolicyTestCmd(t, srv.URL)
	if err != nil {
		t.Fatalf("a permitted caller failed: %v\n%s", err, out)
	}

	for _, want := range []string{"admitted by policy app-prod", "prod-euw1", immutableSubject} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestPolicyTestNeverMints(t *testing.T) {
	// It is run repeatedly while a policy is being edited. Minting on each run
	// would fill the audit trail with deploys that never happened, and hand out
	// credentials to somebody who asked a question.
	var got placeRequest
	srv := recordingHub(t, &got, successBody("prod-euw1"))

	if _, err := runPolicyTestCmd(t, srv.URL); err != nil {
		t.Fatalf("policy test failed: %v", err)
	}
	if !got.DryRun {
		t.Error("policy test asked the hub for a credential")
	}
}

// recordingHub answers with body and records the request it was sent.
func recordingHub(t *testing.T, into *placeRequest, body any) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(into); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encoding stub response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

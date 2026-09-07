package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tokenFile writes a token to a temporary file and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}
	return path
}

func testClient(t *testing.T, endpoint, tokenPath string) *HubClient {
	t.Helper()
	cfg := DefaultConfig()
	cfg.HubEndpoint = endpoint
	cfg.CellName = "prod-euw1"
	cfg.TokenPath = tokenPath

	client, err := NewHubClient(cfg)
	if err != nil {
		t.Fatalf("NewHubClient() = %v", err)
	}
	return client
}

func TestHubClientReport(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotMethod string
		gotBody   map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding report: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, tokenFile(t, "projected-token\n"))
	snapshot := Snapshot{
		Nodes: 3, CPUMilliAllocatable: 12000, CPUMilliCommitted: 2400,
		MemoryBytesAllocatable: 24 << 30, MemoryBytesCommitted: 6 << 30,
		Pods: 42, PodCapacity: 330,
	}

	if err := client.Report(t.Context(), snapshot); err != nil {
		t.Fatalf("Report() = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/api/v1/clusters/prod-euw1/capacity"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	// Trailing whitespace in the mounted file must not reach the header.
	if want := "Bearer projected-token"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if got := gotBody["cpuMilliCommitted"]; got != float64(2400) {
		t.Errorf("cpuMilliCommitted = %v, want 2400", got)
	}
	if _, ok := gotBody["cell"]; ok {
		// The cell is in the path. A body that also named one would be a body
		// that could name a different one.
		t.Error("report body carries a cell field; the hub takes the cell from the path")
	}
}

// TestHubClientRereadsTokenEveryReport pins the rotation behaviour. A cached
// token works until the kubelet rewrites the file, then fails permanently.
func TestHubClientRereadsTokenEveryReport(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	path := tokenFile(t, "first")
	client := testClient(t, srv.URL, path)

	if err := client.Report(t.Context(), Snapshot{}); err != nil {
		t.Fatalf("Report() = %v", err)
	}
	if err := os.WriteFile(path, []byte("rotated"), 0o600); err != nil {
		t.Fatalf("rotating token: %v", err)
	}
	if err := client.Report(t.Context(), Snapshot{}); err != nil {
		t.Fatalf("Report() after rotation = %v", err)
	}

	want := []string{"Bearer first", "Bearer rotated"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("Authorization headers = %q, want %q", seen, want)
	}
}

func TestHubClientRefusals(t *testing.T) {
	tests := []struct {
		name             string
		status           int
		body             string
		wantMessage      string
		wantMisconfigure bool
	}{
		{
			name:             "not the declared reporter",
			status:           http.StatusForbidden,
			body:             `{"error":"caller is not the declared reporter for this cell"}`,
			wantMessage:      "caller is not the declared reporter for this cell",
			wantMisconfigure: true,
		},
		{
			name:             "cell not registered",
			status:           http.StatusNotFound,
			body:             `{"error":"no such cluster"}`,
			wantMessage:      "no such cluster",
			wantMisconfigure: true,
		},
		{
			name:        "hub is starting",
			status:      http.StatusServiceUnavailable,
			body:        `{"error":"registry unavailable"}`,
			wantMessage: "registry unavailable",
		},
		{
			name:        "a proxy answered instead",
			status:      http.StatusBadGateway,
			body:        "<html>gateway</html>",
			wantMessage: "<html>gateway</html>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := testClient(t, srv.URL, tokenFile(t, "token"))
			err := client.Report(t.Context(), Snapshot{})

			var hubErr *HubError
			if !errors.As(err, &hubErr) {
				t.Fatalf("Report() = %v, want a *HubError", err)
			}
			if hubErr.Status != tt.status {
				t.Errorf("Status = %d, want %d", hubErr.Status, tt.status)
			}
			if hubErr.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", hubErr.Message, tt.wantMessage)
			}
			if hubErr.Misconfigured() != tt.wantMisconfigure {
				t.Errorf("Misconfigured() = %v, want %v", hubErr.Misconfigured(), tt.wantMisconfigure)
			}
		})
	}
}

func TestHubClientMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the hub was contacted without a token")
	}))
	defer srv.Close()

	client := testClient(t, srv.URL, filepath.Join(t.TempDir(), "absent"))
	if err := client.Report(t.Context(), Snapshot{}); err == nil {
		t.Fatal("Report() = nil, want an error when the token cannot be read")
	}
}

func TestHubClientEmptyToken(t *testing.T) {
	client := testClient(t, "https://hub.example.test", tokenFile(t, "   \n"))
	err := client.Report(context.Background(), Snapshot{})
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("Report() = %v, want an empty-token error", err)
	}
}

// TestNewHubClientRejectsCredentialsInEndpoint keeps a password out of every
// connection error the reporting loop logs (docs/threat-model.md T-05).
func TestNewHubClientRejectsCredentialsInEndpoint(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HubEndpoint = "https://user:hunter2@hub.example.test"
	cfg.CellName = "prod-euw1"

	_, err := NewHubClient(cfg)
	if err == nil {
		t.Fatal("NewHubClient() = nil, want an error for an endpoint embedding credentials")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoes the credential it rejected: %v", err)
	}
}

func TestNewHubClientRejectsUnusableCABundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("writing bundle: %v", err)
	}

	cfg := DefaultConfig()
	cfg.HubEndpoint = "https://hub.example.test"
	cfg.CellName = "prod-euw1"
	cfg.HubCAFile = path

	if _, err := NewHubClient(cfg); err == nil {
		t.Fatal("NewHubClient() = nil, want an error for a bundle with no certificate")
	}
}

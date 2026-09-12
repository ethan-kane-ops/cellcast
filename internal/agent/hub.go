package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// maxErrorBytes bounds how much of a hub error response is read back. The hub's
// errors are one short JSON object; anything larger is a proxy or a captive
// portal, and reading it into a log line helps nobody.
const maxErrorBytes = 4 << 10

// HubError is a report the hub refused.
//
// The status is kept because the two families need different operator
// responses: a 4xx is a misconfiguration that will still be there on the next
// heartbeat, a 5xx clears on its own.
type HubError struct {
	Status  int
	Message string
}

func (e *HubError) Error() string {
	return fmt.Sprintf("hub refused the report: %d %s", e.Status, e.Message)
}

// Misconfigured reports whether retrying unchanged is pointless.
//
// The agent retries anyway, because the fix is usually an operator registering
// the cell or correcting `spec.reporter` while the agent is running, and an
// agent that gave up would then need a restart nobody knows to perform. What
// this changes is the log level: a misconfiguration is reported at error on
// every attempt, because it is the one failure that will never resolve itself.
func (e *HubError) Misconfigured() bool {
	return e.Status >= 400 && e.Status < 500
}

// HubClient publishes capacity reports to the hub.
type HubClient struct {
	url        string
	tokenPath  string
	httpClient *http.Client

	// tracerProvider starts the span for each report. A no-op unless Run was
	// handed a real one.
	tracerProvider trace.TracerProvider
}

// NewHubClient builds the client the reporting loop publishes through.
func NewHubClient(cfg Config) (*HubClient, error) {
	base, err := url.Parse(cfg.HubEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing hub-endpoint: %w", err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("hub-endpoint %q must include a host", cfg.HubEndpoint)
	}
	if base.User != nil {
		// Refused rather than stripped. Every transport error the loop logs
		// renders this URL, so a password embedded here would end up in the
		// agent's own logs on the first hub outage (docs/threat-model.md T-05).
		// The endpoint does not carry the agent's identity in any case; the
		// projected token does.
		return nil, errors.New("hub-endpoint must not embed credentials in the URL")
	}

	transport, err := hubTransport(cfg.HubCAFile)
	if err != nil {
		return nil, err
	}

	return &HubClient{
		url: base.JoinPath("api", "v1", "clusters", cfg.CellName, "capacity").String(),
		// The path is built from the parsed URL rather than by string
		// concatenation, so a cell name is escaped rather than able to walk out
		// of the path it belongs in.
		tokenPath:      cfg.TokenPath,
		httpClient:     &http.Client{Transport: transport},
		tracerProvider: noop.NewTracerProvider(),
	}, nil
}

func hubTransport(caFile string) (http.RoundTripper, error) {
	if caFile == "" {
		return http.DefaultTransport, nil
	}

	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading hub-ca-file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("hub-ca-file %s contains no usable certificate", caFile)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return transport, nil
}

// Report publishes one heartbeat.
func (c *HubClient) Report(ctx context.Context, s Snapshot) error {
	ctx, span := startReport(ctx, c.tracerProvider)
	err := c.report(ctx, s)
	endReport(span, err)
	return err
}

// report is Report inside its span.
func (c *HubClient) report(ctx context.Context, s Snapshot) error {
	token, err := c.token()
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"nodes":                  s.Nodes,
		"cpuMilliAllocatable":    s.CPUMilliAllocatable,
		"cpuMilliCommitted":      s.CPUMilliCommitted,
		"memoryBytesAllocatable": s.MemoryBytesAllocatable,
		"memoryBytesCommitted":   s.MemoryBytesCommitted,
		"pods":                   s.Pods,
		"podCapacity":            s.PodCapacity,
	})
	if err != nil {
		return fmt.Errorf("encoding capacity report: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building capacity request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	injectTrace(ctx, req.Header)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Safe to wrap whole: http.Client errors render the request URL, and
		// NewHubClient has already refused an endpoint carrying credentials.
		return fmt.Errorf("posting capacity report: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body on a response we are done with

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HubError{Status: resp.StatusCode, Message: hubErrorMessage(resp.Body)}
	}

	// The response body echoes what the hub derived from the report. Discarded
	// rather than parsed: nothing here acts on it, and draining it is what lets
	// the connection be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
	return nil
}

// token reads the projected ServiceAccount token.
//
// Read on every heartbeat, never cached. The kubelet rewrites this file when
// the token approaches expiry, and an agent holding the first one it ever read
// works perfectly until the token expires and then fails permanently, which is
// the kind of bug that surfaces an hour into a demo rather than in a test.
func (c *HubClient) token() (string, error) {
	raw, err := os.ReadFile(c.tokenPath)
	if err != nil {
		// os.ReadFile puts only the path in its error, never the contents.
		return "", fmt.Errorf("reading service account token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("service account token at %s is empty", c.tokenPath)
	}
	return token, nil
}

// hubErrorMessage extracts the hub's error text from a refusal.
func hubErrorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBytes))
	if err != nil || len(raw) == 0 {
		return "no response body"
	}

	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && out.Error != "" {
		return out.Error
	}
	return strings.TrimSpace(string(raw))
}

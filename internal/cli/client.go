package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// ErrNoToken means no caller identity token was supplied.
var ErrNoToken = errors.New("no caller token: set CELLCAST_TOKEN or pass --token-file")

// tokenEnv is the environment variable holding the caller's identity token.
//
// An environment variable and a file are the only two ways in. There is
// deliberately no --token flag: a token in a flag is a token in the process
// table, readable by every other user on a shared CI runner
// (docs/threat-model.md T-05).
const tokenEnv = "CELLCAST_TOKEN"

// Candidate is one cell's journey through the filter, as the hub reported it.
type Candidate struct {
	Cell        string  `json:"cell"`
	Admitted    bool    `json:"admitted"`
	Stage       string  `json:"stage"`
	Reason      string  `json:"reason"`
	Utilisation float64 `json:"utilisation"`
	Chosen      bool    `json:"chosen"`
}

// Credential is a minted credential for the chosen cell.
type Credential struct {
	Server                   string    `json:"server"`
	CertificateAuthorityData string    `json:"certificateAuthorityData"`
	Namespace                string    `json:"namespace"`
	ServiceAccount           string    `json:"serviceAccount"`
	Token                    string    `json:"token"`
	ExpiresAt                time.Time `json:"expiresAt"`
}

// TTL is what lifetime the hub granted.
type TTL struct {
	Granted   string `json:"granted"`
	Default   string `json:"default"`
	Max       string `json:"max"`
	Requested string `json:"requested"`
	Clamped   bool   `json:"clamped"`
}

// Placement is a decision, and the credential when one was minted.
type Placement struct {
	Cell     string `json:"cell"`
	Policy   string `json:"policy"`
	Strategy string `json:"strategy"`
	// Confidence is how much of the permitted fleet the hub could see. A
	// decision that did not come from the hub carries its own value here; see
	// the confidence constants.
	Confidence string `json:"confidence,omitempty"`
	// DecidedFor is the subject the hub resolved the caller to.
	DecidedFor   string      `json:"decidedFor,omitempty"`
	TargetedDark bool        `json:"targetedDark"`
	DryRun       bool        `json:"dryRun"`
	Credential   *Credential `json:"credential"`
	TTL          *TTL        `json:"ttl"`
	Candidates   []Candidate `json:"candidates"`
}

// Refusal is a placement the hub declined.
type Refusal struct {
	Status int
	// Reason is the machine-readable code. Match on this, never on Message.
	Reason  refusal.Reason `json:"reason"`
	Message string         `json:"error"`
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: %s", r.Reason, r.Message)
}

// placeRequest is the body sent to the hub.
type placeRequest struct {
	Workload   string `json:"workload"`
	TargetDark bool   `json:"targetDark,omitempty"`
	TTL        string `json:"ttl,omitempty"`
	DryRun     bool   `json:"dryRun,omitempty"`
	Explain    bool   `json:"explain,omitempty"`
}

// Client talks to a cellcast hub.
type Client struct {
	hub   string
	token string
	http  *http.Client
}

// NewClient builds a hub client.
//
// tokenFile wins over the environment when both are set, because a file is the
// explicit choice and the variable is often inherited from a parent process
// nobody remembered was there.
func NewClient(hub, tokenFile string, timeout time.Duration) (*Client, error) {
	if hub == "" {
		return nil, errors.New("hub address must be set: pass --hub or set CELLCAST_HUB")
	}

	token := os.Getenv(tokenEnv)
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			// The path, never the contents.
			return nil, fmt.Errorf("reading token file %s: %w", tokenFile, err)
		}
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		return nil, ErrNoToken
	}

	return &Client{
		hub:   strings.TrimRight(hub, "/"),
		token: token,
		http:  &http.Client{Timeout: timeout},
	}, nil
}

// Hub is the normalised hub address this client talks to. The decision cache
// keys on it, so it has to be the value after normalisation rather than the
// flag as typed.
func (c *Client) Hub() string { return c.hub }

// Place asks the hub where to deploy, and for a credential unless dryRun.
func (c *Client) Place(ctx context.Context, req placeRequest) (*Placement, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.hub+"/api/v1/placement", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// net/http puts the URL in this error but never the headers, so the
		// bearer token cannot reach a log through it.
		return nil, fmt.Errorf("calling the hub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		ref := &Refusal{Status: resp.StatusCode}
		if err := json.NewDecoder(resp.Body).Decode(ref); err != nil || ref.Reason == "" {
			// A proxy in front of the hub answers in HTML, and a hub that fell
			// over answers not at all. Both land here, and neither is a
			// classifiable refusal.
			ref.Reason = refusal.Unknown
			ref.Message = fmt.Sprintf("hub returned %s", resp.Status)
		}
		return nil, ref
	}

	var out Placement
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &out, nil
}

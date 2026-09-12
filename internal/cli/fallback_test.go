package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

func TestParseStance(t *testing.T) {
	tests := []struct {
		name          string
		value         string
		wantCell      string
		wantLastKnown bool
		wantErr       bool
	}{
		{name: "the default is to fail", value: "", wantCell: ""},
		{name: "fail is explicit", value: "fail"},
		{name: "last-known reads the cache", value: "last-known", wantLastKnown: true},
		{name: "anything else is a cell", value: "prod-euw3", wantCell: "prod-euw3"},
		{name: "a cell may be a subdomain", value: "eu.west-1.prod", wantCell: "eu.west-1.prod"},

		// The reserved words are one character away from a cell name, and the
		// failure they would otherwise produce is a pin to a cell that does not
		// exist, found during an outage.
		{name: "a near miss on last-known is not a cell", value: "lastknown", wantCell: "lastknown"},
		{name: "an underscore is not a cell name", value: "last_known", wantErr: true},
		{name: "uppercase is not a cell name", value: "Prod-EUW3", wantErr: true},
		{name: "a path is not a cell name", value: "../../etc/passwd", wantErr: true},
		{name: "whitespace is not a cell name", value: "prod euw3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStance(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseStance(%q) = %+v, want an error", tt.value, got)
				}
				// The message has to name the alternatives, or a typo sends the
				// reader to the source to find out what is allowed.
				for _, want := range []string{stanceFail, stanceLastKnown} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to mention %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStance(%q) = %v, want nil", tt.value, err)
			}
			if got.cell != tt.wantCell || got.lastKnown != tt.wantLastKnown {
				t.Errorf("parseStance(%q) = %+v, want cell %q lastKnown %v",
					tt.value, got, tt.wantCell, tt.wantLastKnown)
			}
			if wantsFallback := tt.wantCell != "" || tt.wantLastKnown; got.fallsBack() != wantsFallback {
				t.Errorf("fallsBack() = %v, want %v", got.fallsBack(), wantsFallback)
			}
		})
	}
}

// TestFallbackAllowed is the fail-closed-on-authorization control. Every entry
// here is a decision about whether a caller may deploy after the hub declined
// to say where, so the table is written out in full rather than derived.
func TestFallbackAllowed(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// Nothing answered, so nothing was refused.
			name: "the hub is unreachable",
			err:  fmt.Errorf("calling the hub: dial tcp 127.0.0.1:8080: connect: connection refused"),
			want: true,
		},

		// Authorization. A stance must not buy a way past any of these.
		{name: "no policy matches", err: &Refusal{Status: 403, Reason: refusal.NoPolicy}},
		{name: "dark targeting refused", err: &Refusal{Status: 403, Reason: refusal.DarkNotPermitted}},
		{name: "the policy permits no cell", err: &Refusal{Status: 409, Reason: refusal.NoPermittedCells}},
		{name: "the request was malformed", err: &Refusal{Status: 400, Reason: refusal.InvalidRequest}},

		// Optimisation. The caller is permitted and the hub cannot rank.
		{name: "everything is draining", err: &Refusal{Status: 503, Reason: refusal.NoEligibleCells}, want: true},
		{name: "the fleet stopped reporting", err: &Refusal{Status: 503, Reason: refusal.CapacityUnknown}, want: true},
		{name: "the hub cannot decide", err: &Refusal{Status: 503, Reason: refusal.PlacementUnavailable}, want: true},

		// Minting. Both are 503s and neither may be softened.
		{name: "the broker is unavailable", err: &Refusal{Status: 503, Reason: refusal.MintUnavailable}},
		{name: "the mint failed", err: &Refusal{Status: 503, Reason: refusal.MintFailed}},

		// Something that is not the hub answered.
		{name: "a gateway could not reach the hub", err: &Refusal{Status: 502, Reason: refusal.Unknown}, want: true},
		{name: "a gateway timed out", err: &Refusal{Status: 504, Reason: refusal.Unknown}, want: true},
		{name: "a bare 503 from a proxy", err: &Refusal{Status: 503, Reason: refusal.Unknown}, want: true},
		{name: "a proxy's own rate limit", err: &Refusal{Status: 429, Reason: refusal.Unknown}, want: true},
		{name: "the hub's rate limit", err: &Refusal{Status: 429, Reason: refusal.RateLimited}, want: true},
		{name: "an unparseable 403", err: &Refusal{Status: 403, Reason: refusal.Unknown}},
		{name: "an unparseable 500", err: &Refusal{Status: 500, Reason: refusal.Unknown}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallbackAllowed(tt.err); got != tt.want {
				t.Errorf("fallbackAllowed(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestFallbackAllowedThroughAWrappedError keeps the classification working
// after the error has been wrapped on its way up.
func TestFallbackAllowedThroughAWrappedError(t *testing.T) {
	wrapped := fmt.Errorf("placing checkout-api: %w", &Refusal{Status: 403, Reason: refusal.NoPolicy})
	if fallbackAllowed(wrapped) {
		t.Error("a wrapped authorization refusal was treated as an optimisation failure")
	}
}

// TestRefusedFallbackExplainsItself. A caller that set --on-unavailable and saw
// a plain refusal would reasonably conclude the flag is broken.
func TestRefusedFallbackExplainsItself(t *testing.T) {
	original := &Refusal{Status: http.StatusForbidden, Reason: refusal.NoPolicy, Message: "no placement policy permits this caller"}
	err := refusedFallback(original)

	var ref *Refusal
	if !errors.As(err, &ref) || ref != original {
		t.Fatalf("refusedFallback() lost the original refusal: %v", err)
	}
	for _, want := range []string{"--on-unavailable", "NoPolicy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

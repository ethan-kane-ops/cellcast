package oidc

import (
	"encoding/json"
	"slices"
	"testing"
)

func rawClaims(t *testing.T, in map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encoding claim %q: %v", k, err)
		}
		out[k] = encoded
	}
	return out
}

func TestProviderExtract(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		claims   map[string]any
		want     map[string]string
	}{
		{
			name:     "github actions",
			provider: "github",
			claims: map[string]any{
				"repository":       "example/app",
				"ref":              "refs/heads/main",
				"environment":      "production",
				"job_workflow_ref": "example/app/.github/workflows/deploy.yml@refs/heads/main",
				"actor":            "someone",
			},
			want: map[string]string{
				"repository":       "example/app",
				"ref":              "refs/heads/main",
				"environment":      "production",
				"job_workflow_ref": "example/app/.github/workflows/deploy.yml@refs/heads/main",
			},
		},
		{
			name:     "buildkite",
			provider: "buildkite",
			claims: map[string]any{
				"organization_slug": "example",
				"pipeline_slug":     "app-deploy",
				"build_branch":      "main",
			},
			want: map[string]string{
				"organization_slug": "example",
				"pipeline_slug":     "app-deploy",
				"build_branch":      "main",
			},
		},
		{
			name:     "generic extracts nothing",
			provider: "generic",
			claims:   map[string]any{"repository": "example/app"},
			want:     nil,
		},
		{
			name:     "absent claim is omitted, not empty",
			provider: "github",
			claims:   map[string]any{"repository": "example/app"},
			want:     map[string]string{"repository": "example/app"},
		},
		{
			name:     "numeric and boolean claims are rendered",
			provider: "buildkite",
			claims: map[string]any{
				"organization_slug": "example",
				"pipeline_slug":     42,
				"build_branch":      true,
			},
			want: map[string]string{
				"organization_slug": "example",
				"pipeline_slug":     "42",
				"build_branch":      "true",
			},
		},
		{
			name:     "structured claims are skipped rather than flattened",
			provider: "github",
			claims: map[string]any{
				"repository":  "example/app",
				"ref":         map[string]any{"nested": "value"},
				"environment": []string{"a", "b"},
			},
			want: map[string]string{"repository": "example/app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ProviderFor(tt.provider)
			if err != nil {
				t.Fatalf("ProviderFor(%q) = %v, want nil", tt.provider, err)
			}

			got := p.extract(rawClaims(t, tt.claims))
			if len(got) != len(tt.want) {
				t.Fatalf("extract() = %v, want %v", got, tt.want)
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("extract()[%q] = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestProviderForRejectsUnknown(t *testing.T) {
	if _, err := ProviderFor("jenkins"); err == nil {
		t.Fatal("ProviderFor(\"jenkins\") = nil error, want a failure")
	}
}

func TestProviderNamesAreStable(t *testing.T) {
	want := []string{"buildkite", "generic", "github"}
	if got := providerNames(); !slices.Equal(got, want) {
		t.Errorf("providerNames() = %v, want %v", got, want)
	}
}

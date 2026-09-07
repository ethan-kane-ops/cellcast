package broker

import (
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func TestResolveTTL(t *testing.T) {
	const (
		floor   = 10 * time.Minute
		ceiling = time.Hour
	)

	tests := []struct {
		name        string
		labels      map[string]string
		policy      *cellcastv1alpha1.TokenTTLPolicy
		requested   time.Duration
		ceiling     time.Duration
		wantGranted time.Duration
		wantMax     time.Duration
		wantClamped bool
		wantErr     error
	}{
		{
			name:        "a production cell gets the shortest built-in default",
			labels:      map[string]string{"env": "prod"},
			ceiling:     ceiling,
			wantGranted: 10 * time.Minute,
			wantMax:     15 * time.Minute,
		},
		{
			name:        "a development cell gets a longer one",
			labels:      map[string]string{"env": "dev"},
			ceiling:     ceiling,
			wantGranted: 30 * time.Minute,
			wantMax:     60 * time.Minute,
		},
		{
			// The failure this guards is an operator registering a production
			// cell and forgetting the label. If the unlabelled case fell back to
			// the most generous bounds, that typo would quietly hand out an hour
			// of cluster access instead of ten minutes.
			name:        "an unlabelled cell gets production bounds, not development ones",
			labels:      nil,
			ceiling:     ceiling,
			wantGranted: 10 * time.Minute,
			wantMax:     15 * time.Minute,
		},
		{
			name:        "an unrecognised env is treated as unlabelled",
			labels:      map[string]string{"env": "preprod-eu"},
			ceiling:     ceiling,
			wantGranted: 10 * time.Minute,
			wantMax:     15 * time.Minute,
		},
		{
			name:        "a caller may ask for less than the default",
			labels:      map[string]string{"env": "dev"},
			requested:   12 * time.Minute,
			ceiling:     ceiling,
			wantGranted: 12 * time.Minute,
			wantMax:     60 * time.Minute,
		},
		{
			name:        "a caller asking for more than the maximum is clamped to it",
			labels:      map[string]string{"env": "prod"},
			requested:   8 * time.Hour,
			ceiling:     ceiling,
			wantGranted: 15 * time.Minute,
			wantMax:     15 * time.Minute,
			wantClamped: true,
		},
		{
			name:        "policy overrides the built-in bounds in both directions",
			labels:      map[string]string{"env": "prod"},
			policy:      &cellcastv1alpha1.TokenTTLPolicy{Default: dur(20 * time.Minute), Max: dur(45 * time.Minute)},
			ceiling:     ceiling,
			wantGranted: 20 * time.Minute,
			wantMax:     45 * time.Minute,
		},
		{
			name:        "the hub ceiling caps a policy that asks for more",
			labels:      map[string]string{"env": "dev"},
			policy:      &cellcastv1alpha1.TokenTTLPolicy{Default: dur(20 * time.Minute), Max: dur(30 * time.Hour)},
			ceiling:     ceiling,
			wantGranted: 20 * time.Minute,
			wantMax:     ceiling,
		},
		{
			// A default above the ceiling would otherwise silently grant the
			// ceiling, so Granted and Default would disagree in the response.
			name:        "a policy default above the ceiling is reduced to it",
			labels:      map[string]string{"env": "dev"},
			policy:      &cellcastv1alpha1.TokenTTLPolicy{Default: dur(6 * time.Hour), Max: dur(8 * time.Hour)},
			ceiling:     30 * time.Minute,
			wantGranted: 30 * time.Minute,
			wantMax:     30 * time.Minute,
		},
		{
			// Rounding a request up to the provider floor is fine: the
			// operator's ceiling still holds.
			name:        "a request below the provider floor is raised to it",
			labels:      map[string]string{"env": "prod"},
			requested:   time.Minute,
			ceiling:     ceiling,
			wantGranted: floor,
			wantMax:     15 * time.Minute,
		},
		{
			// Rounding the operator's ceiling up is not fine, so this refuses.
			name:    "a policy ceiling below the provider floor is refused",
			labels:  map[string]string{"env": "prod"},
			policy:  &cellcastv1alpha1.TokenTTLPolicy{Max: dur(5 * time.Minute)},
			ceiling: ceiling,
			wantErr: ErrTTLBelowProviderFloor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTTL(tt.labels, tt.policy, tt.requested, tt.ceiling, floor)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ResolveTTL() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTTL() = %v, want nil", err)
			}
			if got.Granted != tt.wantGranted {
				t.Errorf("Granted = %s, want %s", got.Granted, tt.wantGranted)
			}
			if got.Max != tt.wantMax {
				t.Errorf("Max = %s, want %s", got.Max, tt.wantMax)
			}
			if got.Clamped != tt.wantClamped {
				t.Errorf("Clamped = %v, want %v", got.Clamped, tt.wantClamped)
			}
			if got.Granted > got.Max {
				t.Errorf("Granted %s exceeds Max %s", got.Granted, got.Max)
			}
		})
	}
}

// TestNoInputGrantsMoreThanTheCeiling sweeps the inputs a caller and a policy
// author control against a fixed hub ceiling.
//
// The per-case table above asserts specific numbers; this asserts the invariant
// that actually matters, which is that no combination of them produces a
// credential the operator did not authorise.
func TestNoInputGrantsMoreThanTheCeiling(t *testing.T) {
	const ceiling = 20 * time.Minute

	envs := []string{"prod", "staging", "dev", "", "nonsense"}
	requests := []time.Duration{0, time.Second, 5 * time.Minute, 19 * time.Minute, time.Hour, 400 * time.Hour}
	policies := []*cellcastv1alpha1.TokenTTLPolicy{
		nil,
		{Default: dur(time.Hour)},
		{Max: dur(90 * 24 * time.Hour)},
		{Default: dur(72 * time.Hour), Max: dur(72 * time.Hour)},
	}

	for _, env := range envs {
		for _, req := range requests {
			for _, pol := range policies {
				labels := map[string]string{"env": env}
				got, err := ResolveTTL(labels, pol, req, ceiling, KubernetesMinTTL)
				if err != nil {
					t.Fatalf("ResolveTTL(env=%q, req=%s) = %v, want nil", env, req, err)
				}
				if got.Granted > ceiling {
					t.Errorf("env=%q request=%s policy=%+v granted %s, above the %s ceiling",
						env, req, pol, got.Granted, ceiling)
				}
				if got.Granted < KubernetesMinTTL {
					t.Errorf("env=%q request=%s granted %s, below the %s provider floor",
						env, req, got.Granted, KubernetesMinTTL)
				}
			}
		}
	}
}

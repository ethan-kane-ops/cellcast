package cli

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlaceSaysWhetherTheWorkloadStayed. A move is the line that matters: the
// hub moved a service because its cell could no longer take it, and cellcast
// removes nothing, so the old copy keeps running until somebody removes it.
func TestPlaceSaysWhetherTheWorkloadStayed(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		want     []string
		absent   []string
	}{
		{
			name:     "kept",
			previous: "euw1",
			want:     []string{"checkout-api stays on euw1, where it was placed last time"},
		},
		{
			name:     "moved",
			previous: "use1",
			want: []string{
				"checkout-api moves from use1, which cannot take it now",
				"whatever runs there keeps running until it is removed",
			},
		},
		{
			name:   "nothing remembered",
			absent: []string{"stays on", "moves from"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := successBody("euw1")
			if tt.previous != "" {
				body["previousCell"] = tt.previous
			}
			srv := hubStub(t, http.StatusOK, body)
			kubeconfig := filepath.Join(t.TempDir(), "cellcast.kubeconfig")

			out, err := run(t, srv.URL, "--workload", "checkout-api", "--kubeconfig", kubeconfig)
			if err != nil {
				t.Fatalf("place = %v, want nil", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("output does not say %q:\n%s", want, out)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(out, absent) {
					t.Errorf("output says %q with nothing remembered:\n%s", absent, out)
				}
			}
		})
	}
}

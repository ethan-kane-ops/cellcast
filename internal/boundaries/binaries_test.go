package boundaries_test

import (
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/ethan-kane-ops/cellcast"

// deps returns every package linked into the named binary.
//
// `go list -deps` is the check because it answers the question the boundary
// actually asks, which is what ends up in the binary, not what a package
// happens to name in a doc comment. Full module paths rather than relative
// ones, so the result does not depend on this test's directory.
func deps(t *testing.T, binary string) []string {
	t.Helper()

	out, err := exec.Command("go", "list", "-deps", binary).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", binary, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// TestBinaryBoundaries enforces the three-binary split.
//
// The split is a security boundary rather than tidiness (docs/architecture.md
// ADR-007): the agent runs in every registered cell, so the code that mints
// credentials must not be linked into it. Until this test existed that was a
// comment, and a comment does not survive somebody adding a convenient import.
func TestBinaryBoundaries(t *testing.T) {
	tests := []struct {
		binary string
		// forbidden is matched against every transitive dependency, as an exact
		// package or as a prefix of one.
		forbidden []string
		why       string
	}{
		{
			binary:    mod + "/cmd/cellcast-agent",
			forbidden: []string{mod + "/internal/hub/broker"},
			why:       "the agent runs in every spoke and must not contain minting code",
		},
		{
			binary: mod + "/cmd/cellcast",
			forbidden: []string{
				mod + "/internal/hub/broker",
				"sigs.k8s.io/controller-runtime",
			},
			why: "the client ships to developer machines and pipelines; it is not a controller",
		},
	}

	for _, tt := range tests {
		t.Run(tt.binary, func(t *testing.T) {
			for _, dep := range deps(t, tt.binary) {
				for _, bad := range tt.forbidden {
					if dep == bad || strings.HasPrefix(dep, bad+"/") {
						t.Errorf("%s links %s: %s", tt.binary, dep, tt.why)
					}
				}
			}
		})
	}
}

// TestHubIsTheOnlyMinter pins the other half of the same boundary. A test that
// only forbids things passes just as well when the code has been deleted.
func TestHubIsTheOnlyMinter(t *testing.T) {
	broker := mod + "/internal/hub/broker"

	for _, dep := range deps(t, mod+"/cmd/cellcast-hub") {
		if dep == broker {
			return
		}
	}
	t.Errorf("cellcast-hub does not link %s; it is the binary that mints", broker)
}

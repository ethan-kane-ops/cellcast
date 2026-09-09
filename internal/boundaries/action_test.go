package boundaries_test

import (
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The composite action is a published interface. Its inputs are what a caller
// writes in their own workflow, and the table in examples/integrations/README.md
// is where they read what those are. Nothing but this ties the two together,
// and an input added without a row is one nobody outside this repository knows
// exists.

type compositeAction struct {
	Inputs map[string]struct {
		Description string `json:"description"`
		Required    bool   `json:"required"`
		Default     string `json:"default"`
	} `json:"inputs"`
	Outputs map[string]struct {
		Description string `json:"description"`
	} `json:"outputs"`
}

func placeAction(t *testing.T) compositeAction {
	t.Helper()

	var action compositeAction
	raw := readRepoFile(t, filepath.Join(".github", "actions", "place", "action.yml"))
	if err := yaml.Unmarshal([]byte(raw), &action); err != nil {
		t.Fatalf("parsing the place action: %v", err)
	}
	if len(action.Inputs) == 0 || len(action.Outputs) == 0 {
		t.Fatal("the place action declares no inputs or no outputs, so this test checked nothing")
	}
	return action
}

func TestTheActionsInterfaceIsWrittenDown(t *testing.T) {
	action := placeAction(t)
	readme := readRepoFile(t, filepath.Join("examples", "integrations", "README.md"))

	for name := range action.Inputs {
		if !strings.Contains(readme, "`"+name+"`") {
			t.Errorf("the action takes %s and examples/integrations/README.md does not mention it", name)
		}
	}
	for name := range action.Outputs {
		if !strings.Contains(readme, "`"+name+"`") {
			t.Errorf("the action publishes %s and examples/integrations/README.md does not mention it", name)
		}
	}
}

func TestEveryActionInputSaysWhatItIsFor(t *testing.T) {
	// A composite action's inputs are the only documentation GitHub renders for
	// it. An input with no description is one a caller has to read the
	// implementation to understand.
	action := placeAction(t)

	for name, input := range action.Inputs {
		if strings.TrimSpace(input.Description) == "" {
			t.Errorf("input %s has no description", name)
		}
	}
	for name, output := range action.Outputs {
		if strings.TrimSpace(output.Description) == "" {
			t.Errorf("output %s has no description", name)
		}
	}
}

func TestTheActionInstallsTheVersionTheChartsDeclare(t *testing.T) {
	// The default is what a caller who pins nothing gets, and it is the one
	// version number the release flow could plausibly forget. Left behind, the
	// action quietly installs an old client against a new hub, which from
	// inside somebody else's workflow looks like anything but a version
	// problem.
	action := placeAction(t)
	want := chartYAML(t, "cellcast").AppVersion

	if got := action.Inputs["version"].Default; got != want {
		t.Errorf("the action installs %s by default and the charts declare %s; run `just release-version`", got, want)
	}
}

func TestTheActionsSigningIdentityIsTheOneTheDocsName(t *testing.T) {
	// The action verifies the release's signature against an identity written
	// into it. If that drifts from the one the README tells an adopter to
	// expect, one of the two is checking nothing, and the action is the copy
	// nobody reads.
	raw := readRepoFile(t, filepath.Join(".github", "actions", "place", "action.yml"))
	identity := "https://github.com/ethan-kane-ops/cellcast/.github/workflows/release.yml"

	if !strings.Contains(raw, identity) {
		t.Errorf("the action does not verify against %s, which is the identity the README names", identity)
	}
	if !strings.Contains(readRepoFile(t, "README.md"), identity) {
		t.Errorf("README.md no longer names %s; the action is verifying against an identity nothing documents", identity)
	}
}

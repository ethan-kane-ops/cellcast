package boundaries_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// integrationFlag matches a long flag as an example writes one, in a container
// argument, a shell command or the prose beside them.
var integrationFlag = regexp.MustCompile(`--([a-z][a-z0-9-]+)`)

func TestTheIntegrationExamplesOnlyNameFlagsThatExist(t *testing.T) {
	// These are copied straight into somebody's pipeline, which is a worse
	// place to find a renamed flag than a documentation page: the reader is not
	// reading, they are pasting, and the error arrives in a deploy.
	accepted := append(clientFlags(t), binaryFlags(t, "cellcast-hub")...)
	// Flags belonging to the other programs the examples legitimately call.
	foreign := []string{"--audience", "--lifetime", "--kubeconfig", "--jq"}

	root := filepath.Join(repoRoot(t), "examples", "integrations")
	var checked int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(repoRoot(t), path)
		if err != nil {
			return err
		}
		text := readRepoFile(t, rel)

		for _, m := range integrationFlag.FindAllStringSubmatch(text, -1) {
			flag := "--" + m[1]
			checked++
			if slices.Contains(foreign, flag) || slices.Contains(accepted, flag) {
				continue
			}
			t.Errorf("%s names %s, which neither the client nor the hub accepts", rel, flag)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking examples/integrations: %v", err)
	}
	if checked == 0 {
		t.Fatal("the integration examples name no flags, so this test checked nothing")
	}
}

// actionRef matches the ref an example tells a caller to pin the composite
// action to.
var actionRef = regexp.MustCompile(`actions/place@(v[0-9]+\.[0-9]+\.[0-9]+)`)

func TestEveryExamplePinsTheActionToATagThatContainsIt(t *testing.T) {
	// The other version number in the same examples, and the one with teeth.
	// A stale `version:` input installs an old client, which at least runs; a
	// stale ref names a tag cut before action.yml existed, and GitHub cannot
	// resolve it at all. The failure lands in a stranger's workflow, on their
	// first attempt, reading "repository does not contain the path".
	//
	// This is exactly how v0.2.0 shipped: the examples were written in the same
	// change as the action and pinned the only tag there was, which was the one
	// released before it.
	//
	// The tag is checked against the charts rather than against git, because on
	// a release branch the tag does not exist yet. What makes the ref resolve
	// is ordering: the action reaches main before a tag is ever cut from it.
	want := chartYAML(t, "cellcast").AppVersion

	var found int
	// Tracked rather than walked: the working tree also holds gitignored
	// notes and a built copy of the docs site, and neither ships.
	for _, name := range trackedFiles(t) {
		if !slices.Contains([]string{".md", ".yml", ".yaml"}, filepath.Ext(name)) {
			continue
		}
		for _, ref := range actionRef.FindAllStringSubmatch(readRepoFile(t, name), -1) {
			found++
			if ref[1] != want {
				t.Errorf("%s pins the action at %s and the charts declare %s; run `just release-version`", name, ref[1], want)
			}
		}
	}

	if found == 0 {
		t.Fatal("no example pins the action to a released tag, so this test checked nothing")
	}
}

func TestTheTagTheExamplesNameContainsTheAction(t *testing.T) {
	// The check above only proves the examples and the charts say the same
	// number, and both said v0.2.0 while v0.2.0 was a tag cut before the action
	// was written. Agreement is not resolvability.
	//
	// This asks git directly, and so it is the one that fires on the real
	// failure. A release branch has not cut its tag yet, so a missing tag
	// skips and the version-agreement test above covers it. A CI checkout with
	// no tags at all fails instead: that is a job that dropped fetch-tags, and
	// a skip there would hide this test on every run without anyone noticing.
	tag := chartYAML(t, "cellcast").AppVersion
	root := repoRoot(t)

	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", tag+"^{commit}").Run(); err != nil {
		if os.Getenv("CI") != "" {
			tags, _ := exec.Command("git", "-C", root, "tag", "--list").Output()
			if strings.TrimSpace(string(tags)) == "" {
				t.Fatal("this CI checkout has no tags, so this test could never run here; the job's checkout needs fetch-tags: true")
			}
		}
		t.Skipf("%s is not a tag in this clone yet", tag)
	}

	action := filepath.Join(".github", "actions", "place", "action.yml")
	out, err := exec.Command("git", "-C", root, "ls-tree", "-r", "--name-only", tag, "--", action).Output()
	if err != nil {
		t.Fatalf("reading %s at %s: %v", action, tag, err)
	}

	if strings.TrimSpace(string(out)) == "" {
		t.Errorf("the examples tell a caller to use the action at %s, and %s does not contain %s; "+
			"GitHub cannot resolve that ref at all. Cut a release from a commit that has the action", tag, tag, action)
	}
}

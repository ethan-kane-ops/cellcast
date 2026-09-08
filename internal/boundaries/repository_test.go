package boundaries_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Properties of the repository itself rather than of the code in it. Each one
// is something a reader, an adopter or an automated scanner will draw a
// conclusion from, and each is quiet when it breaks.

// trackedFiles returns every file git knows about, which is the right set: an
// ignored build artifact is not part of what anybody clones.
func trackedFiles(t *testing.T) []string {
	t.Helper()

	out, err := exec.Command("git", "-C", repoRoot(t), "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("listing tracked files: %v", err)
	}

	var files []string
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files
}

// executableMagic is the leading bytes of the formats a compiled artifact
// arrives in: ELF, both Mach-O endiannesses, the fat Mach-O wrapper, and PE.
var executableMagic = [][]byte{
	{0x7f, 'E', 'L', 'F'},
	{0xfe, 0xed, 0xfa, 0xce},
	{0xce, 0xfa, 0xed, 0xfe},
	{0xfe, 0xed, 0xfa, 0xcf},
	{0xcf, 0xfa, 0xed, 0xfe},
	{0xca, 0xfe, 0xba, 0xbe},
	{'M', 'Z'},
}

func TestNoCompiledArtefactIsCheckedIn(t *testing.T) {
	// A binary in the tree is code nobody reviewed, that no build produced and
	// that no test covers, sitting in the repository of a tool that mints
	// cluster credentials. It is also the check most likely to start failing by
	// accident: a stray `go build -o` writes into the working directory, and
	// `git add -A` is one keystroke.
	files := trackedFiles(t)
	if len(files) == 0 {
		t.Fatal("git tracks no files, so this test checked nothing")
	}

	for _, name := range files {
		head := make([]byte, 4)
		f, err := os.Open(filepath.Join(repoRoot(t), name))
		if err != nil {
			continue // A deleted-but-staged path is not this test's business.
		}
		n, _ := f.Read(head)
		_ = f.Close()

		for _, magic := range executableMagic {
			if n >= len(magic) && bytes.Equal(head[:len(magic)], magic) {
				t.Errorf("%s is a compiled artefact; nothing built from source belongs in the tree", name)
			}
		}
	}
}

func TestTheSecurityPolicySaysWhereToReport(t *testing.T) {
	// A security policy that does not carry a route is worse than none: it
	// looks like the question has been answered, so a reporter who cannot find
	// an address opens a public issue instead, which is the outcome the policy
	// exists to prevent.
	policy := readRepoFile(t, "SECURITY.md")

	hasRoute := strings.Contains(policy, "security/advisories/new") ||
		regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.]+`).MatchString(policy)
	if !hasRoute {
		t.Error("SECURITY.md names no private reporting route; a reporter with nowhere to go opens a public issue")
	}
}

var (
	usesAction     = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)`)
	pinnedToCommit = regexp.MustCompile(`@[0-9a-f]{40}$`)
)

func TestEveryWorkflowPinsItsActionsAndScopesItsToken(t *testing.T) {
	// A tripwire rather than a check on today's tree. CI is dormant, so this
	// skips; the first workflow anybody adds is the one that would otherwise
	// ship with a write-all token and floating action tags, and find out from
	// an OSSF Scorecard report published alongside the launch.
	//
	// A tag is not a pin. `actions/checkout@v4` resolves to whatever that tag
	// points at today, which is a third party's write access to this repository
	// on every run.
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no workflows: %v", err)
	}

	live := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}

		text := readRepoFile(t, filepath.Join(".github", "workflows", name))
		// The dormant workflow is commented out in full, so it declares
		// nothing and runs nothing. Judging it would report a problem that
		// cannot happen.
		if !containsOutsideComments(text, "jobs:") {
			continue
		}
		live++

		t.Run(name, func(t *testing.T) {
			if !containsOutsideComments(text, "permissions:") {
				t.Error("the workflow declares no permissions, so its token gets the repository default")
			}
			for _, found := range usesAction.FindAllStringSubmatch(text, -1) {
				action := found[1]
				if strings.HasPrefix(action, "./") {
					continue // A local composite action is this repository's own code.
				}
				if !pinnedToCommit.MatchString(action) {
					t.Errorf("%s is pinned to a tag, not a commit; the tag can be moved under you", action)
				}
			}
		})
	}

	if live == 0 {
		t.Skip("every workflow is commented out; this activates with the first live one")
	}
}

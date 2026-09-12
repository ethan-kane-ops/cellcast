package boundaries_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ethan-kane-ops/cellcast/internal/refusal"
)

// Documentation rots quietly. A refusal reason nobody documented is one a user
// meets in a build log with nowhere to look it up, and nothing about the code
// says so. These tests tie the pages that enumerate something to the thing they
// enumerate.

func readDoc(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", name))
	if err != nil {
		t.Fatalf("reading docs/%s: %v", name, err)
	}
	return string(raw)
}

func TestEveryRefusalReasonIsDocumented(t *testing.T) {
	// The reason is the machine-readable half of the contract: clients match on
	// it and never on the prose beside it. Adding one without a row in the
	// troubleshooting table ships a code with no explanation anywhere.
	page := readDoc(t, "troubleshooting.md")

	for _, reason := range refusal.All() {
		if reason == refusal.Unknown {
			// The hub never writes this one. It is what a client records when
			// a proxy answered instead of the hub, so it has no row of its own.
			continue
		}
		if !strings.Contains(page, "`"+string(reason)+"`") {
			t.Errorf("refusal.%s is not in docs/troubleshooting.md; a caller who meets it has nowhere to look", reason)
		}
	}
}

// docFlag matches a long flag as the docs write one, in prose or in a shell
// example.
var docFlag = regexp.MustCompile(`--([a-z][a-z0-9-]+)`)

// flagsNamedIn returns the distinct long flags a page mentions.
func flagsNamedIn(page string) []string {
	var out []string
	for _, m := range docFlag.FindAllStringSubmatch(page, -1) {
		if !slices.Contains(out, "--"+m[1]) {
			out = append(out, "--"+m[1])
		}
	}
	return out
}

func TestTheDocsOnlyNameFlagsThatExist(t *testing.T) {
	// A flag renamed in the code and left in the docs is worse than an
	// undocumented one: the reader trusts it, runs it, and gets an error that
	// does not mention the page they read.
	// Flags that belong to another program the page legitimately quotes.
	foreign := []string{
		"--service-account-issuer", "--service-account-max-token-expiration",
		"--oidc-audience", "--oidc-issuer", "--oidc-ca-file", "--token-max-ttl", "--warmup-timeout",
		"--drain-delay", "--shutdown-timeout", "--capacity-staleness", "--log-level",
		"--set", "--reuse-values", "--namespace", "--create-namespace", "--raw",
		"--from-file",
		// buildkite-agent oidc request-token, and gh, quoted by the
		// integration page.
		"--audience", "--lifetime", "--jq",
		"--previous", "--certificate-identity-regexp", "--certificate-oidc-issuer",
		"--strict", "--with-requirements", "--kubeconfig",
	}

	// Flags the docs name only to say the client does not have them. Held to
	// that at the end, in the other direction: "there is no --token flag" is a
	// promise the threat model makes (T-05), and a flag added under that name
	// would make it false without failing anything else.
	absent := []string{"--token"}

	accepted := clientFlags(t)
	pages := []string{"getting-started.md", "pipeline-integration.md", "placement-policy.md", "troubleshooting.md", "index.md", "extending.md"}

	for _, page := range pages {
		t.Run(page, func(t *testing.T) {
			named := flagsNamedIn(readDoc(t, page))
			if len(named) == 0 {
				t.Fatalf("docs/%s names no flags, so this test checked nothing", page)
			}

			for _, flag := range named {
				if slices.Contains(foreign, flag) || slices.Contains(absent, flag) || slices.Contains(accepted, flag) {
					continue
				}
				t.Errorf("docs/%s names %s, which the client does not accept", page, flag)
			}
		})
	}

	for _, flag := range absent {
		if slices.Contains(accepted, flag) {
			t.Errorf("the docs say the client has no %s flag, and it does", flag)
		}
	}
}

func TestTheHubFlagsTheDocsNameExist(t *testing.T) {
	// The hub's own flags, named across the operational pages. Same failure as
	// above, one process over.
	accepted := binaryFlags(t, "cellcast-hub")

	pages := []string{"high-availability.md", "token-brokering.md", "observability.md", "troubleshooting.md"}
	// Flags belonging to another binary or to Kubernetes itself.
	ignore := []string{
		"--on-unavailable", "--cache-ttl", "--cache-dir", "--dry-run", "--explain",
		"--dark", "--ttl", "--json", "--workload", "--hub", "--kubeconfig",
		"--service-account-max-token-expiration", "--service-account-issuer",
		"--previous", "--reuse-values", "--set", "--create-namespace", "--raw",
		"--certificate-identity-regexp", "--certificate-oidc-issuer", "--strict",
	}

	for _, page := range pages {
		t.Run(page, func(t *testing.T) {
			for _, flag := range flagsNamedIn(readDoc(t, page)) {
				if slices.Contains(ignore, flag) || slices.Contains(accepted, flag) {
					continue
				}
				t.Errorf("docs/%s names %s, which `cellcast-hub` does not accept", page, flag)
			}
		})
	}
}

func TestTheDocsOnlyNameRecipesThatExist(t *testing.T) {
	// `just` recipe names appear all over the development page and in
	// CONTRIBUTING. A renamed recipe leaves a reader running a command that
	// prints a list of recipes it is not in.
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just is not installed")
	}

	out, err := exec.Command("just", "--summary", "--justfile", filepath.Join(repoRoot(t), "justfile")).Output()
	if err != nil {
		t.Skipf("listing recipes: %v", err)
	}
	recipes := strings.Fields(string(out))
	if len(recipes) == 0 {
		t.Fatal("no recipes found, so this test checked nothing")
	}

	// Only inside backticks or at the start of a line in a fenced block.
	// Prose like "just the audit record" is not a recipe reference, and a test
	// that says it is trains the reader to ignore this failure.
	named := regexp.MustCompile("(?m)(?:`|^)just ([a-z][a-z0-9-]*)")
	for _, file := range []string{"docs/development.md", "docs/getting-started.md", "docs/releasing.md", "docs/extending.md", "CONTRIBUTING.md", "README.md", "examples/README.md"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(repoRoot(t), file))
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}
			for _, m := range named.FindAllStringSubmatch(string(raw), -1) {
				if !slices.Contains(recipes, m[1]) {
					t.Errorf("%s says `just %s`, which is not a recipe", file, m[1])
				}
			}
		})
	}
}

package boundaries_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Keyless signing moves the trust decision out of the artifact and into the
// verification command. Nothing about a signed image says who may have signed
// it, so the identity in the README is the security control, and a README that
// has drifted from the signer is worse than an unsigned release: the first
// thing it teaches an adopter is that a verification failure is normal.
//
// These tests hold the documentation, the release and the signing configuration
// to one another.

// justRecipe returns the source of one recipe, so a test can assert what a step
// actually does rather than that a string appears somewhere in the justfile.
func justRecipe(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just is not installed")
	}

	out, err := exec.Command("just", "--justfile", filepath.Join(repoRoot(t), "justfile"), "--show", name).Output()
	if err != nil {
		t.Fatalf("just --show %s: %v", name, err)
	}
	return string(out)
}

// justDependencies returns the recipes one recipe depends on, read off its
// header line.
//
// Parsed rather than searched for. A substring test over the recipe source is
// answered by the comment above it: asking whether `check-all` mentions "vuln"
// is satisfied by a comment saying "plus vulnerabilities", which is how the
// first version of this check passed while the dependency was gone.
func justDependencies(t *testing.T, name string) []string {
	t.Helper()

	for _, line := range strings.Split(justRecipe(t, name), "\n") {
		if !strings.HasPrefix(line, name+":") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		_, deps, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		return strings.Fields(deps)
	}
	t.Fatalf("no header line found for recipe %s", name)
	return nil
}

var (
	docIdentity = regexp.MustCompile(`--certificate-identity (\S+)`)
	docIssuer   = regexp.MustCompile(`--certificate-oidc-issuer (\S+)`)
)

func TestTheDocumentedVerifyCommandNamesTheIdentityTheReleaseChecks(t *testing.T) {
	// `just verify` is run by `just release` as its last step, against what it
	// has just published. If the command in the README names a different
	// identity or issuer, then every adopter who follows the README gets a
	// failure on an artifact the release considered good.
	wantPrefix := justVar(t, "sign_workflow") + "@refs/tags/"
	wantIssuer := justVar(t, "sign_issuer")

	pages := []string{"README.md", "docs/releasing.md"}
	total := 0

	for _, page := range pages {
		t.Run(page, func(t *testing.T) {
			text := readRepoFile(t, page)

			identities := docIdentity.FindAllStringSubmatch(text, -1)
			for _, found := range identities {
				if !strings.HasPrefix(found[1], wantPrefix) {
					t.Errorf("the page tells adopters to expect %s, but the release signs as %s<tag>", found[1], wantPrefix)
				}
			}
			for _, found := range docIssuer.FindAllStringSubmatch(text, -1) {
				if found[1] != wantIssuer {
					t.Errorf("the page names issuer %s, but the release verifies against %s", found[1], wantIssuer)
				}
			}
			if len(identities) != len(docIssuer.FindAllStringSubmatch(text, -1)) {
				t.Error("the page pairs a different number of identities and issuers; cosign verify needs both, and one alone accepts any signer at that issuer")
			}
			total += len(identities)
		})
	}

	if total == 0 {
		t.Fatalf("none of %v documents a verify command, so this test checked nothing", pages)
	}
}

func TestTheReleaseVerifiesTheSignaturesItMade(t *testing.T) {
	// Signing and then not checking the result is how a release ends up
	// carrying a certificate for the wrong identity. The order matters as much
	// as the presence: verification has to come after everything is published,
	// because it is a check on the published artifacts and not on the intent.
	release := justRecipe(t, "release")

	sign := strings.Index(release, "just sign ")
	verify := strings.Index(release, "just verify ")

	switch {
	case sign < 0:
		t.Error("the release does not sign what it publishes")
	case verify < 0:
		t.Error("the release does not verify what it signed; a signature nobody checked is a signature nobody can rely on")
	case verify < sign:
		t.Error("the release verifies before it signs, which checks the previous release")
	}
}

func TestNothingShipsWithoutAVulnerabilityScan(t *testing.T) {
	// govulncheck sits outside the fast gate, which means the only
	// thing standing between a known reachable vulnerability and a published
	// image is that `release` runs the heavy one. Moving the scan out of
	// `check-all`, or the gate out of `release`, would leave both recipes
	// looking correct and remove the property entirely.
	if !slices.Contains(justDependencies(t, "check-all"), "vuln") {
		t.Error("check-all does not scan for vulnerabilities")
	}
	if !containsOutsideComments(justRecipe(t, "release"), "just check-all") {
		t.Error("the release does not run check-all, so nothing scans what it publishes")
	}
}

func TestEveryPublishedImageCarriesAnSBOMAndProvenance(t *testing.T) {
	// The attestations are attached during the build, so they exist only if the
	// publishing build asks for them. Dropping the flags produces images that
	// are otherwise identical and that answer nothing about what is inside.
	push := justRecipe(t, "images-push")

	for _, flag := range []string{"--sbom=true", "--provenance="} {
		if !containsOutsideComments(push, flag) {
			t.Errorf("images-push does not pass %s; the published images would carry no attestation", flag)
		}
	}
}

// signing is the part of the goreleaser config that decides what a signature
// means.
type signing struct {
	Signs []struct {
		Cmd       string   `json:"cmd"`
		Args      []string `json:"args"`
		Artifacts string   `json:"artifacts"`
	} `json:"signs"`
	SBOMs []struct {
		Artifacts string `json:"artifacts"`
	} `json:"sboms"`
}

func TestTheReleaseSignsWithoutAKey(t *testing.T) {
	// A key-based signature verifies with a completely different command, one
	// that names a public key instead of an identity. Every verification
	// instruction in this repository would still look right and would all fail,
	// and the fix would be a documentation change nobody would think to make.
	var cfg signing
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".goreleaser.yaml")), &cfg); err != nil {
		t.Fatalf("parsing .goreleaser.yaml: %v", err)
	}
	if len(cfg.Signs) == 0 {
		t.Fatal("goreleaser signs nothing, so the release publishes unsigned archives")
	}

	for _, sign := range cfg.Signs {
		if sign.Cmd != "cosign" {
			t.Errorf("goreleaser signs with %q; the documented verification is cosign's", sign.Cmd)
		}
		for _, arg := range sign.Args {
			if strings.HasPrefix(arg, "--key") {
				t.Errorf("goreleaser signs with %s; keyless is what the documented verify command checks", arg)
			}
		}
	}

	for _, recipe := range []string{"sign", "verify"} {
		if strings.Contains(justRecipe(t, recipe), "--key") {
			t.Errorf("the %s recipe uses a key; keyless is what the documented verify command checks", recipe)
		}
	}
}

func TestTheArchivesCarryAnSBOM(t *testing.T) {
	// The images get theirs from BuildKit. The archives are the other half, and
	// they are the artifact most likely to be dropped into a pipeline image
	// where somebody later has to answer what is in it.
	var cfg signing
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".goreleaser.yaml")), &cfg); err != nil {
		t.Fatalf("parsing .goreleaser.yaml: %v", err)
	}

	for _, sbom := range cfg.SBOMs {
		if sbom.Artifacts == "archive" {
			return
		}
	}
	t.Error("no SBOM is generated for the archives")
}

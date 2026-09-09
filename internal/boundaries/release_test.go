package boundaries_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// A release is the same handful of facts written down in four places: the
// justfile builds binaries, the Dockerfile builds images, .goreleaser.yaml
// builds archives, and the charts pull what the first three produced. Nothing
// makes them agree.
//
// The failures are all quiet. A chart that names an image nobody builds installs
// cleanly and sits in ImagePullBackOff. A Dockerfile whose toolchain drifts
// below go.mod fails only when somebody builds an image, which on this
// repository was nobody, for months. A binary built without the stamping
// reports "dev" in an audit trail. These tests are the only thing standing
// between each of those and a tag.

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(raw)
}

// justVar evaluates one justfile variable, so a test reads the same value the
// recipes do rather than a second copy of it parsed out of the file.
func justVar(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just is not installed")
	}

	out, err := exec.Command("just", "--justfile", filepath.Join(repoRoot(t), "justfile"), "--evaluate", name).Output()
	if err != nil {
		t.Fatalf("just --evaluate %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// containsOutsideComments reports whether needle appears on a line that is not
// a comment.
//
// All three build files comment with #, and each of them discusses the flags it
// passes in prose right beside the line that passes them. A substring search
// over the whole file therefore passes on a Dockerfile whose only remaining
// mention of -trimpath is the comment explaining why it matters, which is
// exactly the case a break test found.
func containsOutsideComments(text, needle string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// versionVars are every variable internal/version expects the linker to stamp.
// A binary that cannot say which commit it came from is not auditable, and the
// audit trail is the product.
var versionVars = []string{"version", "commit", "date"}

func TestEveryWayOfBuildingStampsTheSameVersionVariables(t *testing.T) {
	// Three writers, one set of facts. The usual failure is a new build path
	// that forgets one variable: `cellcast version` then reports a real version
	// built at an "unknown" date, which looks close enough to right to survive
	// review.
	justfile := readRepoFile(t, "justfile")

	writers := []struct {
		name string
		// stamps is where the -X arguments live, which for the justfile is one
		// variable rather than the whole file; flags is where -trimpath lives,
		// which is the go build line beside it.
		stamps string
		flags  string
	}{
		{name: "justfile", stamps: justVar(t, "ldflags"), flags: justfile},
		{name: "Dockerfile", stamps: readRepoFile(t, "Dockerfile"), flags: readRepoFile(t, "Dockerfile")},
		{name: ".goreleaser.yaml", stamps: readRepoFile(t, ".goreleaser.yaml"), flags: readRepoFile(t, ".goreleaser.yaml")},
	}

	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			for _, v := range versionVars {
				stamp := "internal/version." + v + "="
				if !containsOutsideComments(w.stamps, stamp) {
					t.Errorf("%s does not stamp %s; the artifact it builds would report the package default", w.name, stamp)
				}
			}
			if !containsOutsideComments(w.flags, "-trimpath") {
				t.Errorf("%s does not build with -trimpath; the binary embeds the absolute path of whoever built it, and two builds of one commit differ", w.name)
			}
		})
	}
}

func TestTheBuildStampIsTheCommitNotTheClock(t *testing.T) {
	// The reproducibility claim on a tag is only as good as its timestamps. A
	// wall-clock stamp makes every rebuild of one commit a different binary,
	// which is the usual reason a "reproducible" release is not one.
	out, err := exec.Command("git", "-C", repoRoot(t), "log", "-1", "--format=%cI").Output()
	if err != nil {
		t.Skipf("reading the commit timestamp: %v", err)
	}

	want := strings.TrimSpace(string(out))
	if got := justVar(t, "date"); got != want {
		t.Errorf("the build stamps date=%s, want the commit timestamp %s", got, want)
	}
}

func TestTwoBuildsOfOneCommitAreIdentical(t *testing.T) {
	// What -trimpath is for, asserted rather than assumed. Given the same
	// source and the same flags the compiler must produce the same bytes;
	// anything that leaks in from the environment shows up here as a different
	// digest.
	dir := t.TempDir()
	ldflags := justVar(t, "ldflags")

	digests := make([]string, 2)
	for i := range digests {
		out := filepath.Join(dir, fmt.Sprintf("cellcast-%d", i))
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/cellcast")
		cmd.Dir = repoRoot(t)
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building the client: %v\n%s", err, combined)
		}

		digests[i] = sha256File(t, out)
	}

	if digests[0] != digests[1] {
		t.Errorf("two builds of the same commit differ (%s, %s); something outside the source is reaching the binary", digests[0][:12], digests[1][:12])
	}
}

func sha256File(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestTheBinaryDoesNotCarryTheBuildersFilesystem(t *testing.T) {
	// The half of the reproducibility claim that can actually fail. Byte
	// equality between two builds on one machine holds with or without
	// -trimpath, so on its own it proves very little; a binary that names the
	// directory it was compiled in cannot be reproduced by anybody who does not
	// have that directory, and it leaks the builder's home path to whoever runs
	// `strings` on a public release.
	dir := t.TempDir()
	out := filepath.Join(dir, "cellcast")

	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", justVar(t, "ldflags"), "-o", out, "./cmd/cellcast")
	cmd.Dir = repoRoot(t)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the client: %v\n%s", err, combined)
	}

	binary, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the build output: %v", err)
	}
	if bytes.Contains(binary, []byte(repoRoot(t))) {
		t.Errorf("the binary embeds %s; the build is not running with -trimpath", repoRoot(t))
	}
}

// chartImage is the part of a chart's values that decides what gets pulled.
type chartImage struct {
	Image struct {
		Registry   string `json:"registry"`
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
	} `json:"image"`
}

// chartMeta is the part of Chart.yaml the release has to keep honest.
type chartMeta struct {
	Version     string            `json:"version"`
	AppVersion  string            `json:"appVersion"`
	Annotations map[string]string `json:"annotations"`
}

var chartsInRepo = []string{"cellcast", "cellcast-agent"}

func chartValues(t *testing.T, chart string) chartImage {
	t.Helper()
	var parsed chartImage
	if err := yaml.Unmarshal([]byte(readRepoFile(t, filepath.Join("charts", chart, "values.yaml"))), &parsed); err != nil {
		t.Fatalf("parsing charts/%s/values.yaml: %v", chart, err)
	}
	return parsed
}

func chartYAML(t *testing.T, chart string) chartMeta {
	t.Helper()
	var parsed chartMeta
	if err := yaml.Unmarshal([]byte(readRepoFile(t, filepath.Join("charts", chart, "Chart.yaml"))), &parsed); err != nil {
		t.Fatalf("parsing charts/%s/Chart.yaml: %v", chart, err)
	}
	return parsed
}

func TestEveryImageAChartPullsIsAnImageTheReleaseBuilds(t *testing.T) {
	// This one has already happened. Both charts referenced
	// ghcr.io/ethan-kane-ops/cellcast-hub and -agent while the only Dockerfile
	// in the repository built the client, and no image existed for either
	// server. Nothing said so, because a chart naming an image that does not
	// exist is a perfectly valid chart.
	built := strings.Fields(justVar(t, "image_binaries"))
	if len(built) == 0 {
		t.Fatal("the justfile publishes no images, so this test checked nothing")
	}
	registry, owner := justVar(t, "registry"), justVar(t, "owner")

	for _, chart := range chartsInRepo {
		t.Run(chart, func(t *testing.T) {
			image := chartValues(t, chart).Image
			if image.Repository == "" {
				t.Fatalf("charts/%s/values.yaml names no image repository", chart)
			}

			if image.Registry != registry {
				t.Errorf("the chart pulls from %s, but the release pushes to %s", image.Registry, registry)
			}

			gotOwner, name, ok := strings.Cut(image.Repository, "/")
			if !ok {
				t.Fatalf("image.repository %q is not owner/name", image.Repository)
			}
			if gotOwner != owner {
				t.Errorf("the chart pulls from %s/%s, but the release pushes to %s/", registry, gotOwner, owner)
			}

			if _, err := os.Stat(filepath.Join(repoRoot(t), "cmd", name)); err != nil {
				t.Errorf("the chart pulls %s, which is not a binary in cmd/", name)
			}
			if !slices.Contains(built, name) {
				t.Errorf("the chart pulls %s, which the release does not build; the install would sit in ImagePullBackOff", name)
			}
		})
	}
}

func TestEveryImageTheReleaseBuildsIsABinaryInThisRepository(t *testing.T) {
	// The other direction. A test that only checks the charts' side passes just
	// as well when the release is quietly building something that does not
	// exist, and the Dockerfile's own guard would not fire until somebody ran
	// the build for that architecture.
	built := strings.Fields(justVar(t, "image_binaries"))
	if len(built) == 0 {
		t.Fatal("the justfile publishes no images, so this test checked nothing")
	}

	for _, name := range built {
		if _, err := os.Stat(filepath.Join(repoRoot(t), "cmd", name)); err != nil {
			t.Errorf("the release builds an image for %q, which is not a binary in cmd/", name)
		}
	}
}

// externalPublishers are goreleaser sections that push to a repository other
// than this one. Each needs a credential the release workflow does not have.
var externalPublishers = []string{
	"homebrew_casks", "brews", "nix", "scoops", "winget", "aurs", "chocolateys", "krews",
}

func TestTheReleasePublishesOnlyWhereItsTokenReaches(t *testing.T) {
	// The release workflow authenticates with GITHUB_TOKEN, which GitHub scopes
	// to this repository alone. A publisher pointed at a tap, a bucket or
	// another repository therefore fails on a credential, and it fails in the
	// last step: after three images, two charts and the GitHub release have all
	// been pushed. A half-published release is the one state this pipeline
	// cannot back out of on its own.
	//
	// It is also the step no rehearsal covers. `just release-check` runs
	// goreleaser with --skip=publish, so the first execution of a publisher is
	// the real release. That combination is why this is a test rather than a
	// note: a tap that had never been created sat in this file for weeks
	// without anything failing.
	config := readRepoFile(t, ".goreleaser.yaml")

	for _, section := range externalPublishers {
		if containsOutsideComments(config, section+":") {
			t.Errorf("goreleaser declares %s, which publishes to another repository; "+
				"the release workflow's GITHUB_TOKEN cannot reach it, and it would fail "+
				"after everything else is already public", section)
		}
	}
}

func TestTheChartsDeclareTheTagTheyWillBePublishedUnder(t *testing.T) {
	// version and appVersion are one number cut from one commit, and appVersion
	// is also the default image tag. If appVersion lost its leading v the
	// default pull would be :0.2.0 against a registry holding :v0.2.0, which is
	// a chart that installs and never starts.
	semver := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

	var first chartMeta
	for i, chart := range chartsInRepo {
		meta := chartYAML(t, chart)

		if !semver.MatchString(meta.Version) {
			t.Errorf("charts/%s declares version %q, which Helm requires to be bare semver", chart, meta.Version)
		}
		if meta.AppVersion != "v"+meta.Version {
			t.Errorf("charts/%s declares version %q and appVersion %q; appVersion is the git tag, so it is version with a leading v", chart, meta.Version, meta.AppVersion)
		}
		if tag := chartValues(t, chart).Image.Tag; tag != "" {
			t.Errorf("charts/%s pins image.tag to %q; leaving it empty is what makes appVersion the single answer", chart, tag)
		}

		if i == 0 {
			first = meta
			continue
		}
		if meta.Version != first.Version {
			t.Errorf("charts/%s is at %s and charts/%s is at %s; they are cut from one tag", chart, meta.Version, chartsInRepo[0], first.Version)
		}
	}
}

// Version badges as helm-docs writes them into each chart's README.
var (
	readmeVersion    = regexp.MustCompile(`\[Version: ([^\]]+)\]`)
	readmeAppVersion = regexp.MustCompile(`\[AppVersion: ([^\]]+)\]`)
)

func TestTheChartReadmesShowTheVersionTheChartDeclares(t *testing.T) {
	// helm-docs writes the version into a badge at the top of each chart's
	// README, and that README is the page Artifact Hub renders. Nothing
	// regenerates it when Chart.yaml changes, so the badge silently keeps
	// advertising the previous release. Bumping appVersion for this pipeline
	// left both charts claiming the old one.
	for _, chart := range chartsInRepo {
		t.Run(chart, func(t *testing.T) {
			meta := chartYAML(t, chart)
			readme := readRepoFile(t, filepath.Join("charts", chart, "README.md"))

			for _, badge := range []struct {
				name    string
				pattern *regexp.Regexp
				want    string
			}{
				{name: "Version", pattern: readmeVersion, want: meta.Version},
				{name: "AppVersion", pattern: readmeAppVersion, want: meta.AppVersion},
			} {
				found := badge.pattern.FindStringSubmatch(readme)
				if found == nil {
					t.Errorf("the README shows no %s badge; run `just chart-docs`", badge.name)
					continue
				}
				if found[1] != badge.want {
					t.Errorf("the README advertises %s %s and Chart.yaml declares %s; run `just chart-docs`",
						badge.name, found[1], badge.want)
				}
			}
		})
	}
}

func TestTheChartsDeclareTheLicenceTheRepositoryUses(t *testing.T) {
	// Artifact Hub shows this annotation on the listing page, and a licence
	// stated wrongly there is a licence statement a stranger relies on. Both
	// charts said MIT while the repository has always been Apache-2.0.
	markers := map[string]string{
		"Apache License":                     "Apache-2.0",
		"MIT License":                        "MIT",
		"GNU GENERAL PUBLIC LICENSE":         "GPL-3.0",
		"Mozilla Public License Version 2.0": "MPL-2.0",
	}

	licence := readRepoFile(t, "LICENSE")
	want := ""
	for marker, spdx := range markers {
		if strings.Contains(licence, marker) {
			want = spdx
			break
		}
	}
	if want == "" {
		t.Fatal("LICENSE matches no licence this test recognises; add it to markers rather than deleting the check")
	}

	for _, chart := range chartsInRepo {
		if got := chartYAML(t, chart).Annotations["artifacthub.io/license"]; got != want {
			t.Errorf("charts/%s declares artifacthub.io/license %q, but LICENSE is %s", chart, got, want)
		}
	}
}

// goreleaserConfig is the part of the release config these tests reason about.
type goreleaserConfig struct {
	Builds []struct {
		ID   string `json:"id"`
		Main string `json:"main"`
	} `json:"builds"`
}

func TestTheArchivesShipTheClientAndNothingElse(t *testing.T) {
	// The hub and the agent are servers that hold trust configuration and mint
	// credentials. They ship as images, installed by a chart that applies a
	// service account, RBAC, a security context and a network policy. A tarball
	// of the hub is an invitation to run the broker as a host process with none
	// of that, and it would look like a supported way to do it.
	var cfg goreleaserConfig
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".goreleaser.yaml")), &cfg); err != nil {
		t.Fatalf("parsing .goreleaser.yaml: %v", err)
	}
	if len(cfg.Builds) == 0 {
		t.Fatal("goreleaser builds nothing, so this test checked nothing")
	}

	for _, build := range cfg.Builds {
		if build.Main != "./cmd/cellcast" {
			t.Errorf("goreleaser build %q ships %s; only the client is distributed as an archive", build.ID, build.Main)
		}
	}
}

// dockerfileImageRefs returns every image reference the Dockerfile resolves,
// which on this Dockerfile means the two ARGs the FROM lines interpolate.
var (
	imageArg      = regexp.MustCompile(`(?m)^ARG (?:GO|RUNTIME)_IMAGE=(\S+)`)
	dockerfileGo  = regexp.MustCompile(`(?m)^ARG GO_IMAGE=golang:(\d+)\.(\d+)`)
	moduleGo      = regexp.MustCompile(`(?m)^go (\d+)\.(\d+)`)
	dockerfileUsr = regexp.MustCompile(`(?m)^USER (\d+)`)
	entrypoint    = regexp.MustCompile(`(?m)^ENTRYPOINT (.*)$`)
)

func TestTheImageToolchainCanCompileThisModule(t *testing.T) {
	// The shipped Dockerfile pinned golang:1.24-alpine against a module
	// requiring 1.26, so `docker build` failed on the first command with
	// "go.mod requires go >= 1.26.0". Nothing caught it because nothing in the
	// repository built an image.
	dockerfile := readRepoFile(t, "Dockerfile")

	image := dockerfileGo.FindStringSubmatch(dockerfile)
	if image == nil {
		t.Fatal("no golang base image found in the Dockerfile")
	}
	module := moduleGo.FindStringSubmatch(readRepoFile(t, "go.mod"))
	if module == nil {
		t.Fatal("no go directive found in go.mod")
	}

	imageMajor, imageMinor := atoi(t, image[1]), atoi(t, image[2])
	modMajor, modMinor := atoi(t, module[1]), atoi(t, module[2])

	if imageMajor < modMajor || (imageMajor == modMajor && imageMinor < modMinor) {
		t.Errorf("the Dockerfile builds on Go %s.%s but go.mod requires %s.%s; every image build fails",
			image[1], image[2], module[1], module[2])
	}
	if !strings.Contains(dockerfile, "GOTOOLCHAIN=local") {
		t.Error("the Dockerfile does not set GOTOOLCHAIN=local; a base image below go.mod would silently download its own compiler instead of failing")
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return n
}

func TestTheBaseImagesArePinnedByDigest(t *testing.T) {
	// A tag is a moving target. Rebuilding last month's tag against this
	// month's golang:1.26-alpine produces a different binary, which makes the
	// reproducibility claim on the release false in a way nobody notices until
	// they try to check it.
	refs := imageArg.FindAllStringSubmatch(readRepoFile(t, "Dockerfile"), -1)
	if len(refs) == 0 {
		t.Fatal("no base image arguments found in the Dockerfile")
	}

	for _, ref := range refs {
		if !strings.Contains(ref[1], "@sha256:") {
			t.Errorf("base image %s is pinned by tag, not by digest; run `just image-bases`", ref[1])
		}
	}
}

func TestTheImageDeliversSignalsToTheProcessItStarts(t *testing.T) {
	// Exec form or the shell becomes PID 1 and SIGTERM stops there. The hub's
	// entire drain sequence (docs/architecture.md ADR-011) begins with that
	// signal reaching the process, so a shell-form entrypoint would turn every
	// rolling update back into dropped placements, with nothing in the logs.
	found := entrypoint.FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if found == nil {
		t.Fatal("the Dockerfile declares no ENTRYPOINT")
	}
	if !strings.HasPrefix(strings.TrimSpace(found[1]), "[") {
		t.Errorf("ENTRYPOINT is %s, which is shell form; SIGTERM would reach /bin/sh and the hub would never drain", found[1])
	}
}

func TestTheImageRunsAsTheUserTheChartsExpect(t *testing.T) {
	// The charts set runAsNonRoot with an explicit uid, so a root image would
	// still start. Then somebody installs without the chart, or a future values
	// change drops podSecurityContext, and the broker is running as root. The
	// image should be the same shape on its own as it is under the chart.
	got := dockerfileUsr.FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if got == nil {
		t.Fatal("the Dockerfile sets no USER; the image would run as root outside a pod security context")
	}

	for _, chart := range chartsInRepo {
		var values struct {
			PodSecurityContext struct {
				RunAsUser    int  `json:"runAsUser"`
				RunAsNonRoot bool `json:"runAsNonRoot"`
			} `json:"podSecurityContext"`
		}
		if err := yaml.Unmarshal([]byte(readRepoFile(t, filepath.Join("charts", chart, "values.yaml"))), &values); err != nil {
			t.Fatalf("parsing charts/%s/values.yaml: %v", chart, err)
		}

		if !values.PodSecurityContext.RunAsNonRoot {
			t.Errorf("charts/%s does not set runAsNonRoot", chart)
		}
		if want := strconv.Itoa(values.PodSecurityContext.RunAsUser); got[1] != want {
			t.Errorf("the image runs as uid %s and charts/%s pins runAsUser %s", got[1], chart, want)
		}
	}
}

// releaseTagInReadme matches the tag the verification examples name.
var releaseTagInReadme = regexp.MustCompile(`refs/tags/(v[0-9]+\.[0-9]+\.[0-9]+)`)

func TestTheReadmeVerifiesTheReleaseTheChartsDeclare(t *testing.T) {
	// The verification block is the one command an adopter copies verbatim, and
	// a superseded tag in it verifies perfectly well. That is the worst way for
	// it to be wrong: the reader checks a release they are not running and gets
	// a green tick for it.
	want := chartYAML(t, "cellcast").AppVersion
	readme := readRepoFile(t, "README.md")

	for _, ref := range []string{
		"ghcr.io/ethan-kane-ops/cellcast-hub:" + want,
		"refs/tags/" + want,
	} {
		if !strings.Contains(readme, ref) {
			t.Errorf("README.md does not name %s; run `just release-version`", ref)
		}
	}

	// The other direction, because the check above passes as soon as one line
	// has been updated and says nothing about the three below it.
	for _, found := range releaseTagInReadme.FindAllStringSubmatch(readme, -1) {
		if found[1] != want {
			t.Errorf("README.md tells a reader to verify %s while the charts declare %s", found[1], want)
		}
	}
}

func TestTheReleaseTagIsSignedAndNotMerelyAnnotated(t *testing.T) {
	// The distinction that hid this: commit.gpgsign signs commits, and an
	// annotated tag is a different object with a different setting. `git tag -a`
	// writes an unsigned tag pointing at a signed commit, and the tag ruleset's
	// signature rule is satisfied by that commit, so the push succeeds and the
	// release looks fully signed from every angle except the tag itself.
	//
	// The recipe carries the flag rather than relying on tag.gpgSign, because
	// the config belongs to whichever machine cuts the release and this is a
	// property of the release.
	justfile := readRepoFile(t, "justfile")

	if containsOutsideComments(justfile, "git tag -a") {
		t.Error("the release creates an annotated tag with `git tag -a`, which is unsigned unless tag.gpgSign happens to be set on that machine")
	}
	if !containsOutsideComments(justfile, "git tag -s") {
		t.Error("nothing in the justfile signs the release tag")
	}
}

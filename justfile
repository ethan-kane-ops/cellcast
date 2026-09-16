# cellcast task runner.
#
# These recipes are the verification layer. CI runs them rather than
# reimplementing them, so what CI checks and what a contributor can run locally
# cannot drift apart. `just check` is the gate that must pass before every
# commit.

binaries := "cellcast cellcast-hub cellcast-agent"

# The control plane envtest runs. Kept in step with the k8s.io/* modules in
# go.mod: a CRD schema is validated by the API server that reads it, so testing
# against a different minor than the one the hub links proves less than it looks.
envtest_k8s := "1.37.x"

# Statement coverage floor. 80 is the target; the gate is set where the suite
# is, so that a drop is a signal rather than noise.
coverage_min := "70"

# How long each fuzz target runs under `just fuzz`. Short enough to sit in a
# coffee break across every target, long enough to be worth running. A real
# campaign passes a longer value: `just fuzz 10m`.
fuzz_time := "30s"

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
commit := `git rev-parse --short HEAD 2>/dev/null || echo none`
# The commit's timestamp, not the clock. Two builds of one commit then produce
# identical bytes, which is the whole of the reproducibility claim; a wall-clock
# stamp is the usual reason a "reproducible" build is not one. The `-dirty`
# suffix on the version is what tells you the tree was not the commit.
date := `git log -1 --format=%cI 2>/dev/null || echo unknown`
pkg := "github.com/ethan-kane-ops/cellcast/internal/version"

# Where the published artifacts go. Images and charts share a registry and
# differ only by path, so one login covers both.
registry := "ghcr.io"
owner := "ethan-kane-ops"
chart_repo := "oci://" + registry + "/" + owner + "/charts"

# Binaries published as container images.
#
# The client is on this list as well as in the archives. An Argo CD PreSync hook
# or a Buildkite step needs something it can run as a container, not a tarball;
# a person installing it on a workstation unpacks the archive.
image_binaries := "cellcast-hub cellcast-agent cellcast"
chart_names := "cellcast cellcast-agent"

# Architectures every image is published for.
platforms := "linux/amd64,linux/arm64"

# Who is expected to have signed a release, and who vouched for that identity.
#
# Keyless signing puts the signer's identity in a short-lived certificate rather
# than in a key, so "is it signed" is not a useful question: the question is
# whether it was signed by the identity adopters were told to expect. These two
# values are that expectation. `just verify` checks against them, the release
# fails if the check fails, and a contract test holds the documented command to
# the same pair, because a verify command that has drifted from the signer is
# worse than no signature at all.
sign_workflow := "https://github.com/" + owner + "/cellcast/.github/workflows/release.yml"
sign_issuer := "https://token.actions.githubusercontent.com"

# buildx needs a container-driver builder to emit a multi-platform manifest. The
# default `docker` driver cannot, and says so only at the end of a long build.
builder := "cellcast"

ldflags := "-s -w" + \
    " -X " + pkg + ".version=" + version + \
    " -X " + pkg + ".commit=" + commit + \
    " -X " + pkg + ".date=" + date

# grpcnotrace drops gRPC's golang.org/x/net/trace integration, which nothing
# here uses and which imports html/template. With html/template in a binary the
# linker can no longer discard unused methods anywhere in it, so the OTLP
# exporters' gRPC dependency cost the hub 20 MB and the agent 28 MB instead of
# 3 each. The Dockerfile passes the same tag; TestServerBuildsLeaveOutGRPCTrace
# holds both.
go_tags := "grpcnotrace"

default:
    @just --list

# Build all three binaries into ./bin/
build:
    #!/usr/bin/env bash
    set -euo pipefail
    for b in {{binaries}}; do
        go build -trimpath -tags '{{go_tags}}' -ldflags '{{ldflags}}' -o "bin/$b" "./cmd/$b"
        echo "built bin/$b"
    done

# Build a single binary, e.g. `just build-one cellcast-hub`
build-one name:
    go build -trimpath -tags '{{go_tags}}' -ldflags '{{ldflags}}' -o bin/{{name}} ./cmd/{{name}}

# Run a locally built binary, e.g. `just run cellcast-hub --help`
run name *args: (build-one name)
    ./bin/{{name}} {{args}}

# Run all tests
test:
    go test ./...

# Run tests with the race detector (the capacity index is concurrent by construction)
test-race:
    go test -race ./...

# Fuzz every target for {{fuzz_time}} each (`just fuzz 10m` for a real campaign)
fuzz duration=fuzz_time:
    #!/usr/bin/env bash
    # Go fuzzes exactly one target per invocation and refuses a -fuzz regex that
    # matches more than one, so the targets are enumerated and run in turn
    # rather than handed over as a pattern.
    #
    # Not part of `check`: the seeds already run there as ordinary unit tests,
    # every time. This is the campaign, and it is unbounded work by design.
    #
    # A crasher is written to the package's testdata/fuzz/<Target>/ and is then
    # a permanent regression test. Commit it.
    set -euo pipefail
    failed=0
    for pkg in $(go list ./...); do
        targets=$(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true)
        for target in $targets; do
            echo "==> $target ($pkg)"
            go test "$pkg" -run "^${target}$" -fuzz "^${target}$" -fuzztime={{duration}} || failed=1
        done
    done
    if [ "$failed" -ne 0 ]; then
        echo "fuzzing found a crasher; the input is in the package's testdata/fuzz/ and should be committed" >&2
        exit 1
    fi

# List every fuzz target without running a campaign
fuzz-list:
    #!/usr/bin/env bash
    set -euo pipefail
    for pkg in $(go list ./...); do
        go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' | sed "s|^|${pkg#github.com/ethan-kane-ops/cellcast/}  |" || true
    done

# Test with coverage and fail below the gate
cover: envtest-assets
    #!/usr/bin/env bash
    # -coverpkg=./... rather than per-package coverage: the envtest suite in
    # internal/apitest holds no statements of its own and exercises the api/ and
    # controller code in other packages, which default coverage would not count.
    set -euo pipefail
    export KUBEBUILDER_ASSETS="$(go tool setup-envtest use {{envtest_k8s}} --bin-dir "$PWD/bin/envtest" -p path)"
    go test -coverpkg=./... -coverprofile=coverage.out ./...
    total=$(go tool cover -func=coverage.out | tail -1 | awk '{print $NF}' | tr -d '%')
    echo "total statement coverage: ${total}% (gate {{coverage_min}}%, stretch 80%)"
    awk -v got="$total" -v min={{coverage_min}} 'BEGIN {
        if (got + 0 < min + 0) {
            printf "coverage %.1f%% is below the %d%% gate\n", got, min > "/dev/stderr"
            exit 1
        }
    }'

# Run linters
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    # gofmt reports rather than rewrites. The pre-commit hook rewrites staged
    # files, which never sees a file that was committed misformatted before the
    # hook existed, and golangci-lint's default set does not include a
    # formatter.
    unformatted=$(gofmt -l . | grep -v '^site/' || true)
    if [ -n "$unformatted" ]; then
        echo "gofmt: not formatted, run 'gofmt -w' on:" >&2
        echo "$unformatted" >&2
        exit 1
    fi
    go vet ./...
    golangci-lint run

# Tidy go modules
tidy:
    go mod tidy

# Regenerate deepcopy methods
generate:
    go tool controller-gen object paths=./api/...

# Regenerate CRD manifests and refresh the copies the chart installs
manifests:
    #!/usr/bin/env bash
    set -euo pipefail
    go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases
    # Helm's .Files cannot reach outside the chart directory, so the chart holds
    # a copy of everything it installs that is generated or maintained
    # elsewhere. The copy is refreshed here and verified by verify-generate, so
    # a stale one is a failed gate rather than a chart that installs last
    # month's schema.
    cp config/crd/bases/*.yaml charts/cellcast/crd-bases/
    cp config/prometheus/prometheusrule.yaml charts/cellcast/files/prometheusrule.yaml

# Fail if generated output is stale (a hand-edited CRD is a silent correctness bug)
verify-generate: generate manifests
    #!/usr/bin/env bash
    set -euo pipefail
    # Only the paths `generate` and `manifests` write. The chart paths are the
    # copies `manifests` makes, not the whole chart: widening this to charts/
    # would report an edited template as stale generated output and send the
    # reader to a command that changes nothing. api/ is narrowed to the deepcopy
    # file for the same reason, one directory over: the hand-written types live
    # beside it, and comparing the directory fails on every uncommitted edit to
    # them, including the edit that this recipe exists to regenerate from.
    generated="api/*/zz_generated.deepcopy.go config/crd/ charts/cellcast/crd-bases/ charts/cellcast/files/"
    if ! git diff --quiet -- $generated; then
        echo "generated output is stale; run 'just generate manifests' and commit the result" >&2
        git diff --stat -- $generated >&2
        exit 1
    fi
    echo "generated output is up to date"

# Serve the docs site locally with live reload
docs-serve:
    uv run --with-requirements docs/requirements.txt mkdocs serve

# Build the static docs site into ./site
docs-build:
    uv run --with-requirements docs/requirements.txt mkdocs build --strict

# Regenerate CHANGELOG.md from Conventional Commits
changelog:
    git cliff -o CHANGELOG.md

# --- Release ------------------------------------------------------------------
#
# The release lives in these recipes and the workflow calls them, rather than
# the steps existing only inside a workflow file. A pipeline that cannot be run
# outside CI cannot be rehearsed before it is trusted, and this one publishes
# images that broker cluster credentials. `just release-check` runs every step
# and publishes nothing.
#
# Publishing order matters. Everything is verified before anything is pushed,
# and the GitHub release goes last, because it is the artifact a person reads
# and should not appear before the things it describes exist.

# Print the current digests for the base images the Dockerfile pins
image-bases:
    #!/usr/bin/env bash
    # The Dockerfile pins by digest, which is what makes a rebuild of an old tag
    # reproducible. Digests do not update themselves; this is how you find the
    # new one when you decide to move.
    set -euo pipefail
    for ref in golang:1.26-alpine gcr.io/distroless/static-debian12:nonroot; do
        docker pull -q "$ref" > /dev/null
        echo "$ref -> $(docker image inspect "$ref" | jq -r '.[0].RepoDigests[0]')"
    done

# Ensure a buildx builder that can emit a multi-platform manifest
_builder:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! docker buildx inspect {{builder}} > /dev/null 2>&1; then
        echo "creating buildx builder {{builder}} (the default driver cannot build multi-platform)"
        docker buildx create --name {{builder}} --driver docker-container --bootstrap > /dev/null
    fi

# Build one image for this machine's architecture and load it locally
image name tag=version:
    docker build \
        --build-arg BINARY={{name}} \
        --build-arg VERSION={{tag}} \
        --build-arg COMMIT={{commit}} \
        --build-arg DATE={{date}} \
        --tag {{registry}}/{{owner}}/{{name}}:{{tag}} .
    @echo "built {{registry}}/{{owner}}/{{name}}:{{tag}}"

# Build every published image for every platform, discarding the result
images-check tag=version: _builder
    #!/usr/bin/env bash
    # buildx cannot --load a multi-platform manifest into the local daemon, so
    # the local proof that both architectures compile is a build whose output
    # goes nowhere. Without this, a compile error on the architecture you do not
    # run is found by the release.
    set -euo pipefail
    for b in {{image_binaries}}; do
        echo "==> $b ({{platforms}})"
        docker buildx build --builder {{builder}} --platform {{platforms}} \
            --build-arg BINARY="$b" \
            --build-arg VERSION={{tag}} \
            --build-arg COMMIT={{commit}} \
            --build-arg DATE={{date}} \
            --output=type=cacheonly .
    done
    echo "every image builds for {{platforms}}"

# Build and push every published image as a multi-arch manifest
images-push tag: _builder
    #!/usr/bin/env bash
    # The tag is required rather than defaulted. `git describe` on an untagged
    # commit yields something like v0.1.0-4-gf0d8f81-dirty, and pushing that to
    # a public registry is not a mistake worth leaving one keystroke away.
    set -euo pipefail
    for b in {{image_binaries}}; do
        echo "==> pushing {{registry}}/{{owner}}/$b:{{tag}}"
        # BuildKit records the SBOM and the provenance during the build, from
        # what it actually compiled. An SBOM produced afterwards by scanning the
        # finished image is a guess at the same question, and a worse one:
        # a stripped static binary tells a scanner very little.
        docker buildx build --builder {{builder}} --platform {{platforms}} \
            --build-arg BINARY="$b" \
            --build-arg VERSION={{tag}} \
            --build-arg COMMIT={{commit}} \
            --build-arg DATE={{date}} \
            --sbom=true --provenance=mode=max \
            --tag {{registry}}/{{owner}}/$b:{{tag}} \
            --push .
    done

# Package both charts into dist/charts
chart-package:
    #!/usr/bin/env bash
    # No --version or --app-version override. Chart.yaml is the single place
    # those live, a contract test holds them in lockstep with the tag, and a
    # flag here would be a second answer that wins silently.
    set -euo pipefail
    rm -rf dist/charts
    mkdir -p dist/charts
    for c in {{chart_names}}; do
        helm package "charts/$c" --destination dist/charts
    done
    ls -1 dist/charts

# Push the packaged charts to the OCI registry
chart-push: chart-package
    #!/usr/bin/env bash
    set -euo pipefail
    for f in dist/charts/*.tgz; do
        echo "==> pushing $f to {{chart_repo}}"
        helm push "$f" {{chart_repo}}
    done

# Set the version both charts declare, and regenerate the changelog for it
release-version tag:
    #!/usr/bin/env bash
    # The chart version, the chart appVersion and the image tag are one number
    # cut from one commit. appVersion keeps the leading v because it is also the
    # default image tag; version drops it because Helm requires bare semver.
    set -euo pipefail
    tag="{{tag}}"
    if ! printf '%s' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
        echo "tag must be a v-prefixed semver, e.g. v0.3.0 (got '$tag')" >&2
        exit 1
    fi
    bare="${tag#v}"
    for c in {{chart_names}}; do
        f="charts/$c/Chart.yaml"
        sed -i.bak -E "s/^version: .*/version: $bare/" "$f"
        sed -i.bak -E "s/^appVersion: .*/appVersion: \"$tag\"/" "$f"
        rm -f "$f.bak"
        echo "$f -> version $bare, appVersion $tag"
    done
    # The README's verification block is the one command an adopter copies
    # verbatim, and a superseded tag in it verifies fine, which is the worst
    # way for it to be wrong. Each pattern names where a release number appears
    # rather than rewriting every triple in the file: a Go version is also
    # three numbers with dots in it.
    sed -i.bak -E \
        -e "s|^v[0-9]+\\.[0-9]+\\.[0-9]+\\.|$tag.|" \
        -e "s|(cellcast-hub:)v[0-9]+\\.[0-9]+\\.[0-9]+|\\1$tag|g" \
        -e "s|(refs/tags/)v[0-9]+\\.[0-9]+\\.[0-9]+|\\1$tag|g" \
        -e "s|(charts/cellcast:)[0-9]+\\.[0-9]+\\.[0-9]+|\\1$bare|g" \
        README.md
    rm -f README.md.bak
    echo "README.md -> verifies $tag"
    # The GitHub Action installs a release by number, and that number is what a
    # caller who pins nothing gets. The pattern is deliberately narrow: it is
    # the only default in the file that looks like a version.
    action=".github/actions/place/action.yml"
    sed -i.bak -E "s|^([[:space:]]*default: )v[0-9]+\\.[0-9]+\\.[0-9]+.*$|\\1$tag|" "$action"
    rm -f "$action.bak"
    echo "$action -> installs $tag"
    # The ref an example tells a caller to write in their own workflow. This
    # one is not a cosmetic staleness like a badge: a tag cut before the action
    # existed does not contain action.yml, so the ref does not resolve at all
    # and the documented three-line integration fails on its first run. The
    # files are found rather than listed, so a fourth example is covered the
    # day it is written.
    mapfile -t refs < <(git grep -lE 'actions/place@v[0-9]+\.[0-9]+\.[0-9]+' -- '*.md' '*.yml' '*.yaml')
    if [ ${#refs[@]} -gt 0 ]; then
        sed -i.bak -E "s|(actions/place@)v[0-9]+\.[0-9]+\.[0-9]+|\1$tag|g" "${refs[@]}"
        rm -f "${refs[@]/%/.bak}"
        echo "${refs[*]} -> use the action at $tag"
    fi
    # The client image an example runs as a container, which is what the Argo CD
    # hook does. The same staleness as the ref above and a quieter one: an old
    # client against a new hub goes on placing until the release that moves the
    # wire. Found rather than listed, for the same reason.
    mapfile -t images < <(git grep -lE '{{registry}}/{{owner}}/cellcast:v[0-9]+\.[0-9]+\.[0-9]+' -- '*.md' '*.yml' '*.yaml')
    if [ ${#images[@]} -gt 0 ]; then
        sed -i.bak -E "s|({{registry}}/{{owner}}/cellcast:)v[0-9]+\.[0-9]+\.[0-9]+|\1$tag|g" "${images[@]}"
        rm -f "${images[@]/%/.bak}"
        echo "${images[*]} -> run the client image at $tag"
    fi
    # The charts' READMEs carry the version in a badge, so they go stale on
    # every release unless they are regenerated here. A contract test holds
    # them to Chart.yaml, which is how that was found.
    just chart-docs
    git cliff --tag "$tag" -o CHANGELOG.md
    echo "changelog regenerated; review it, commit, then tag $tag"

# Show the version the next release would take, and what its notes would say
release-preview bump="auto":
    #!/usr/bin/env bash
    # Writes nothing and touches no branch.
    #
    # The number comes from the commits, not from somebody's memory: cliff.toml
    # sets features_always_bump_minor and breaking_always_bump_major, so the
    # conventional-commit prefixes the commit-msg hook already enforces are what
    # decide it. `auto` reads them; patch, minor and major override.
    set -euo pipefail
    echo "next: $(git cliff --bump {{bump}} --bumped-version)"
    echo "── notes ──"
    git cliff --bump {{bump}} --unreleased

# Open the pull request that sets the release version and changelog
release-prepare bump="auto":
    #!/usr/bin/env bash
    # Half of the release. This one stops at the pull request, because `main`
    # requires one and because the tag has to name the commit that reached
    # `main` rather than the branch it arrived on. `just release-tag` is the
    # other half, and it runs after the merge.
    set -euo pipefail
    fail() { echo "$1" >&2; exit 1; }

    [ -z "$(git status --porcelain)" ] || fail "working tree is dirty; a release commit must be reviewable"
    [ "$(git rev-parse --abbrev-ref HEAD)" = "main" ] || fail "run this from main; the release branch is cut here"
    git fetch -q origin main
    [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || fail "main is behind origin; pull first"

    case "{{bump}}" in
        v[0-9]*)                tag="{{bump}}" ;;
        auto|patch|minor|major) tag="$(git cliff --bump {{bump}} --bumped-version)" ;;
        *) fail "usage: just release-prepare [auto|patch|minor|major|vX.Y.Z]" ;;
    esac
    case "$tag" in v*) ;; *) tag="v$tag" ;; esac
    bare="${tag#v}"
    if git rev-parse -q --verify "refs/tags/$tag" > /dev/null; then
        fail "tag $tag already exists"
    fi

    git switch -c "release/$tag"
    just release-version "$tag"
    # After the bump, not before: release-version rewrites both Chart.yaml files
    # and regenerates the chart READMEs, and a contract test holds those to each
    # other. Checking first would check the previous release.
    just check
    git add -A
    git commit -m "chore(release): $tag"
    git push
    # A release pull request is read by the same stranger as any other one, so
    # the body describes the diff rather than the release procedure.
    body=$(printf '%s\n' \
        "## What changed" \
        "" \
        "- Both charts declare version $bare and appVersion $tag." \
        "- CHANGELOG.md gains the $tag section, generated from the commits since the last tag." \
        "- Both chart READMEs regenerate, because each carries its chart version in a badge." \
        "" \
        "## Why" \
        "" \
        "The chart version, the chart appVersion and the image tag are one number cut from one" \
        "commit. Moving them together in a single commit is what allows a release tag to name a" \
        "commit on which all three already agree." \
        "" \
        "## Testing" \
        "" \
        "\`just check\`, including the chart lint and the contract test that holds each chart" \
        "README to its Chart.yaml.")
    gh pr create --title "chore(release): $tag" --body "$body"
    echo
    echo "merge that, then: git switch main && git pull && just release-tag"

# Tag the release commit on main and push it, which starts the release workflow
release-tag:
    #!/usr/bin/env bash
    # The trigger, and the last thing done from a laptop. Everything after this
    # happens in the release workflow, which is not a preference: keyless
    # signatures carry the identity of whoever authenticated, and the identity
    # every published verify command names is that workflow's. A tag applied
    # here and a release cut here are not the same thing.
    #
    # The version is read from the chart rather than passed in, so this cannot
    # tag one number while the charts declare another.
    set -euo pipefail
    fail() { echo "$1" >&2; exit 1; }

    [ -z "$(git status --porcelain)" ] || fail "working tree is dirty"
    [ "$(git rev-parse --abbrev-ref HEAD)" = "main" ] || fail "a release tag belongs on main"
    git fetch -q origin main
    [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || fail "main is behind origin; pull first"

    tag=$(grep -E '^appVersion:' charts/cellcast/Chart.yaml | sed -E 's/^appVersion: *"?([^"]*)"?/\1/')
    [ -n "$tag" ] || fail "charts/cellcast/Chart.yaml declares no appVersion"
    if git rev-parse -q --verify "refs/tags/$tag" > /dev/null; then
        fail "tag $tag already exists; the release for it has already been cut"
    fi
    # The guard the release itself will apply, applied before the tag exists so
    # that a mismatch costs nothing to fix.
    for c in {{chart_names}}; do
        got=$(grep -E '^appVersion:' "charts/$c/Chart.yaml" | sed -E 's/^appVersion: *"?([^"]*)"?/\1/')
        [ "$got" = "$tag" ] || fail "charts/$c declares appVersion $got, not $tag; run 'just release-prepare $tag'"
    done

    echo "tagging $(git rev-parse --short HEAD) as $tag"
    # -s, not -a. commit.gpgsign covers commits and nothing else: an annotated
    # tag is a separate object with its own signature, governed by tag.gpgSign,
    # which defaults to false. The release ruleset's signature rule is satisfied
    # by the commit the tag points at, so an unsigned tag object passes it and
    # nothing anywhere says the tag itself is unsigned.
    #
    # The flag rather than the config, because the config belongs to whichever
    # machine happens to cut the release and the recipe belongs here.
    git tag -s "$tag" -m "$tag"
    git push origin "refs/tags/$tag"
    echo
    echo "release workflow started: https://github.com/{{owner}}/cellcast/actions/workflows/release.yml"
    echo "it runs check-all and the end-to-end suite before publishing anything."

# Print the release notes for one tag
release-notes tag=version:
    #!/usr/bin/env bash
    # Which git-cliff mode is right depends on whether the tag exists yet.
    # Before it does, the commits are unreleased and have to be labelled with
    # the tag they are about to be given. After it does, they are not unreleased
    # any more and --unreleased renders nothing, which is how a release gets
    # published with an empty body.
    set -euo pipefail
    if git rev-parse -q --verify "refs/tags/{{tag}}" > /dev/null; then
        git cliff --current --strip all
    else
        git cliff --tag "{{tag}}" --unreleased --strip all
    fi

# Every release step, publishing nothing
release-check: check
    #!/usr/bin/env bash
    # The rehearsal. Same builds, same packaging, same goreleaser config, no
    # registry and no GitHub release.
    set -euo pipefail
    goreleaser check
    # Signing is skipped, and only here. Snapshot mode does not skip it by
    # itself, and a keyless signature needs an OIDC flow that a rehearsal has no
    # business starting: it would either open a browser or fail. What the
    # rehearsal does check is that the sign configuration parses, which
    # `goreleaser check` above does.
    goreleaser release --snapshot --clean --skip=publish,sign
    just images-check
    just chart-package
    echo
    echo "release-check passed. Nothing was published."

# Sign every published image and chart with a keyless certificate
sign tag:
    #!/usr/bin/env bash
    # No key anywhere: the signer authenticates to Fulcio over OIDC, gets a
    # certificate valid for minutes, and the signature plus that certificate go
    # to Rekor and to the registry beside the artifact. There is nothing to
    # rotate and nothing to steal.
    #
    # --recursive so the per-architecture manifests are signed too, not just the
    # index. Somebody who pulls by platform digest is verifying a child.
    set -euo pipefail
    for b in {{image_binaries}}; do
        echo "==> signing {{registry}}/{{owner}}/$b:{{tag}}"
        cosign sign --yes --recursive "{{registry}}/{{owner}}/$b:{{tag}}"
    done
    bare="{{tag}}"
    bare="${bare#v}"
    for c in {{chart_names}}; do
        echo "==> signing {{registry}}/{{owner}}/charts/$c:$bare"
        cosign sign --yes "{{registry}}/{{owner}}/charts/$c:$bare"
    done

# Check every published artifact against the identity the docs tell adopters to expect
verify tag:
    #!/usr/bin/env bash
    # The same commands the README gives an adopter, run against the release
    # that was just published. If this fails, the artifacts are signed by
    # somebody other than the identity the documentation names, which is the
    # case where a signature is actively misleading rather than merely absent.
    set -euo pipefail
    identity="{{sign_workflow}}@refs/tags/{{tag}}"
    for b in {{image_binaries}}; do
        echo "==> verifying {{registry}}/{{owner}}/$b:{{tag}}"
        cosign verify "{{registry}}/{{owner}}/$b:{{tag}}" \
            --certificate-identity "$identity" \
            --certificate-oidc-issuer {{sign_issuer}} > /dev/null
    done
    bare="{{tag}}"
    bare="${bare#v}"
    for c in {{chart_names}}; do
        echo "==> verifying {{registry}}/{{owner}}/charts/$c:$bare"
        cosign verify "{{registry}}/{{owner}}/charts/$c:$bare" \
            --certificate-identity "$identity" \
            --certificate-oidc-issuer {{sign_issuer}} > /dev/null
    done
    echo "every published artifact is signed by $identity"

# Build one image and show the attestations it carries
verify-attestations name="cellcast-hub": _builder
    #!/usr/bin/env bash
    # The claim that every image ships an SBOM and build provenance, checked
    # rather than asserted. Built to an OCI layout instead of a registry so it
    # needs no credentials, and the predicates are read straight out of it.
    #
    # Not part of `check`: it needs docker.
    set -euo pipefail
    out=$(mktemp -d)
    trap 'rm -rf "$out"' EXIT
    docker buildx build --builder {{builder}} --platform linux/amd64 \
        --build-arg BINARY={{name}} \
        --sbom=true --provenance=mode=max \
        --output "type=oci,dest=$out/image.tar" .
    tar -xf "$out/image.tar" -C "$out"
    blob() { echo "$out/blobs/sha256/${1#sha256:}"; }
    index=$(jq -r '.manifests[0].digest' "$out/index.json")
    att=$(jq -r '.manifests[]|select(.annotations["vnd.docker.reference.type"]=="attestation-manifest")|.digest' "$(blob "$index")")
    if [ -z "$att" ]; then
        echo "{{name}} carries no attestation manifest" >&2
        exit 1
    fi
    found=$(jq -r '.layers[].annotations["in-toto.io/predicate-type"]' "$(blob "$att")" | sort)
    echo "$found"
    for want in https://slsa.dev/provenance/v1 https://spdx.dev/Document; do
        if ! grep -qx "$want" <<<"$found"; then
            echo "{{name}} carries no $want attestation" >&2
            exit 1
        fi
    done
    sbom=$(jq -r '.layers[]|select(.annotations["in-toto.io/predicate-type"]=="https://spdx.dev/Document")|.digest' "$(blob "$att")")
    echo "SBOM lists $(jq -r '.predicate.packages|length' "$(blob "$sbom")") packages"

# Cut a release: verify everything, then publish images, charts and the release
release tag: (_release-guard tag)
    #!/usr/bin/env bash
    set -euo pipefail
    # Verification first, all of it. A credential broker that shipped broken is
    # not something to fix forward: the bad image is already pulled.
    just check-all
    just verify-e2e
    # Then publish, least reversible last.
    just images-push "{{tag}}"
    just chart-push
    just sign "{{tag}}"
    # The notes go outside dist/. `goreleaser --clean` empties that directory
    # before it reads --release-notes, so a file written there is gone by the
    # time the release pipe wants it.
    notes=$(mktemp)
    trap 'rm -f "$notes"' EXIT
    just release-notes "{{tag}}" > "$notes"
    goreleaser release --clean --release-notes "$notes"
    # Last, and it can fail the release after everything is published, which is
    # deliberate. A release whose signatures do not match the documented
    # identity is one an adopter must not be told to trust, and finding that out
    # from this command is better than finding it out from an adopter.
    just verify "{{tag}}"
    echo "released {{tag}}"

# Refuse to release from a tree that is not exactly the tag
_release-guard tag:
    #!/usr/bin/env bash
    set -euo pipefail
    fail() { echo "$1" >&2; exit 1; }
    tag="{{tag}}"
    [ -z "$(git status --porcelain)" ] || fail "working tree is dirty; a release must be a commit somebody can check out"
    git rev-parse -q --verify "refs/tags/$tag" > /dev/null || fail "tag $tag does not exist"
    [ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] || fail "tag $tag does not point at HEAD"
    for c in {{chart_names}}; do
        got=$(grep -E '^appVersion:' "charts/$c/Chart.yaml" | sed -E 's/^appVersion: *"?([^"]*)"?/\1/')
        [ "$got" = "$tag" ] || fail "charts/$c declares appVersion $got, not $tag; run 'just release-version $tag'"
    done
    command -v goreleaser > /dev/null || fail "goreleaser is not installed"
    command -v git-cliff > /dev/null || fail "git-cliff is not installed"
    echo "release guard passed for $tag"

# Lint both charts and render them with every optional block turned on
chart-lint:
    #!/usr/bin/env bash
    set -euo pipefail
    helm lint charts/cellcast
    helm lint charts/cellcast-agent --set cellName=prod-euw1 --set hub.endpoint=https://hub.example.test
    # Rendering is the half that catches a template which lints and then fails
    # to produce valid YAML. Optional blocks are on, because a block nobody
    # rendered is a block nobody checked.
    helm template cellcast charts/cellcast \
        --set metrics.serviceMonitor.enabled=true \
        --set metrics.prometheusRule.enabled=true \
        --set networkPolicy.enabled=true \
        --set hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com \
        --set hub.oidc.issuers[0].provider=github > /dev/null
    helm template cellcast-agent charts/cellcast-agent \
        --set cellName=prod-euw1 \
        --set hub.endpoint=https://hub.example.test \
        --set hub.caConfigMap=hub-ca > /dev/null
    echo "charts lint and render"

# Regenerate the charts' values documentation
chart-docs:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v helm-docs > /dev/null; then
        echo "helm-docs is not installed; run 'mise install' in this repo" >&2
        exit 1
    fi
    helm-docs --chart-search-root charts --sort-values-order file

# Print what the charts would install, for reading before installing
chart-show chart="cellcast":
    helm template cellcast charts/{{chart}}         --set cellName=prod-euw1 --set hub.endpoint=https://hub.example.test

# Full local gate. Run before every commit.
check: tidy verify-generate lint chart-lint test

# Download the etcd and kube-apiserver binaries envtest runs against
envtest-assets:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "envtest assets: $(go tool setup-envtest use {{envtest_k8s}} --bin-dir "$PWD/bin/envtest" -p path)"

# Run the CRD and controller layer against a real API server
envtest: envtest-assets
    #!/usr/bin/env bash
    # Supersedes the throwaway kind cluster the old `verify-crds` recipe used.
    # envtest runs the same kube-apiserver and etcd binaries without the rest of
    # a cluster, so establishing the CRDs takes about a second instead of a
    # minute, and the suite can then assert what the schema actually enforces:
    # the validation markers, the defaults, both CEL rules on TrustConfig, and
    # the status subresource. A fake client accepts all of it regardless.
    set -euo pipefail
    export KUBEBUILDER_ASSETS="$(go tool setup-envtest use {{envtest_k8s}} --bin-dir "$PWD/bin/envtest" -p path)"
    go test ./internal/apitest/ -count=1 -v

# Mint a real credential against a throwaway cluster and check what it can do
verify-mint:
    #!/usr/bin/env bash
    # The minting path is the one place a bug hands out live cluster access, and
    # a fake client cannot check it: the TokenRequest API refuses any lifetime
    # under ten minutes, and a fake accepts whatever it is given. This spins up a
    # cluster, mints for real, and asserts the credential authenticates as the
    # configured service account and is bounded by that account's RBAC.
    #
    # Not part of `check`: it needs docker and takes about a minute.
    set -euo pipefail
    cluster=cellcast-mint-verify
    kubeconfig=$(mktemp)
    restore() {
        kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
        rm -f "$kubeconfig"
    }
    trap restore EXIT
    kind create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 90s
    export KUBECONFIG="$kubeconfig"
    kubectl apply -f config/crd/bases/
    kubectl create ns cellcast-system
    kubectl create ns apps
    kubectl -n apps create sa deployer
    # Deliberately narrow: the assertion is that the minted credential gets
    # exactly this and not the permissions the hub itself holds.
    kubectl -n apps create role deployer --verb=list,get,watch --resource=pods
    kubectl -n apps create rolebinding deployer --role=deployer --serviceaccount=apps:deployer
    CELLCAST_LIVE=1 go test ./internal/hub/broker/ -run TestLiveMint -v -count=1

# Place a workload end to end, then kill the hub and check every fallback stance
verify-e2e:
    #!/usr/bin/env bash
    # The whole product against one cluster: a caller identity, a policy, a
    # capacity report, a placement, a mint, and a kubeconfig that is then used
    # to talk to the cell it names. The assertion that matters is that the least
    # loaded cell in the fleet is one the caller may not reach, so a scoring pass
    # that ran before the permission filter would return it.
    #
    # The second test uses the same fixture with the hub stopped mid-pipeline,
    # then restarted. It checks each declared --on-unavailable stance and that
    # no stance answers an authorization refusal (docs/architecture.md ADR-006).
    #
    # Not part of `check`: it needs docker and takes about a minute.
    set -euo pipefail
    cluster=cellcast-e2e
    kubeconfig=$(mktemp)
    restore() {
        kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
        rm -f "$kubeconfig"
    }
    trap restore EXIT
    kind create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 90s
    export KUBECONFIG="$kubeconfig"
    kubectl apply -f config/crd/bases/
    kubectl create ns cellcast-system
    kubectl create ns apps
    kubectl -n apps create sa deployer
    kubectl -n apps create role deployer --verb=list,get,watch --resource=pods
    kubectl -n apps create rolebinding deployer --role=deployer --serviceaccount=apps:deployer
    just build
    CELLCAST_LIVE=1 go test ./internal/hub/ -run 'TestLiveEndToEnd|TestLiveFallback' -v -count=1

# Run three real cells with real agents and watch one drop out of scoring
verify-agent:
    #!/usr/bin/env bash
    # The only way to check most of the agent's behaviour. Three kind clusters,
    # each created with its own service account issuer, because the
    # agent binding is on the issuer and a fleet sharing one proves nothing
    # (docs/architecture.md ADR-009). A stock kind cluster issues as
    # https://kubernetes.default.svc.cluster.local, so every cluster here is
    # patched to issue as its own API server address and to serve OIDC
    # discovery anonymously, which is what a managed cluster does for free.
    #
    # Not part of `check`: it needs docker and takes several minutes.
    set -euo pipefail
    work=$(mktemp -d)
    clusters="cell-1 cell-2 cell-3"
    restore() {
        for c in $clusters; do
            kind delete cluster --name "cellcast-$c" >/dev/null 2>&1 || true
        done
        rm -rf "$work"
    }
    trap restore EXIT

    just build

    port=6451
    # Load is parked in ascending order, so cell-1 is the emptiest and is the
    # cell placement should choose. It is also the one whose agent gets killed.
    load=0
    entries=""
    for c in $clusters; do
        issuer="https://127.0.0.1:$port"
        kubeconfig="$work/$c.kubeconfig"

        {
            printf 'kind: Cluster\n'
            printf 'apiVersion: kind.x-k8s.io/v1alpha4\n'
            printf 'networking:\n'
            printf '  apiServerAddress: "127.0.0.1"\n'
            printf '  apiServerPort: %s\n' "$port"
            printf 'kubeadmConfigPatches:\n'
            printf '  - |\n'
            printf '    kind: ClusterConfiguration\n'
            printf '    apiServer:\n'
            printf '      extraArgs:\n'
            printf '        - name: service-account-issuer\n'
            printf '          value: %s\n' "$issuer"
            printf '        - name: service-account-jwks-uri\n'
            printf '          value: %s/openid/v1/jwks\n' "$issuer"
        } > "$work/$c.kind.yaml"

        kind create cluster --name "cellcast-$c" --config "$work/$c.kind.yaml" \
            --kubeconfig "$kubeconfig" --wait 120s
        export KUBECONFIG="$kubeconfig"

        # The hub verifies each agent's token against that cluster's key set,
        # so discovery has to be readable without already holding a token.
        kubectl create clusterrolebinding oidc-discovery \
            --clusterrole=system:service-account-issuer-discovery \
            --group=system:unauthenticated
        # ServiceAccount and RBAC only, rendered from the chart that ships so
        # the fixture cannot drift from what a real cell installs. The
        # Deployment is deliberately not applied: these agents run as host
        # processes against each cluster's kubeconfig, which is what lets the
        # test kill one.
        kubectl create ns cellcast-system
        helm template cellcast-agent charts/cellcast-agent \
            --namespace cellcast-system \
            --set cellName="$c" --set hub.endpoint=https://placeholder.invalid \
            --show-only templates/serviceaccount.yaml \
            --show-only templates/rbac.yaml \
            | kubectl apply -f -
        kubectl create ns apps
        kubectl -n apps create sa deployer

        # Parked load, so the scoring order is a property of the fixture rather
        # than of whichever cluster happened to be busier.
        if [ "$load" -gt 0 ]; then
            kubectl -n apps create deployment filler \
                --image=registry.k8s.io/pause:3.10 --replicas="$load"
            kubectl -n apps set resources deployment filler --requests=cpu=200m,memory=64Mi
        fi

        kubectl config view --raw --minify \
            -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
            | base64 -d > "$work/$c.ca.pem"
        kubectl -n cellcast-system create token cellcast-agent \
            --audience cellcast --duration 2h > "$work/$c.agent.jwt"
        kubectl -n apps create token deployer \
            --audience cellcast --duration 2h > "$work/$c.caller.jwt"

        entries="$entries{\"name\":\"$c\",\"kubeconfig\":\"$kubeconfig\",\"issuer\":\"$issuer\","
        entries="$entries\"ca\":\"$work/$c.ca.pem\",\"agentToken\":\"$work/$c.agent.jwt\","
        entries="$entries\"callerToken\":\"$work/$c.caller.jwt\",\"load\":$load},"

        port=$((port + 1))
        load=$((load + 4))
    done

    printf '[%s]\n' "${entries%,}" > "$work/fleet.json"

    # The first cell's cluster also stores the hub's registry.
    export KUBECONFIG="$work/cell-1.kubeconfig"
    kubectl apply -f config/crd/bases/

    CELLCAST_LIVE=1 CELLCAST_FLEET="$work/fleet.json" \
        go test ./internal/hub/ -run TestLiveAgentFleet -v -count=1 -timeout 15m

# Install both charts into a real cluster and check what they install stays up
verify-chart:
    #!/usr/bin/env bash
    # The question `helm template` and a server-side dry run cannot answer: does
    # the image the Dockerfile builds actually run under the chart the release
    # publishes.
    #
    # Everything between "renders" and "Ready" is untested by anything else. The
    # container runs as 65532 with a read-only root filesystem, and a binary
    # that wants to write anywhere crashloops rather than failing to render. The
    # probes name ports the process has to be listening on. The Role the chart
    # creates has to be enough for the manager's caches to sync, and a verb
    # missing from it looks like a replica that is simply never ready. None of
    # that is visible in rendered YAML, and all of it fails on an adopter's
    # first install.
    #
    # Not part of `check`: it needs docker and takes a few minutes.
    set -euo pipefail
    cluster=cellcast-chart
    ns=cellcast-system
    # Not a release tag. A cluster left behind after an interrupted run cannot
    # then be mistaken for one running a published image.
    tag=chart-verify
    kubeconfig=$(mktemp)

    on_exit() {
        status=$?
        if [ "$status" -ne 0 ] && [ -s "$kubeconfig" ]; then
            # A bare `helm --wait` timeout names nothing. This is the difference
            # between "it failed" and knowing which container said what.
            export KUBECONFIG="$kubeconfig"
            echo "=== pods ===" >&2
            kubectl -n "$ns" get pods -o wide >&2 || true
            echo "=== events ===" >&2
            kubectl -n "$ns" get events --sort-by=.lastTimestamp | tail -30 >&2 || true
            for pod in $(kubectl -n "$ns" get pods -o name 2>/dev/null); do
                echo "=== $pod ===" >&2
                kubectl -n "$ns" describe "$pod" | tail -25 >&2 || true
                kubectl -n "$ns" logs "$pod" --tail=40 --all-containers >&2 || true
            done
        fi
        kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
        rm -f "$kubeconfig"
    }
    trap on_exit EXIT

    kind create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 90s
    export KUBECONFIG="$kubeconfig"

    for b in cellcast-hub cellcast-agent; do
        just image "$b" "$tag"
        kind load docker-image --name "$cluster" "{{registry}}/{{owner}}/$b:$tag"
    done

    # Chart defaults everywhere they are not the point: three replicas, leader
    # election, the PodDisruptionBudget, and the CRDs the chart installs itself
    # rather than the copies in config/crd/bases.
    helm install cellcast charts/cellcast \
        --namespace "$ns" --create-namespace \
        --set image.tag="$tag" \
        --set hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com \
        --set hub.oidc.issuers[0].provider=github \
        --wait --timeout 5m

    helm install cellcast-agent charts/cellcast-agent \
        --namespace "$ns" \
        --set image.tag="$tag" \
        --set cellName=staging-euw1 \
        --set hub.endpoint=http://cellcast."$ns".svc:8080 \
        --wait --timeout 5m

    # The published examples, applied through the CRDs the chart installed.
    kubectl apply -f examples/

    # One wait for the whole control loop. Reaching CredentialMissing means the
    # hub process started, its controllers registered, its cache synced and the
    # Role the chart creates was enough to watch TrustConfigs and write their
    # status. A verb missing anywhere in that chain never gets this far.
    kubectl -n "$ns" wait --for=jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'=CredentialMissing \
        trustconfig/prod-euw1 --timeout=120s

    # Ready once is not the claim. A container that crashed and came back still
    # ships a chart that fails somebody's first install, and `helm --wait` is
    # satisfied either way.
    restarted=$(kubectl -n "$ns" get pods \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.containerStatuses[0].restartCount}{"\n"}{end}' \
        | grep -v '=0$' || true)
    if [ -n "$restarted" ]; then
        echo "containers restarted, so the chart installs something that does not stay up:" >&2
        echo "$restarted" >&2
        exit 1
    fi

    kubectl -n "$ns" get pods
    echo "both charts install, come up and stay up"

# Upgrade the previous release in place and check the fleet on it survives
verify-upgrade:
    #!/usr/bin/env bash
    # The path every adopter takes after the first install, and the one
    # verify-chart never takes: it installs onto an empty cluster. This installs
    # the newest release from the registry adopters pull it from, builds a fleet
    # on it at the API version that release served, upgrades to this tree with
    # the same values, and checks four things:
    #
    #   every object survives with its UID and no field lost or changed;
    #   placement gives the same answer before and after;
    #   after the upgrade every hub replica answers it, which each can only do
    #   by hearing the agent through the replica it reports to;
    #   the storage migration in docs/upgrading.md leaves nothing stored at
    #   v1alpha1, proven by removing that version and reading every object
    #   back, which is what a release that stops serving it will do.
    #
    # The agent stays on the previous release throughout. Cells are upgraded
    # after the hub and often much later, so an old agent reporting to a new
    # hub is the state a fleet spends longest in.
    #
    # Not part of `check`: it needs docker, pulls from GHCR and takes a few
    # minutes.
    set -euo pipefail
    cluster=cellcast-upgrade
    ns=cellcast-system
    tag=upgrade-verify
    # A stock kind cluster's service account issuer. The hub and the agent both
    # run inside the cluster, where this name resolves and the cluster CA
    # verifies it, so unlike verify-agent nothing here patches the issuer.
    issuer=https://kubernetes.default.svc.cluster.local
    resources="clusters placementpolicies trustconfigs workloadplacements"
    work=$(mktemp -d)
    kubeconfig="$work/kubeconfig"
    forwards=""

    on_exit() {
        status=$?
        if [ -n "$forwards" ]; then kill $forwards 2>/dev/null || true; fi
        if [ "$status" -ne 0 ] && [ -s "$kubeconfig" ]; then
            export KUBECONFIG="$kubeconfig"
            echo "=== pods ===" >&2
            kubectl -n "$ns" get pods -o wide >&2 || true
            for pod in $(kubectl -n "$ns" get pods -o name 2>/dev/null); do
                echo "=== $pod ===" >&2
                kubectl -n "$ns" logs "$pod" --tail=40 --all-containers >&2 || true
            done
        fi
        kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
        rm -rf "$work"
    }
    trap on_exit EXIT
    fail() { echo "$1" >&2; exit 1; }

    # Every cellcast object in every namespace, read at one version: identity,
    # UID and spec. Status is the hub's to rewrite and is left out.
    snapshot() {
        kubectl get "clusters.$1.cellcast.io,placementpolicies.$1.cellcast.io,trustconfigs.$1.cellcast.io" -A -o json \
            | jq -S '[.items[] | {kind, namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid, spec}]
                | sort_by(.kind, .namespace, .name)'
    }

    # A dry-run placement through each hub replica, leaving the chosen cell in
    # $cell. The agent reports to one replica, and each of the others hears it
    # only through that replica's relay (docs/high-availability.md), so
    # `place every` needs every replica to answer, all with the same cell.
    # `place any` is satisfied by one, for the release being upgraded from,
    # which may predate the relay. A replica that has not heard from the agent
    # answers 503 until it does; any other refusal is final.
    place() {
        local need=$1 targets="" total=0 answered="" got=0 port=18090 pod target code
        for pod in $(kubectl -n "$ns" get pods -o json \
                -l app.kubernetes.io/instance=cellcast,app.kubernetes.io/component=hub \
                | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name'); do
            kubectl -n "$ns" port-forward "pod/$pod" "$port:8080" > /dev/null 2>&1 &
            forwards="$forwards $!"
            targets="$targets $pod=$port"
            total=$((total + 1))
            port=$((port + 1))
        done
        [ "$total" -gt 0 ] || fail "found no hub replica to place through"
        if [ "$need" = every ]; then need=$total; else need=1; fi
        for _ in $(seq 1 60); do
            for target in $targets; do
                pod=${target%=*}
                case " $answered " in *" $pod "*) continue ;; esac
                code=$(curl -s -o "$work/placement-$pod.json" -w '%{http_code}' -H @"$work/auth" \
                    -d '{"workload":"upgrade-probe","dryRun":true}' \
                    "http://127.0.0.1:${target#*=}/api/v1/placement" || true)
                case "$code" in
                    200) answered="$answered $pod"; got=$((got + 1)) ;;
                    000|503) ;;
                    *) fail "$pod refused the placement with $code: $(cat "$work/placement-$pod.json")" ;;
                esac
            done
            if [ "$got" -ge "$need" ]; then break; fi
            sleep 2
        done
        kill $forwards 2>/dev/null || true
        wait $forwards 2>/dev/null || true
        forwards=""
        if [ "$got" -lt "$need" ]; then
            for target in $targets; do
                pod=${target%=*}
                case " $answered " in
                    *" $pod "*) ;;
                    *) echo "$pod never answered: $(cat "$work/placement-$pod.json" 2>/dev/null)" >&2 ;;
                esac
            done
            fail "$got of $total hub replicas answered the placement, want $need"
        fi
        cell=$(for pod in $answered; do jq -r .cell "$work/placement-$pod.json"; done | sort -u | paste -sd, -)
        case "$cell" in *,*) fail "the replicas chose different cells: $cell" ;; esac
    }

    kind create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 90s
    export KUBECONFIG="$kubeconfig"
    # The hub fetches the issuer's discovery document before it holds any token
    # to present, as it does for a CI platform's issuer.
    kubectl create clusterrolebinding oidc-discovery \
        --clusterrole=system:service-account-issuer-discovery \
        --group=system:unauthenticated

    # The same values at install and at upgrade, as an adopter's values file
    # carries them, so the chart's three hub replicas. The short warmup keeps
    # the rollout quick: a replica of the release being upgraded from may relay
    # nothing, and then a new replica hears from the agent only once the
    # agent's connection moves to a new one, or once warmup stops waiting.
    values=(
        --set "hub.oidc.issuers[0].url=$issuer"
        --set "hub.oidc.issuers[0].provider=generic"
        --set hub.oidc.caConfigMap=kube-root-ca.crt
        --set hub.warmupTimeout=20s
    )

    # No --version, so the newest release on the registry: the one an adopter
    # is upgrading from.
    helm install cellcast "{{chart_repo}}/cellcast" \
        --namespace "$ns" --create-namespace "${values[@]}" --wait --timeout 5m
    from=$(helm -n "$ns" list -o json | jq -r '.[] | select(.name == "cellcast") | .app_version')
    echo "installed $from from {{chart_repo}}"

    kubectl create ns apps
    kubectl -n apps create sa deployer
    printf 'Authorization: Bearer %s\n' \
        "$(kubectl -n apps create token deployer --audience cellcast --duration 1h)" > "$work/auth"

    # The fleet at v1alpha1, the version every release before the graduation
    # served, and with no stickiness on the policies: releases before it had no
    # such field, so the upgrade has to supply the default. The second policy
    # is outside the hub's namespace, so no controller ever rewrites it and
    # only the storage migration can move it off v1alpha1.
    #
    # Applied before the agent starts, which would otherwise be refused for a
    # cell that does not exist yet and back off for minutes.
    kubectl apply -f - <<EOF
    apiVersion: cellcast.io/v1alpha1
    kind: TrustConfig
    metadata:
      name: upgrade-cell
      namespace: $ns
    spec:
      provider: kubernetes
      credentialSource:
        inCluster: true
      kubernetes:
        serviceAccountName: deployer
        namespace: apps
    ---
    apiVersion: cellcast.io/v1alpha1
    kind: Cluster
    metadata:
      name: upgrade-cell
      namespace: $ns
      labels:
        env: upgrade
    spec:
      endpoint: https://kubernetes.default.svc
      provider: generic
      trustConfigRef:
        name: upgrade-cell
      reporter:
        issuer: $issuer
        subject: system:serviceaccount:$ns:cellcast-agent
    ---
    apiVersion: cellcast.io/v1alpha1
    kind: PlacementPolicy
    metadata:
      name: deployers
      namespace: $ns
    spec:
      subjects:
        - issuer: $issuer
          subject: system:serviceaccount:apps:deployer
      permittedCells:
        matchLabels:
          env: upgrade
    ---
    apiVersion: cellcast.io/v1alpha1
    kind: PlacementPolicy
    metadata:
      name: unwatched
      namespace: apps
    spec:
      subjects:
        - issuer: $issuer
          subject: system:serviceaccount:apps:deployer
      permittedCells:
        matchLabels:
          env: upgrade
    EOF

    helm install cellcast-agent "{{chart_repo}}/cellcast-agent" \
        --namespace "$ns" --version "${from#v}" \
        --set cellName=upgrade-cell \
        --set hub.endpoint=http://cellcast."$ns".svc:8080 \
        --set agent.heartbeatInterval=5s \
        --set agent.requestTimeout=3s \
        --wait --timeout 5m

    place any
    before=$cell
    [ "$before" = upgrade-cell ] || fail "placement chose $before before the upgrade, want upgrade-cell"
    snapshot v1alpha1 > "$work/before.json"
    echo "on $from: $(jq length "$work/before.json") objects, placement chose $before"

    just image cellcast-hub "$tag"
    kind load docker-image --name "$cluster" "{{registry}}/{{owner}}/cellcast-hub:$tag"
    helm upgrade cellcast charts/cellcast --namespace "$ns" "${values[@]}" \
        --set image.tag="$tag" --wait --timeout 10m

    # The upgrade patched the CRDs in place rather than leaving the previous
    # release's schema behind.
    for r in $resources; do
        storage=$(kubectl get crd "$r.cellcast.io" -o jsonpath='{.spec.versions[?(@.storage==true)].name}')
        [ "$storage" = v1beta1 ] || fail "$r.cellcast.io stores $storage after the upgrade, want v1beta1"
    done

    # Every field the fleet had, at the same value, on an object with the same
    # UID. A field the upgrade added is allowed: it is a default.
    snapshot v1beta1 > "$work/after.json"
    lost=$(jq -rn --slurpfile old "$work/before.json" --slurpfile new "$work/after.json" '
        ($new[0] | map({key: "\(.kind)/\(.namespace)/\(.name)", value: .}) | from_entries) as $now
        | $old[0][] as $o
        | "\($o.kind)/\($o.namespace)/\($o.name)" as $id
        | $now[$id] as $n
        | if $n == null then "\($id) is gone"
          elif $n.uid != $o.uid then "\($id) was recreated"
          else ($o.spec | paths(scalars)) as $p
            | select(($n.spec | getpath($p)) != ($o.spec | getpath($p)))
            | "\($id) spec.\($p | map(tostring) | join(".")) changed"
          end')
    [ -z "$lost" ] || fail "the upgrade lost or changed fields: $lost"
    modes=$(jq -r '[.[] | select(.kind == "PlacementPolicy") | .spec.stickiness.mode] | unique | join(",")' "$work/after.json")
    [ "$modes" = Preferred ] || fail "the policies read stickiness [$modes] after the upgrade, want the default Preferred"

    place every
    [ "$cell" = "$before" ] || fail "placement chose $cell after the upgrade and $before before it"
    restarted=$(kubectl -n "$ns" get pods \
        -o jsonpath='{range .items[*]}{.metadata.name}{"="}{.status.containerStatuses[0].restartCount}{"\n"}{end}' \
        | grep -v '=0$' || true)
    [ -z "$restarted" ] || fail "containers restarted after the upgrade: $restarted"
    echo "on this tree: every object intact, every hub replica chose $cell"

    # The migration docs/upgrading.md gives. Any write stores an object at the
    # storage version, and an empty merge patch is a write with nothing in it
    # to conflict with a controller's.
    for r in $resources; do
        echo "$r.cellcast.io was stored at $(kubectl get crd "$r.cellcast.io" -o jsonpath='{.status.storedVersions}')"
        kubectl get "$r.cellcast.io" -A -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
            | while read -r n name; do
                kubectl -n "$n" patch "$r.cellcast.io" "$name" --type=merge -p '{}' > /dev/null
            done
        kubectl patch crd "$r.cellcast.io" --subresource=status --type=merge \
            -p '{"status":{"storedVersions":["v1beta1"]}}' > /dev/null
    done

    # What a release that stops serving v1alpha1 will do. An object still
    # stored at v1alpha1 no longer decodes once its version is gone, so reading
    # everything back is the proof that the migration missed nothing. The test
    # operation makes the patch refuse to remove anything but v1alpha1.
    for r in $resources; do
        kubectl patch crd "$r.cellcast.io" --type=json -p \
            '[{"op":"test","path":"/spec/versions/0/name","value":"v1alpha1"},{"op":"remove","path":"/spec/versions/0"}]' > /dev/null
    done
    snapshot v1beta1 > "$work/migrated.json"
    diff -u "$work/after.json" "$work/migrated.json" || fail "objects changed across the storage migration"
    place every
    [ "$cell" = "$before" ] || fail "placement chose $cell once v1alpha1 was gone, and $before before"

    echo "upgraded $from to this tree: objects intact, placement unchanged, v1alpha1 removable"

# Scan for known vulnerabilities in code that is actually reachable
vuln:
    #!/usr/bin/env bash
    # Reachability, not a dependency list. govulncheck builds the call graph and
    # reports only advisories on paths this code can actually execute, so the
    # output is short enough that a non-empty one means something.
    #
    # Not in `check`. It fetches the advisory database, and a pre-commit gate
    # that fails when vuln.go.dev is slow teaches people to pass --no-verify.
    # It is in `check-all`, which `release` runs, so nothing ships with a known
    # reachable vulnerability.
    set -euo pipefail
    go tool govulncheck ./...

# Scan the built binaries rather than the source
vuln-binaries: build
    #!/usr/bin/env bash
    # The other question: not "can this module reach a vulnerability" but "does
    # the artifact somebody downloaded contain one". Binary mode reads the
    # module versions recorded in the binary itself, which is what an adopter
    # can run against a release without having the source.
    set -euo pipefail
    for b in {{binaries}}; do
        echo "==> bin/$b"
        go tool govulncheck -mode=binary "bin/$b"
    done

# Everything check does, plus vulnerabilities, the race detector and a real API server
check-all: check vuln test-race envtest

# Install pre-commit hooks into .git/hooks
hooks:
    pre-commit install
    @echo "pre-commit hooks installed"

# Run pre-commit against every file, not just staged ones
hooks-all:
    pre-commit run --all-files

# Remove build artifacts
clean:
    rm -rf bin/ dist/ coverage.out

# Install the client binary via `go install` and reshim so mise exposes it
install:
    go install -trimpath -ldflags '{{ldflags}}' ./cmd/cellcast
    mise reshim 2>/dev/null || true
    @echo "installed → $(which cellcast 2>/dev/null || go env GOBIN)/cellcast"

# --- Integration --------------------------------------------------------------
#
# The claim the project is pitched on is that cellcast attaches to a pipeline
# somebody already has. These two recipes are the halves of that claim a
# workflow cannot state for itself: the fleet it runs against, and the check
# that the app reached the cell cellcast named rather than simply reaching one.
#
# The fleet is the demo's, deliberately. A second three-cell builder would drift
# from the first, and the shape is already the one that makes the point: the
# emptiest cell in the estate is the one policy refuses.

# Build the demo fleet and start a hub that also trusts GitHub Actions
integration-up:
    #!/usr/bin/env bash
    set -euo pipefail
    ./demo/setup.sh
    # Applied before the hub is asked anything, and applied unedited, so an
    # example that has drifted from the schema fails here rather than failing
    # for whoever copied it.
    KUBECONFIG=demo/.work/euw1.kubeconfig \
        kubectl apply -f examples/integrations/github-actions/policy.yaml
    CELLCAST_EXTRA_ISSUERS="https://token.actions.githubusercontent.com=github" ./demo/up.sh

# Print what the hub and the agents logged, for a failed integration run
integration-logs:
    #!/usr/bin/env bash
    # A refusal is the interesting failure and the least visible one: the client
    # prints the reason and nothing else, and the reason alone does not say
    # which subject was presented or which claims a policy could have matched.
    # The audit record does, and it is written at info.
    set -euo pipefail
    # Tolerant on purpose. This runs because something already failed, and a
    # fleet that fell over before it wrote a log should not turn one failure
    # into two.
    shopt -s nullglob
    logs=(demo/.work/logs/*.log)
    if [ ${#logs[@]} -eq 0 ]; then
        echo "no logs; the fleet did not get far enough to write any"
        exit 0
    fi
    for log in "${logs[@]}"; do
        echo "=== $log ==="
        cat "$log"
    done

# Check the sample app reached the named cell and no other
integration-check cell:
    #!/usr/bin/env bash
    # Both halves. That the app is in the cell cellcast chose proves the
    # credential worked; that it is in neither of the others proves the
    # credential was scoped to one cell, which is the part a single positive
    # check would miss entirely.
    set -euo pipefail
    found=""
    for cell in euw1 use1 apse1; do
        if KUBECONFIG="demo/.work/$cell.kubeconfig" \
            kubectl -n apps get deployment sample-app > /dev/null 2>&1; then
            found="$found $cell"
        fi
    done
    found="${found# }"
    if [ "$found" != "{{ cell }}" ]; then
        echo "sample-app is in [$found]; cellcast placed it on {{ cell }}" >&2
        exit 1
    fi
    echo "sample-app is in {{ cell }} and nowhere else"

# Run the Argo CD PreSync hook in a real cell, LIVE and then DRAINING
integration-argo:
    #!/usr/bin/env bash
    # The hook is a Job, so checking it needs no Argo CD: applying it in a cell
    # and reading its exit code is exactly what a sync does before it starts.
    #
    # Both halves are the test. Succeeding while the cell is LIVE proves the
    # client image runs, the projected token reaches the hub and the one-cell
    # policy admits it. Failing once the cell is DRAINING is the whole reason
    # the hook is written as a gate, and a green run on its own says nothing
    # about it.
    set -euo pipefail
    fail() { echo "$1" >&2; exit 1; }

    cell=euw1
    ns=apps
    job="examples/integrations/argocd/presync-job.yaml"
    policy="examples/integrations/argocd/policy.yaml"
    export KUBECONFIG="demo/.work/$cell.kubeconfig"

    [ -f "demo/.work/$cell.issuer" ] || fail "no fleet; run: just integration-up"

    # The example pins a published release, because that is what a reader
    # copies. Building that same tag from this tree and loading it into the cell
    # is what makes this run exercise the client in this commit instead.
    tag=$(grep -oE 'cellcast:v[0-9]+\.[0-9]+\.[0-9]+' "$job" | head -1 | cut -d: -f2)
    [ -n "$tag" ] || fail "$job pins no client image"
    just image cellcast "$tag" > /dev/null
    kind load docker-image "{{registry}}/{{owner}}/cellcast:$tag" --name "cellcast-demo-$cell" > /dev/null

    # A pod reaches the hub across the kind network rather than over loopback,
    # which is why integration-up binds it to 0.0.0.0. Which address that is
    # depends on where the containers are.
    #
    # On a Linux runner they are on the machine the hub runs on, so the kind
    # network's gateway is the hub's host. The IPv4 one specifically: a
    # dual-stack network lists its IPv6 config first on some machines, and that
    # address has to be bracketed before it is a URL at all.
    #
    # On macOS the containers are inside a Colima VM and the hub is not, so that
    # same gateway is the VM, where nothing is listening. What a pod needs there
    # is the address the VM reaches the host at, which is the VM's default
    # route.
    case "$(uname -s)" in
        Darwin)
            hub_host=$(colima ssh -- ip route show default 2> /dev/null | awk '{print $3}' | head -1)
            ;;
        *)
            hub_host=$(docker network inspect kind | jq -r '[.[0].IPAM.Config[].Gateway | select(test("^[0-9.]+$"))] | first // ""')
            ;;
    esac
    [ -n "$hub_host" ] || fail "cannot work out the address a pod in a cell reaches the hub at"
    kubectl -n "$ns" create configmap cellcast-hook \
        --from-literal="hub=http://$hub_host:18080" \
        --dry-run=client -o yaml | kubectl apply -f - > /dev/null

    # One substitution, and a field rather than a line: a kind cluster issues as
    # its own API server address, and no test fleet can own example.com.
    # Everything else is applied as written, so an example that has drifted from
    # the schema stops the build here rather than stopping a reader.
    kubectl patch --local --type=json -f "$policy" -o yaml \
        -p "[{\"op\": \"replace\", \"path\": \"/spec/subjects/0/issuer\", \"value\": \"$(cat "demo/.work/$cell.issuer")\"}]" \
        | kubectl apply -f - > /dev/null

    run() {
        kubectl -n "$ns" delete job cellcast-presync --ignore-not-found --wait > /dev/null
        kubectl apply -f "$job" > /dev/null
        for _ in $(seq 1 60); do
            if [ "$(kubectl -n "$ns" get job cellcast-presync -o jsonpath='{.status.succeeded}')" = "1" ]; then
                echo succeeded
                return 0
            fi
            if [ -n "$(kubectl -n "$ns" get job cellcast-presync -o jsonpath='{.status.failed}')" ]; then
                echo failed
                return 0
            fi
            sleep 2
        done
        echo "never finished"
    }

    # --explain is on the hook, so this is every candidate cell and why it was
    # or was not chosen. It is the only explanation of a refusal anybody gets.
    hook_log() { kubectl -n "$ns" logs job/cellcast-presync 2>/dev/null || true; }

    echo "==> the hook, with $cell LIVE"
    got=$(run)
    log=$(hook_log)
    printf '%s\n' "$log"
    [ "$got" = "succeeded" ] || fail "the hook $got while $cell is LIVE; it would have stopped a sync that should have run"
    printf '%s\n' "$log" | grep -q "on $cell" \
        || fail "the hook succeeded but does not name $cell; the one-cell policy is not the thing being answered"

    echo "==> the hook, with $cell DRAINING"
    # Restored whatever happens: the fleet outlives this recipe, and a cell left
    # draining makes every later placement in the same run refuse for a reason
    # that has nothing to do with what is being tested.
    restore() { kubectl -n cellcast-system patch clusters.cellcast.io "$cell" --type=merge -p '{"spec":{"state":"LIVE"}}' > /dev/null 2>&1 || true; }
    trap restore EXIT
    kubectl -n cellcast-system patch clusters.cellcast.io "$cell" --type=merge -p '{"spec":{"state":"DRAINING"}}' > /dev/null
    got=$(run)
    log=$(hook_log)
    printf '%s\n' "$log"
    [ "$got" = "failed" ] || fail "the hook $got while $cell is DRAINING; the gate does not gate"
    # Failed, and failed for the right reason. A pod that cannot pull its image
    # or cannot reach the hub fails too, and either would read here as a gate
    # that works, which is the one way this check could pass while proving
    # nothing.
    printf '%s\n' "$log" | grep -q NoEligibleCells \
        || fail "the hook failed while $cell is DRAINING, but not with a refusal; something other than the gate stopped it"

    echo "the hook allowed the sync while $cell was LIVE and stopped it once $cell was DRAINING"

# --- Demo ---------------------------------------------------------------------
#
# The recording the site embeds. It runs against three real kind clusters with
# real agents rather than a stub, because the whole claim being made is that the
# placement and the credential are real, and a recording of a stub cannot show
# that. The scenario lives in demo/ so it can be rerun when the output format
# moves.

# Build the three-cell demo fleet and start the hub and agents
demo-setup:
    ./demo/setup.sh
    ./demo/up.sh

# Restart the hub and agents against a fleet that is already built
demo-up:
    ./demo/up.sh

# Stop the hub and agents, leaving the clusters up for another take
demo-stop:
    ./demo/down.sh --keep-clusters

# Run the demo scenario in this terminal
demo:
    ./demo/demo-cast.sh

# Record the demo scenario as an asciinema cast
demo-cast out="cellcast.cast":
    #!/usr/bin/env bash
    set -euo pipefail
    command -v asciinema > /dev/null || { echo "asciinema not installed" >&2; exit 1; }
    rm -f {{ out }}
    # 96 columns is what the site's player and its text fallback are sized for.
    asciinema rec {{ out }} \
        --window-size 96x30 \
        --idle-time-limit 2 \
        --command ./demo/demo-cast.sh
    @echo "recorded {{ out }} → copy it to the site's public/casts/"

# Stop the hub and agents and delete the demo clusters
demo-down:
    ./demo/down.sh

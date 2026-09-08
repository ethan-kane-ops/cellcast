# cellcast task runner.
#
# CI is intentionally dormant until the repository goes public (ENG-188), so
# these recipes and the pre-commit hooks are the verification layer. `just
# check` is the gate that must pass before every commit.

binaries := "cellcast cellcast-hub cellcast-agent"

# The control plane envtest runs. Kept in step with the k8s.io/* modules in
# go.mod: a CRD schema is validated by the API server that reads it, so testing
# against a different minor than the one the hub links proves less than it looks.
envtest_k8s := "1.37.x"

# Statement coverage floor. 80 is the stretch target (ENG-177); the gate is set
# where the suite actually is so that a drop is a signal rather than noise.
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
# the supported install for a person stays brew.
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

default:
    @just --list

# Build all three binaries into ./bin/
build:
    #!/usr/bin/env bash
    set -euo pipefail
    for b in {{binaries}}; do
        go build -trimpath -ldflags '{{ldflags}}' -o "bin/$b" "./cmd/$b"
        echo "built bin/$b"
    done

# Build a single binary, e.g. `just build-one cellcast-hub`
build-one name:
    go build -trimpath -ldflags '{{ldflags}}' -o bin/{{name}} ./cmd/{{name}}

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
    # The chart paths are the copies `manifests` writes, not the whole chart:
    # widening this to charts/ would report an edited template as stale
    # generated output and send the reader to a command that changes nothing.
    generated="api/ config/crd/ charts/cellcast/crd-bases/ charts/cellcast/files/"
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
# CI is dormant until the repository goes public (ENG-188), so the release lives
# in these recipes rather than in a workflow. That is the better shape anyway: a
# pipeline whose steps exist only inside a workflow file cannot be rehearsed
# before it is trusted, and this one publishes images that broker cluster
# credentials. `just release-check` runs every step and publishes nothing.
#
# Publishing order is deliberate. Everything is verified before anything is
# pushed, and the GitHub release goes last, because it is the artifact a person
# reads and it should not appear before the things it describes exist.

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
    # The charts' READMEs carry the version in a badge, so they go stale on
    # every release unless they are regenerated here. A contract test holds
    # them to Chart.yaml, which is how that was found.
    just chart-docs
    git cliff --tag "$tag" -o CHANGELOG.md
    echo "changelog regenerated; review it, commit, then tag $tag"

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
    # loaded cell in the fleet is deliberately one the caller may not reach, so
    # a scoring pass that ran before the permission filter would return it.
    #
    # The second test is ENG-175's done-when: the same fixture, with the hub
    # stopped mid-pipeline, then restarted. It checks each declared
    # --on-unavailable stance and that no stance answers an authorization
    # refusal (docs/architecture.md ADR-006).
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
    # ENG-174's done-when, and the only way to check most of it. Three kind
    # clusters, each created with its own service account issuer, because the
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

# Everything check does, plus the race detector and the real API server
check-all: check test-race envtest

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

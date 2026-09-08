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

version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
commit := `git rev-parse --short HEAD 2>/dev/null || echo none`
date := `date -u +%Y-%m-%dT%H:%M:%SZ`
pkg := "github.com/ethan-kane-ops/cellcast/internal/version"

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

# Regenerate CRD manifests
manifests:
    go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases

# Fail if generated output is stale (a hand-edited CRD is a silent correctness bug)
verify-generate: generate manifests
    #!/usr/bin/env bash
    set -euo pipefail
    if ! git diff --quiet -- api/ config/crd/; then
        echo "generated output is stale; run 'just generate manifests' and commit the result" >&2
        git diff --stat -- api/ config/crd/ >&2
        exit 1
    fi
    echo "generated output is up to date"

# Full local gate. Run before every commit.
check: tidy verify-generate lint test

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
        # Namespace, ServiceAccount and RBAC only. The Deployment is not
        # applied because these agents run as host processes against each
        # cluster's kubeconfig, which is what lets the test kill one.
        kubectl apply -f config/agent/00-namespace.yaml -f config/agent/10-rbac.yaml
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
    rm -rf bin/ coverage.out

# Install the client binary via `go install` and reshim so mise exposes it
install:
    go install -trimpath -ldflags '{{ldflags}}' ./cmd/cellcast
    mise reshim 2>/dev/null || true
    @echo "installed → $(which cellcast 2>/dev/null || go env GOBIN)/cellcast"

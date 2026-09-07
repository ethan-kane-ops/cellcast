# cellcast task runner.
#
# CI is intentionally dormant until the repository goes public (ENG-188), so
# these recipes and the pre-commit hooks are the verification layer. `just
# check` is the gate that must pass before every commit.

binaries := "cellcast cellcast-hub cellcast-agent"

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

# Test with coverage report
cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -1

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

# Prove the generated CRDs actually install into a real API server
verify-crds:
    #!/usr/bin/env bash
    # kubeconform cannot do this: its schema store has no CustomResourceDefinition
    # schema, so every invocation fails with "could not find schema" regardless of
    # the -kubernetes-version or -schema-location given. The only validation that
    # means anything is an API server accepting them, so this spins up a throwaway
    # kind cluster, applies the CRDs, waits for Established, and tears it down.
    #
    # Not part of `check`: it needs docker and takes about a minute. ENG-177
    # supersedes it with envtest, which does the same thing without the cluster.
    set -euo pipefail
    cluster=cellcast-crd-verify
    previous=$(kubectl config current-context 2>/dev/null || true)
    restore() {
        kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
        if [ -n "$previous" ]; then
            kubectl config use-context "$previous" >/dev/null 2>&1 || true
        fi
    }
    trap restore EXIT
    kind create cluster --name "$cluster" --wait 90s
    kubectl --context "kind-$cluster" apply -f config/crd/bases/
    kubectl --context "kind-$cluster" wait --for=condition=Established \
        --timeout=60s crd/clusters.cellcast.io crd/placementpolicies.cellcast.io \
        crd/trustconfigs.cellcast.io
    echo "CRDs install and reach Established"

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

# Place a workload end to end and use the credential that comes back
verify-e2e:
    #!/usr/bin/env bash
    # The whole product against one cluster: a caller identity, a policy, a
    # capacity report, a placement, a mint, and a kubeconfig that is then used
    # to talk to the cell it names. The assertion that matters is that the least
    # loaded cell in the fleet is deliberately one the caller may not reach, so
    # a scoring pass that ran before the permission filter would return it.
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
    CELLCAST_LIVE=1 go test ./internal/hub/ -run TestLiveEndToEnd -v -count=1

# Everything check does, plus the race detector (CRDs: see `just verify-crds`)
check-all: check test-race

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

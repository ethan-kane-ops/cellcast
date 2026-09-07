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

# Everything check does, plus the race detector
#
# CRDs are not schema-validated here: kubeconform's schema store has no
# CustomResourceDefinition schema, and the validation that actually matters is
# installing them into a real API server. That lands with envtest in ENG-177.
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

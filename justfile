default:
    @just --list

# Build the binary into ./bin/ (isolated — does not affect the installed binary)
build:
    go build -o bin/cellcast ./cmd/cellcast

# Run the locally built binary — safe during development, never touches the installed version
run *args: build
    ./bin/cellcast {{args}}

# Run all tests
test:
    go test ./...

# Run tests with race detector
test-race:
    go test -race ./...

# Run linters
lint:
    go vet ./...
    golangci-lint run

# Tidy go modules
tidy:
    go mod tidy

# Tidy + lint + test
check: tidy lint test

# Remove build artifacts
clean:
    rm -rf bin/

# Install binary via `go install` and reshim so mise exposes it immediately
install:
    go install ./cmd/cellcast
    mise reshim 2>/dev/null || true
    @echo "installed → $(which cellcast 2>/dev/null || go env GOBIN)/cellcast"

# Cut a release
release version:
    git tag v{{version}}
    git push origin v{{version}}
    gh release create v{{version}} --generate-notes --draft

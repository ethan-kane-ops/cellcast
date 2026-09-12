# syntax=docker/dockerfile:1

# One Dockerfile for all three binaries; BINARY selects which one.
#
# One file rather than three because the build flags are a correctness concern
# rather than a per-image preference: -trimpath and the version stamping have to
# be identical across the hub and the agent, or the reproducibility claim on the
# tag holds only for whichever image was built last.

# Pinned by digest, not by tag. A tag is a moving target, and "reproducible
# builds" that resolve their own compiler at build time are not reproducible.
# Both digests are refreshed by hand; `just image-bases` prints the current
# ones for the two tags below.
ARG GO_IMAGE=golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# --platform=$BUILDPLATFORM keeps the compiler on the builder's own
# architecture and cross-compiles from there. Emulating the toolchain under
# QEMU to produce an arm64 binary costs minutes and buys nothing: CGO is off, so
# Go cross-compiles for free.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

# GOTOOLCHAIN=local turns a base image older than go.mod into a build failure
# instead of a silent toolchain download. The download would succeed, and the
# resulting image would have been compiled by a toolchain nothing in this
# repository pins.
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG BINARY
ARG TARGETOS
ARG TARGETARCH
# Defaults match internal/version, so an unstamped image reports what it is
# rather than claiming a version it does not have.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build <<'EOF'
set -eu
if [ ! -d "./cmd/${BINARY}" ]; then
    echo "BINARY must name a directory under cmd/, got '${BINARY}'" >&2
    exit 1
fi
# grpcnotrace keeps html/template out of the binary: the justfile's go_tags
# says why, and TestServerBuildsLeaveOutGRPCTrace holds the two together.
GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -tags grpcnotrace \
    -ldflags "-s -w \
        -X github.com/ethan-kane-ops/cellcast/internal/version.version=${VERSION} \
        -X github.com/ethan-kane-ops/cellcast/internal/version.commit=${COMMIT} \
        -X github.com/ethan-kane-ops/cellcast/internal/version.date=${DATE}" \
    -o /out/entrypoint "./cmd/${BINARY}"
EOF

FROM ${RUNTIME_IMAGE}

ARG BINARY
ARG VERSION
ARG COMMIT

LABEL org.opencontainers.image.title="${BINARY}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.source="https://github.com/ethan-kane-ops/cellcast" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="Multi-cluster deployment placement oracle and short-lived credential broker"

COPY --from=build /out/entrypoint /entrypoint

# 65532 is distroless' nonroot user, and the charts pin the same uid. The image
# is usable without a securityContext and identical with one.
USER 65532:65532

# Exec form, and a fixed path rather than the binary's own name.
#
# The path is fixed because ENTRYPOINT cannot expand a build argument. The form
# is exec because a shell form would make /bin/sh PID 1, and SIGTERM would stop
# at the shell: the hub's whole drain sequence (docs/architecture.md ADR-011)
# depends on the process itself receiving that signal. distroless has no shell
# to fall back to, so this fails loudly rather than draining nothing.
ENTRYPOINT ["/entrypoint"]

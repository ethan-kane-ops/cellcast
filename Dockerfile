FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /bin/cellcast ./cmd/cellcast

FROM gcr.io/distroless/static-debian12:latest
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/cellcast"
LABEL org.opencontainers.image.description="Multi-cluster deployment placement oracle and short-lived credential broker"
COPY --from=build /bin/cellcast /cellcast
ENTRYPOINT ["/cellcast"]

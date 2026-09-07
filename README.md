# cellcast

Multi-cluster deployment placement oracle and short-lived credential broker.

Your pipeline asks two questions on every deploy: **where does this go**, and **what credential lets
me deploy it there**. cellcast answers both, and then forgets the answer. It slots into the CI/CD you
already run instead of replacing it.

- **Not a propagation engine.** No GitOps loop to adopt, no manifests to hand over. Open Cluster
  Management, Karmada and Rancher Fleet all answer the placement question only after you adopt their
  control plane. cellcast is a queryable oracle you bolt onto the pipeline you have.
- **Not a credential store.** It holds trust configuration and mints on demand. There is no
  long-lived downstream credential anywhere in the system to steal.

> **Read the [threat model](docs/threat-model.md) first.** This is a tool that mints cluster
> credentials. The security argument, its limits, and the risks accepted for v0.1 are written down
> rather than implied.

## Status

Pre-v0.1. Design is settled and recorded; implementation is in progress. Not usable yet.

## Documentation

| | |
| --- | --- |
| [Architecture](docs/architecture.md) | System shape, the placement path, and eight decision records with the alternatives that were rejected |
| [Threat model](docs/threat-model.md) | Trust boundaries, seven threats with mitigations, and the risks explicitly accepted for v0.1 |

## Requirements

- Go 1.22+
- [mise](https://mise.jdx.dev/) — runtime manager (`brew install mise`)
- [just](https://just.systems/) — task runner (managed by mise)

## Getting started

```bash
mise install        # install pinned Go + tools
just build          # compile → bin/cellcast
./bin/cellcast --help
```

## Development

```bash
just check          # tidy + lint + test (run before every commit)
just test           # go test ./...
just lint           # go vet + golangci-lint
just build          # compile
just install        # install to $GOBIN
```

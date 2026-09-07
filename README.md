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

## Components

Three binaries, deployed in two places. The split is a security boundary: the
agent runs in every registered cell and does not contain the minting code.

| Binary | Runs in | Job |
| --- | --- | --- |
| `cellcast-hub` | the hub cluster | API, controllers, the only component that can mint |
| `cellcast-agent` | every registered cell | reports capacity on a heartbeat |
| `cellcast` | the pipeline runner | client CLI |

## Requirements

- Go 1.26+
- [mise](https://mise.jdx.dev/) - runtime manager (`brew install mise`)
- [just](https://just.systems/) - task runner (managed by mise)

## Getting started

```bash
mise install        # install pinned Go + tools
just build          # compile all three binaries into bin/
./bin/cellcast --help
```

## Development

```bash
just check          # tidy + verify-generate + lint + test (run before every commit)
just check-all      # the above, plus the race detector
just generate       # regenerate deepcopy from api/
just manifests      # regenerate CRD manifests from api/
just hooks          # install the pre-commit hooks
```

`api/v1alpha1` is the source of truth for the CRDs. Everything in
`config/crd/bases/` is generated; `just check` fails on a stale diff.

Continuous integration is deliberately dormant until this repository is public.
`just check` and the pre-commit hooks are the verification layer in the meantime.

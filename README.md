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
| [Architecture](docs/architecture.md) | System shape, the placement path, and ten decision records with the alternatives that were rejected |
| [Threat model](docs/threat-model.md) | Trust boundaries, eight threats with mitigations, and the risks explicitly accepted for v0.1 |
| [Audit trail](docs/audit.md) | What is recorded for every placement and mint, and worked queries over it |
| [Metrics](docs/metrics.md) | The Prometheus surface, the dashboard, and the four alerts that matter |

## Components

Three binaries, deployed in two places. The split is a security boundary: the
agent runs in every registered cell and does not contain the minting code.

| Binary | Runs in | Job |
| --- | --- | --- |
| `cellcast-hub` | the hub cluster | API, controllers, the only component that can mint |
| `cellcast-agent` | every registered cell | reports capacity on a heartbeat, under its own projected ServiceAccount token |
| `cellcast` | the pipeline runner | client CLI |

## What happens when cellcast is down

cellcast sits in the deploy critical path, so this is the first question worth an answer. A
placement is a recommendation, not a command, and the pipeline declares up front what should happen
when there is no answer to be had:

| `--on-unavailable` | Behaviour |
| --- | --- |
| `fail` (the default) | Exit non-zero. For a pipeline that must not guess |
| `last-known` | Reuse the last cell the hub chose for this workload, if it is still inside `--cache-ttl` (default one hour) |
| `<cell>` | Use a cell pinned in advance |

Nothing falls back implicitly. An implicit fallback is how a deploy silently lands in the wrong
cluster.

**A fallback returns a cell and never a credential.** Minting runs through the hub, so a hub that
cannot be reached cannot issue one either. `last-known` is worth having for a pipeline that needs
only the cell name (`--dry-run`, to pick a values file or a target it already holds access to), or
one that keeps a break-glass credential for exactly this situation. A pipeline with neither cannot
deploy through a hub outage, and no flag will change that.

**A refusal is not an outage.** If the hub answers that the caller is not permitted, no stance
applies and the command fails. Falling back there would make `--on-unavailable` the way around the
policy engine. If the hub answers that it cannot work out which of the caller's permitted cells is
least loaded, a stance may answer it: the cost is a suboptimal cell, never an unauthorised one.

Every result names its `source` (`hub`, `cache` or `pinned`) and its `confidence`, in both output
modes, so a pipeline branches on `cellcast place --json | jq -r .source` rather than on whether a
file happened to appear. On a fallback the kubeconfig at the target path is removed, so a credential
left by an earlier run cannot be picked up by the next step.

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
just check-all      # the above, plus the race detector and the real API server
just envtest        # the CRD and controller layer against a real kube-apiserver
just cover          # statement coverage, failing below the 70% gate
just generate       # regenerate deepcopy from api/
just manifests      # regenerate CRD manifests from api/
just hooks          # install the pre-commit hooks
```

`just envtest` runs `internal/apitest` against a real kube-apiserver and etcd,
downloaded on first use into `bin/envtest`. It is where the CRD schema is
actually tested: the fake client the unit tests use accepts objects the API
server refuses, so the validation markers, the defaults, the two CEL rules on
`TrustConfig` and the status subresource are only checked here. The same suite
also runs the hub's real controllers under a manager, which is what proves the
watches fire at all.

Without the downloaded assets the suite skips rather than fails, so a fresh
clone can still run `just check`.

Three recipes verify against real clusters instead of fakes. They need docker and
take a few minutes each, so they sit outside `just check`:

```bash
just verify-mint    # a real credential is minted and is bounded by its RBAC
just verify-e2e     # a placement end to end, then the hub dies and every fallback stance is checked
just verify-agent   # three cells, three agents, and one dropping out of scoring
```

`api/v1alpha1` is the source of truth for the CRDs. Everything in
`config/crd/bases/` is generated; `just check` fails on a stale diff.

Continuous integration is deliberately dormant until this repository is public.
`just check` and the pre-commit hooks are the verification layer in the meantime.

# cellcast

[![CI](https://github.com/ethan-kane-ops/cellcast/actions/workflows/ci.yml/badge.svg)](https://github.com/ethan-kane-ops/cellcast/actions/workflows/ci.yml)
[![e2e](https://github.com/ethan-kane-ops/cellcast/actions/workflows/e2e.yml/badge.svg)](https://github.com/ethan-kane-ops/cellcast/actions/workflows/e2e.yml)
[![Release](https://github.com/ethan-kane-ops/cellcast/actions/workflows/release.yml/badge.svg)](https://github.com/ethan-kane-ops/cellcast/actions/workflows/release.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/ethan-kane-ops/cellcast/badge)](https://securityscorecards.dev/viewer/?uri=github.com/ethan-kane-ops/cellcast)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go Reference](https://pkg.go.dev/badge/github.com/ethan-kane-ops/cellcast.svg)](https://pkg.go.dev/github.com/ethan-kane-ops/cellcast)
[![golangci-lint](https://img.shields.io/badge/linted%20by-golangci--lint-00ADD8?logo=go&logoColor=white)](https://golangci-lint.run)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-%E2%89%A5%201.28-blue.svg)](https://kubernetes.io)
[![Docs](https://img.shields.io/badge/docs-mkdocs--material-blue.svg)](https://ethan-kane-ops.github.io/cellcast/)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/cellcast)](https://artifacthub.io/packages/search?repo=cellcast)

**Multi-cluster deployment placement oracle and short-lived credential broker.**

A deploy pipeline asks two questions: which cluster does this workload go to, and what credential
gets it there. cellcast answers both and stores neither. It attaches to an existing pipeline
instead of replacing it.

```console
$ cellcast place --workload checkout-api --ttl 15m
placed checkout-api on prod-euw1 (policy app-prod, LeastLoaded, confidence high)
credential valid for 15m
kubeconfig written to cellcast.kubeconfig (0600, as apps/deployer)

$ kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

No secret was configured and none was stored. The pipeline authenticated with the OIDC token its CI
platform already issues, and the credential it received expires before the build log finishes
uploading.

> **Read the [threat model](docs/threat-model.md) before deploying this.** cellcast mints cluster
> credentials. The security argument, its limits, and the risks accepted are written down rather
> than implied.

## Status

v0.3.0. The placement path, the broker, the audit trail, metrics, multi-replica HA, both charts,
the release pipeline and the pipeline integrations are built and tested. Pre-1.0, the API and the
CRD schema may change between minor versions, and [building from
source](#building-from-source) stays supported.

## What it is not

- **Not a propagation engine.** No GitOps loop to adopt, no manifests to hand over. Open Cluster
  Management, Karmada and Rancher Fleet answer the placement question only after their control
  plane is adopted. cellcast is a queryable oracle attached to an existing pipeline.
- **Not a credential store.** It holds trust configuration and mints on demand. No long-lived
  downstream credential exists anywhere in the system.
- **Not authoritative.** It answers where and hands back a credential. It does not template, apply,
  roll back, or watch.

## Components

Three binaries, deployed in two places. The split is a security boundary: the agent runs in every
registered cell and does not contain the minting code.

| Binary | Runs in | Job |
| --- | --- | --- |
| `cellcast-hub` | the hub cluster | API, controllers, the only component that can mint |
| `cellcast-agent` | every registered cell | reports capacity on a heartbeat, under its own projected ServiceAccount token |
| `cellcast` | the pipeline runner | client CLI |

## Installing it

One command per cluster. In the hub cluster:

```bash
helm install cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast \
  --namespace cellcast-system --create-namespace \
  --set hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com \
  --set hub.oidc.issuers[0].provider=github
```

Then in each cell, once it has been registered as a `Cluster`:

```bash
helm install cellcast-agent oci://ghcr.io/ethan-kane-ops/charts/cellcast-agent \
  --namespace cellcast-system --create-namespace \
  --set cellName=prod-euw1 \
  --set hub.endpoint=https://cellcast.example.com
```

The issuer is not optional in practice. A hub with none authenticates nobody and refuses every
request. That is the correct state for a broker that cannot tell who is asking, and it is not a
working install. Both charts document every value they take:
[hub](charts/cellcast/README.md), [agent](charts/cellcast-agent/README.md).

## When the hub is unavailable

A placement is a recommendation, not a command. The pipeline declares up front what happens when
there is no answer, and the default is to fail:

| `--on-unavailable` | Behaviour |
| --- | --- |
| `fail` (the default) | Exit non-zero. For a pipeline that must not guess |
| `last-known` | Reuse the last cell chosen for this workload, if it is still inside `--cache-ttl` |
| `<cell>` | Use a cell pinned in advance |

Nothing falls back implicitly, and **a fallback returns a cell but never a credential**. Minting
runs through the hub, so a hub that cannot be reached cannot issue one. `last-known` therefore
suits a pipeline that needs only the cell name, or one holding a break-glass credential for this
situation. A pipeline with neither cannot deploy through a hub outage.

A refusal is not an outage. If the hub answers that the caller is not permitted, no stance applies
and the command fails; falling back there would make the flag a route around the policy engine.

Every result names its `source` (`hub`, `cache` or `pinned`) and its `confidence` in both output
modes, so a pipeline branches on `cellcast place --json | jq -r .source` rather than on whether a
file appeared. On a fallback the kubeconfig at the target path is removed, so a credential left by
an earlier run cannot be picked up by the next step.

**The hub itself runs more than one replica.** Every replica answers placements, because deciding
reads and never writes; only the controllers take a lease. A replica reports itself unready until
it has heard capacity for the fleet, and on shutdown it reports unready and keeps serving while
Kubernetes takes it out of the Service, so a rolling update of cellcast does not fail the deploys
running through it. The details, including the deadlock that bounds the warmup wait, are in
[ADR-011](docs/architecture.md#adr-011-the-api-path-runs-n-replicas-and-only-the-controllers-elect).

## When the answer is no

A refusal is not an outage, and no stance answers one. `NoPolicy` is the refusal that needs
explaining, because the caller authenticated perfectly well and was still turned away.
`cellcast policy test` runs the same decision without minting and prints what the hub read out of the
token:

```console
$ cellcast policy test --workload checkout-api
refused: NoPolicy
  no placement policy permits this caller

the hub read this token as:
  issuer   https://token.actions.githubusercontent.com
  subject  repo:acme@56138094/app@1332281435:pull_request
  claims   ref=refs/heads/main
           repository=acme/app
```

Compare those with the policy's `spec.subjects`. The commonest cause is a subject written in a
format the issuer does not produce: every GitHub repository created after 15 July 2026 issues the
immutable form shown above.

## Verifying what you install

Images and charts are signed with [cosign](https://docs.sigstore.dev/) keyless signing. There is no
long-lived key: the signer authenticates over OIDC, receives a certificate valid for minutes, and
both signature and certificate are recorded in a public transparency log. A signature alone
therefore proves nothing. Verification has to name the expected signer, which for this project is
its own release workflow:

```bash
cosign verify ghcr.io/ethan-kane-ops/cellcast-hub:v0.3.0 \
  --certificate-identity https://github.com/ethan-kane-ops/cellcast/.github/workflows/release.yml@refs/tags/v0.3.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same command covers the agent image, the client image and both charts
(`ghcr.io/ethan-kane-ops/charts/cellcast:0.3.0`). For the client archives, verify the checksum file
and then check the archive against it:

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.bundle \
  --certificate-identity https://github.com/ethan-kane-ops/cellcast/.github/workflows/release.yml@refs/tags/v0.3.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
shasum -a 256 --ignore-missing -c checksums.txt
```

Every image carries an SBOM and SLSA build provenance, recorded by the builder from what it
compiled rather than inferred afterwards by scanning a stripped static binary:

```bash
docker buildx imagetools inspect ghcr.io/ethan-kane-ops/cellcast-hub:v0.3.0 --format '{{ json .SBOM }}'
```

Pin by digest in production regardless. A tag can be moved; a digest names the same bytes
tomorrow, and both charts accept `image.digest`, which wins over `image.tag`. `just release` runs
the commands above against what it has just published and fails if the signing identity is not the
one printed here. See [SECURITY.md](./SECURITY.md) and [docs/releasing.md](docs/releasing.md).

## Documentation

| | |
| --- | --- |
| [Docs site](https://ethan-kane-ops.github.io/cellcast/) | Getting started, concepts, operations, troubleshooting |
| [Architecture](docs/architecture.md) | System shape, the placement path, and eleven decision records with the alternatives that were rejected |
| [Threat model](docs/threat-model.md) | Trust boundaries, eight threats with mitigations, and the risks explicitly accepted |
| [Extending](docs/extending.md) | Adding a CI platform the hub does not know, or a second way to mint |
| [Examples](examples/) | A complete four-cell fleet and three policies, applyable as they are |
| [Pipeline integrations](examples/integrations/) | A GitHub Action, an Argo CD PreSync hook and a Buildkite step, each with its policy |
| [Audit trail](docs/audit.md) | What is recorded for every placement and mint, and worked queries over it |
| [Metrics](docs/metrics.md) | The Prometheus surface, the dashboard, and the five alerts |
| [Hub chart](charts/cellcast/README.md) | Installing the hub, and every value it takes |
| [Agent chart](charts/cellcast-agent/README.md) | Installing the reporter in a cell, and the identity that has to match |

## Building from source

- Go 1.26+
- [mise](https://mise.jdx.dev/), runtime manager (`brew install mise`)
- [just](https://just.systems/), task runner (managed by mise)

```bash
mise install        # install pinned Go + tools
just build          # compile all three binaries into bin/
./bin/cellcast --help
```

## Development

```bash
just                # list every recipe
just check          # tidy + verify-generate + lint + chart-lint + test
just check-all      # the above, plus vulnerability scanning, the race detector and envtest
```

`just check` must pass before every commit. The full set of recipes, what the test layers cover,
and how to run against real clusters are in [docs/development.md](docs/development.md).
Contributing: [CONTRIBUTING.md](./CONTRIBUTING.md). Support: [SUPPORT.md](./SUPPORT.md).
Planned and rejected work: [ROADMAP.md](./ROADMAP.md).

## License

Apache License 2.0. See [LICENSE](./LICENSE).

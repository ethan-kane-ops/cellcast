# cellcast

**Multi-cluster deployment placement oracle and short-lived credential broker.**

Your pipeline asks two questions on every deploy: **where does this go**, and **what credential lets
me deploy it there**. cellcast answers both, and then forgets the answer. It slots into the CI/CD you
already run instead of replacing it.

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

## When cellcast is down

The first question worth an answer for anything in the deploy critical path, so it is answered here
rather than discovered.

A placement is a recommendation, not a command. The pipeline declares up front what happens when
there is no answer, and the default is to fail:

| `--on-unavailable` | Behaviour |
| --- | --- |
| `fail` (the default) | Exit non-zero. For a pipeline that must not guess |
| `last-known` | Reuse the last cell chosen for this workload, if it is still inside `--cache-ttl` |
| `<cell>` | Use a cell pinned in advance |

Nothing falls back implicitly, and **a fallback returns a cell but never a credential**. A refusal is
not an outage: if the hub says the caller is not permitted, no stance applies and the command fails.
Falling back there would make the flag a way around the policy engine.

## Verifying what you install

Images and charts are signed with [cosign](https://docs.sigstore.dev/) keyless signing. There is
no long-lived key: the signer authenticates over OIDC, gets a certificate valid for minutes, and
the signature and that certificate are recorded in a public transparency log. So the useful
question is not whether an artifact is signed, it is **who signed it**, and the only answer this
project will accept is its own release workflow:

```bash
cosign verify ghcr.io/ethan-kane-ops/cellcast-hub:v0.3.0 \
  --certificate-identity https://github.com/ethan-kane-ops/cellcast/.github/workflows/release.yml@refs/tags/v0.3.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same command covers the agent image, the client image and both charts
(`ghcr.io/ethan-kane-ops/charts/cellcast:0.3.0`). For the client archives, verify the checksum
file and then check the archive against it:

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.bundle \
  --certificate-identity https://github.com/ethan-kane-ops/cellcast/.github/workflows/release.yml@refs/tags/v0.3.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
shasum -a 256 --ignore-missing -c checksums.txt
```

Every image also carries an SBOM and SLSA build provenance, recorded by the builder from what it
actually compiled rather than guessed afterwards by scanning a stripped static binary:

```bash
docker buildx imagetools inspect ghcr.io/ethan-kane-ops/cellcast-hub:v0.3.0 --format '{{ json .SBOM }}'
```

Pin by digest in production regardless. A tag can be moved; a digest names the same bytes
tomorrow, and both charts accept `image.digest`, which wins over `image.tag`.

Nothing is published yet, so those commands describe the first tagged release rather than
something you can run today. They are not aspirational: `just release` runs them itself against
what it has just published, and fails if the signing identity is not the one above. See
[SECURITY.md](./SECURITY.md).

> **Read the [threat model](docs/threat-model.md) before deploying this.** It mints cluster
> credentials. The security argument, its limits, and the risks explicitly accepted are written down
> rather than implied.

## Status

v0.2. The placement path, the broker, the audit trail, metrics, multi-replica HA, the charts and
the release pipeline are built. Nothing is published to a registry yet, so the install commands
above describe where the artifacts will be rather than where they are; until the first tag,
installing means [building from source](#building-from-source) and `just release-check` is what
proves the pipeline that will put them there.

## What it is not

- **Not a propagation engine.** No GitOps loop to adopt, no manifests to hand over. Open Cluster
  Management, Karmada and Rancher Fleet all answer the placement question only after you adopt their
  control plane. cellcast is a queryable oracle you bolt onto the pipeline you have.
- **Not a credential store.** It holds trust configuration and mints on demand. There is no
  long-lived downstream credential anywhere in the system to steal.
- **Not authoritative.** It answers where and hands back a credential. It does not template, apply,
  roll back, or watch.

## Documentation

| | |
| --- | --- |
| [Docs site](https://ethan-kane-ops.github.io/cellcast/) | Getting started, concepts, operations, troubleshooting |
| [Architecture](docs/architecture.md) | System shape, the placement path, and eleven decision records with the alternatives that were rejected |
| [Threat model](docs/threat-model.md) | Trust boundaries, eight threats with mitigations, and the risks explicitly accepted |
| [Audit trail](docs/audit.md) | What is recorded for every placement and mint, and worked queries over it |
| [Metrics](docs/metrics.md) | The Prometheus surface, the dashboard, and the five alerts that matter |
| [Hub chart](charts/cellcast/README.md) | Installing the hub, and every value it takes |
| [Agent chart](charts/cellcast-agent/README.md) | Installing the reporter in a cell, and the identity that has to match |

## Components

Three binaries, deployed in two places. The split is a security boundary: the
agent runs in every registered cell and does not contain the minting code.

| Binary | Runs in | Job |
| --- | --- | --- |
| `cellcast-hub` | the hub cluster | API, controllers, the only component that can mint |
| `cellcast-agent` | every registered cell | reports capacity on a heartbeat, under its own projected ServiceAccount token |
| `cellcast` | the pipeline runner | client CLI |

## Fallback behaviour in detail

**A fallback returns a cell and never a credential.** Minting runs through the hub, so a hub that
cannot be reached cannot issue one either. `last-known` is worth having for a pipeline that needs
only the cell name (`--dry-run`, to pick a values file or a target it already holds access to), or
one that keeps a break-glass credential for exactly this situation. A pipeline with neither cannot
deploy through a hub outage, and no flag will change that.

Every result names its `source` (`hub`, `cache` or `pinned`) and its `confidence`, in both output
modes, so a pipeline branches on `cellcast place --json | jq -r .source` rather than on whether a
file happened to appear. On a fallback the kubeconfig at the target path is removed, so a credential
left by an earlier run cannot be picked up by the next step.

**The hub itself runs more than one replica.** Every replica answers placements, because deciding
reads and never writes; only the controllers take a lease. A replica reports itself unready until it
has heard capacity for the fleet, and on shutdown it reports unready and keeps serving while
Kubernetes takes it out of the Service, so a rolling update of cellcast does not fail the deploys
running through it. The details, including the deadlock that bounds the warmup wait, are
[ADR-011](docs/architecture.md#adr-011-the-api-path-runs-n-replicas-and-only-the-controllers-elect).

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

The issuer is not optional in practice. A hub with none authenticates nobody and
refuses every request, which is the correct state for a broker that cannot tell
who is asking, and is not a working install. Both charts document every value
they take: [hub](charts/cellcast/README.md), [agent](charts/cellcast-agent/README.md).

## Building from source

- Go 1.26+
- [mise](https://mise.jdx.dev/) - runtime manager (`brew install mise`)
- [just](https://just.systems/) - task runner (managed by mise)

```bash
mise install        # install pinned Go + tools
just build          # compile all three binaries into bin/
./bin/cellcast --help
```

Contributing: [CONTRIBUTING.md](./CONTRIBUTING.md). Support: [SUPPORT.md](./SUPPORT.md).
What is planned and what has been decided against: [ROADMAP.md](./ROADMAP.md).

## Development

```bash
just check          # tidy + verify-generate + lint + chart-lint + test (run before every commit)
just check-all      # the above, plus the race detector and the real API server
just envtest        # the CRD and controller layer against a real kube-apiserver
just cover          # statement coverage, failing below the 70% gate
just fuzz           # fuzz every target in turn (just fuzz 10m for a campaign)
just generate       # regenerate deepcopy from api/
just manifests      # regenerate CRD manifests from api/
just hooks          # install the pre-commit hooks
```

`just fuzz` runs ten targets over the code that reads bytes somebody else
chose: the JWT parsing that happens before anything is verified, the placement
and registration request bodies, the label selector a policy is written in, and
the capacity arithmetic a spoke's agent drives. They assert more than "it does
not crash": claim extraction may never emit a claim the provider does not
declare or flatten a JSON object into something a policy could match on, a
refusal may only carry a reason the client contract defines, a registration
carrying credential material may never be stored, and a report the index accepts
may never score as NaN. Seeds run as ordinary unit tests on every `just check`.

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

## License

Apache License 2.0. See [LICENSE](./LICENSE).

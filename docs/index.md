# cellcast

cellcast answers one question for a deploy pipeline: **which of my clusters
should this go to, and what credential may I use to get there?**

It is a placement oracle and a credential broker, and not a deploy engine. The
pipeline keeps whatever it already uses to apply manifests.

```console
$ cellcast place --workload checkout-api --ttl 15m
placed checkout-api on prod-euw1 (policy app-prod, LeastLoaded, confidence high)
credential valid for 15m
kubeconfig written to cellcast.kubeconfig (0600, as apps/deployer)
```

## What it replaces

The long-lived kubeconfig sitting in a CI secret. Every pipeline that deploys to
a cluster has one, and rotating them across an estate is a project of its own.
cellcast replaces it with a credential minted at the moment of the deploy,
scoped to one namespace and one service account in one cell, expiring before the
build log finishes uploading.

The pipeline authenticates with the OIDC token its CI platform already issues.
No secret is stored anywhere.

## The three things it does

**Decides.** A `PlacementPolicy` says which callers may reach which cells. The
survivors are ranked on capacity that in-cluster agents report. Filtering
happens before scoring, always, so the least-loaded cell in the estate is never
returned to a caller not permitted to reach it.

**Mints.** The hub holds trust configuration, never credentials. It exchanges
that configuration for a short-lived token against the chosen cell at the moment
it is asked, and never stores the result.

**Records.** One structured JSON line per placement and per mint, carrying the
caller's identity, the policy that matched, every cell considered and why each
was excluded, and a hash of the token issued. Never the token.

## What happens when cellcast is down

A placement is a recommendation, not a command, and the pipeline declares up
front what should happen when there is no answer:

| `--on-unavailable` | Behaviour |
|---|---|
| `fail` (the default) | Exit non-zero. For a pipeline that must not guess |
| `last-known` | Reuse the last cell chosen for this workload, if it is still inside `--cache-ttl` |
| `<cell>` | Use a cell pinned in advance |

Nothing falls back implicitly, and a fallback returns a cell but never a
credential. A refusal is not an outage: if the hub says the caller is not
permitted, no stance applies and the command fails. Falling back there would
make the flag a way around the policy engine.

## Where to go next

- **[Getting started](getting-started.md)**: a hub and two cells, one command each.
- **[Architecture](architecture.md)**: the system shape and eleven decision records, each with the alternatives that were rejected.
- **[Threat model](threat-model.md)**: trust boundaries, the threats, and the risks explicitly accepted.
- **[Placement policy](placement-policy.md)**: writing a policy, and seeing what it does before relying on it.
- **[Extending](extending.md)**: adding a CI platform the hub does not know, or a second way to mint.
- **[Examples](https://github.com/ethan-kane-ops/cellcast/tree/main/examples)**: a complete four-cell fleet and the policies over it, applyable as they are.

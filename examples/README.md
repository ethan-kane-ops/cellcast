# Examples

A complete fleet, in the three objects cellcast reads. Apply it against a hub
and it works; change the endpoints, the issuers and the labels and it is yours.

```bash
kubectl apply -f examples/
```

Every object goes in the hub's namespace, `cellcast-system` by default. These
are validated against a real API server by `TestTheExamplesAreAcceptedByTheAPIServer`,
so an example that has drifted from the CRD schema fails `just envtest` rather
than failing for a reader.

## What the fleet is

| Cell | State | Labels | Cell of |
|---|---|---|---|
| `prod-euw1` | LIVE | `env=prod region=euw1` | production, taking deploys |
| `prod-use1` | LIVE | `env=prod region=use1` | production, taking deploys |
| `prod-apse1` | DARK | `env=prod region=apse1` | a new region, reachable only on request |
| `staging-euw1` | LIVE | `env=staging region=euw1` | the hub's own cluster |

Three policies over it: production pinned to two regions, a smoke test that may
target the dark cell by name, and a staging policy that spreads evenly across
whatever is labelled `env: staging`.

## The files

| File | What it declares |
|---|---|
| [`trustconfigs.yaml`](trustconfigs.yaml) | How the hub authenticates to each cell in order to mint for it |
| [`clusters.yaml`](clusters.yaml) | The cells themselves, their state, and which agent may report for each |
| [`policies.yaml`](policies.yaml) | Which callers reach which cells, and how the survivors are ranked |

## What you have to supply

Three of the four `TrustConfig` objects reference a Secret holding a kubeconfig
for that cell. cellcast never contains a credential, so those Secrets are
created out of band and are not in this directory:

```bash
kubectl -n cellcast-system create secret generic prod-euw1-admin \
  --from-file=kubeconfig=./prod-euw1.kubeconfig
```

The fourth, `staging-euw1`, uses `inCluster: true` instead, which is the shape
to copy when the hub's own cluster is also a cell.

## What to change first

- **`spec.endpoint`** on each `Cluster`. It has to be reachable from the hub.
- **`spec.reporter.issuer`**, which must be that cell's real service account
  issuer. It is how the hub tells one cell's agent from another, so two cells
  sharing an issuer cannot be distinguished and the second one is refused. A
  stock kind or kubeadm cluster issues as
  `https://kubernetes.default.svc.cluster.local` and needs
  `--service-account-issuer` set before it can be registered.
- **`spec.subjects`** in every policy. The examples name a GitHub Actions
  repository and branch. An issuer-only selector would permit every repository
  on GitHub, so replace the subject rather than deleting it.
- **The issuers the hub itself trusts.** A policy naming an issuer does not add
  it. The hub verifies signatures only against the issuers it was started with:

  ```bash
  --oidc-issuer https://token.actions.githubusercontent.com=github
  ```

## Checking a change before anything relies on it

```bash
cellcast place --workload checkout-api --dry-run --explain
```

`--dry-run` runs the whole decision, policy and capacity included, and stops
before minting. `--explain` prints every cell and why it was or was not chosen,
which is how a policy edit gets reviewed rather than guessed at.

## Related

- [Placement policy](../docs/placement-policy.md) for the full field reference.
- [Getting started](../docs/getting-started.md) for installing the hub and an agent first.
- [Extending](../docs/extending.md) for adding a CI platform or a credential provider.

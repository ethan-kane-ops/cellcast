# Getting started

A hub and two cells, one command per cluster, then a placement.

## What you need

- Two or more Kubernetes clusters. One is the **hub**, which holds the registry
  and mints; the others are **cells**, which receive deploys. The hub cluster can
  also be a cell.
- `helm` and `kubectl`.
- A CI platform that issues OIDC tokens (GitHub Actions, GitLab, Buildkite), or
  any other OIDC issuer under your control.

!!! warning "Each cell needs its own service account issuer"

    The hub tells one cell's agent from another by the `iss` claim of its
    token. Managed providers give every cluster a distinct issuer URL already.
    A stock kubeadm or kind cluster issues as
    `https://kubernetes.default.svc.cluster.local`, and every one of them
    collides, so those clusters need `--service-account-issuer` set before they
    can be told apart. See [ADR-009](architecture.md).

## 1. Install the hub

```bash
helm install cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast \
  --namespace cellcast-system --create-namespace \
  --set hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com \
  --set hub.oidc.issuers[0].provider=github
```

The issuer is not optional in practice. A hub with none authenticates nobody and
refuses every request. That is the correct state for a broker that cannot tell
who is asking, and it is not a working install.

## 2. Tell the hub how to reach a cell

Two objects per cell. A `TrustConfig` says how the hub authenticates to it, and
a `Cluster` registers it.

The `TrustConfig` references a credential; it never contains one:

```yaml
apiVersion: cellcast.io/v1alpha1
kind: TrustConfig
metadata:
  name: prod-euw1
  namespace: cellcast-system
spec:
  provider: kubernetes
  credentialSource:
    # A Secret in the hub namespace holding a kubeconfig for this cell. Use
    # `inCluster: true` instead when the cell is the hub's own cluster.
    secretRef:
      name: prod-euw1-admin
      key: kubeconfig
  kubernetes:
    # What gets minted: a token for this service account, in this namespace,
    # in that cell. Nothing broader is reachable through cellcast.
    serviceAccountName: deployer
    namespace: apps
```

Then the `Cluster`:

```yaml
apiVersion: cellcast.io/v1alpha1
kind: Cluster
metadata:
  name: prod-euw1
  namespace: cellcast-system
  labels:
    env: prod
    region: euw1
spec:
  endpoint: https://prod-euw1.example.com
  provider: eks
  state: LIVE
  trustConfigRef:
    name: prod-euw1
  # Who may publish capacity for this cell. Unset means nobody, not anybody.
  reporter:
    issuer: https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE
    subject: system:serviceaccount:cellcast-system:cellcast-agent
```

Check both were accepted:

```console
$ kubectl -n cellcast-system get trustconfig,cluster
NAME                                PROVIDER     READY   DETAIL
trustconfig.cellcast.io/prod-euw1   kubernetes   True

NAME                            PROVIDER   STATE   ACCEPTING   ENDPOINT
cluster.cellcast.io/prod-euw1   eks        LIVE    True        https://prod-euw1.example.com
```

`READY: False` on the `TrustConfig` means the hub could not find the Secret or
the service account it names. Fixing it here costs less than finding it during a
deploy.

## 3. Install the agent in each cell

```bash
helm install cellcast-agent oci://ghcr.io/ethan-kane-ops/charts/cellcast-agent \
  --namespace cellcast-system --create-namespace \
  --set cellName=prod-euw1 \
  --set hub.endpoint=https://cellcast.example.com
```

`cellName` must match the `Cluster` above. Within one heartbeat interval the
hub is scoring the cell.

Until a cell reports, it has no capacity, so it is **excluded** from scoring
rather than treated as empty. A naive least-loaded ranking reads a missing entry
as zero load, which is the best possible score, so the first cluster to break
badly enough that its agent stopped reporting would become the target for every
deploy in the estate.

## 4. Say who may deploy where

Nothing is permitted until a policy says so.

```yaml
apiVersion: cellcast.io/v1alpha1
kind: PlacementPolicy
metadata:
  name: app-prod
  namespace: cellcast-system
spec:
  # Which callers this policy is about. Matched on the token's claims.
  subjects:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:acme/checkout:ref:refs/heads/main
  # Which cells they may reach. A label selector over registered Clusters.
  permittedCells:
    matchLabels:
      env: prod
  strategy: LeastLoaded
  tokenTTL:
    default: 15m
    max: 30m
```

## 5. Place something

From the pipeline, with its identity token in `CELLCAST_TOKEN`:

```console
$ cellcast place --hub https://cellcast.example.com \
    --workload checkout-api --dry-run --explain
would place checkout-api on prod-euw1 (policy app-prod, LeastLoaded, confidence high)

CELL        ADMITTED  STAGE       REASON                                       UTILISATION
prod-euw1   yes                                                                0.42
prod-euw2   yes                                                                0.77
dev-euw1    no        permission  cell is not permitted by this caller's policy 0.01
```

`--dry-run` runs the whole decision and stops before minting, and `--explain`
shows every cell and why it was or was not chosen. Running it before the first
real deploy is how a policy that permits more than intended gets caught while it
still costs nothing.

Drop both flags and cellcast writes a kubeconfig for the existing deploy step:

```console
$ cellcast place --hub https://cellcast.example.com --workload checkout-api --ttl 15m
placed checkout-api on prod-euw1 (policy app-prod, LeastLoaded, confidence high)
credential valid for 15m
kubeconfig written to cellcast.kubeconfig (0600, as apps/deployer)

$ kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

## Next

- [Placement policy](placement-policy.md), including what a policy cannot do.
- [Token brokering](token-brokering.md): what is minted, and what bounds it.
- [Pipeline integration](pipeline-integration.md): wiring this into CI.
- [Troubleshooting](troubleshooting.md) when a placement is refused.

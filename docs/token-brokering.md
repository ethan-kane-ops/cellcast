# Token brokering

The hub holds **trust configuration**, never credentials. What it stores is
"here is how to reach cell X and what to ask for"; what it hands back is a token
minted seconds earlier and expiring shortly after.

## What gets minted

For a Kubernetes cell, a `TokenRequest` for one service account in one
namespace:

```yaml
apiVersion: cellcast.io/v1alpha1
kind: TrustConfig
metadata:
  name: prod-euw1
  namespace: cellcast-system
spec:
  provider: kubernetes
  credentialSource:
    secretRef:
      name: prod-euw1-admin
      key: kubeconfig
  kubernetes:
    serviceAccountName: deployer
    namespace: apps
```

The credential handed to the caller can do exactly what `apps/deployer` can do
in that cell, for the granted lifetime. **Whatever you grant that service
account is what cellcast can hand out**, so it is the object worth reviewing,
not this one.

`credentialSource` takes one of two forms and never both:

| Form | Used when |
|---|---|
| `secretRef` | The cell is a different cluster. The Secret holds a kubeconfig for it |
| `inCluster: true` | The cell is the hub's own cluster |

## What bounds the lifetime

Three limits, applied in order, smallest wins:

1. **What the caller asked for** (`--ttl`). May be shorter than the default;
   never longer than the policy's `max`.
2. **The policy's `tokenTTL.max`**.
3. **The hub's `--token-max-ttl`**, which no policy can raise.

A clamped request is not silent. The response says what was granted, what was
asked for, and what capped it:

```console
credential valid for 30m (requested 2h, capped by policy at 30m)
```

A deploy that assumed it had two hours would otherwise fail partway through
rather than at the start, which is the more expensive failure.

!!! note "Why the hub's own ceiling exists"

    A Kubernetes API server applies no maximum of its own unless the operator
    set `--service-account-max-token-expiration`. A default cluster will issue a
    token lasting years if asked. `--token-max-ttl` is the bound that is certain
    to exist, which is why it is a real control rather than defence in depth.

There is also a floor the hub does not choose: the `TokenRequest` API refuses
anything under ten minutes. A policy with a `max` below that admits placements
and can never mint for them, so the hub rejects that configuration at startup
rather than at deploy time.

## What the hub does not do

**It does not store what it minted.** The token goes into the response and
nowhere else. There is no record to steal later, and no way to recover a
credential after the fact.

**It does not log it.** Not the token, not a prefix. The audit record carries a
SHA-256 digest for correlation, and a test asserts no field on that record can
hold credential material.

**It does not cache the credential it authenticates with.** The Secret behind a
`TrustConfig` is read uncached at the moment it is used and not retained. Trust
configuration is cached because it is read on every mint and changes rarely;
the material that authenticates the hub to a spoke is not.

**It never mints on a fallback.** A cached placement is a cached decision, never
a cached credential. A hub that cannot be reached cannot mint, and no client
flag changes that.

## Verifying trust configuration before you need it

The hub reports whether a `TrustConfig` could mint, checked when you write it
rather than when a pipeline depends on it:

```console
$ kubectl -n cellcast-system get trustconfig
NAME        PROVIDER     READY   DETAIL
prod-euw1   kubernetes   True
prod-euw2   kubernetes   False   secret "prod-euw2-admin" has no key "kubeconfig"
```

It checks configuration, not permission. Whether `apps/deployer` can actually do
anything useful in that cell is that cluster's RBAC to answer, and asking would
mean the hub holding permission to introspect it.

## If the hub is compromised

Stated plainly because it is the question a reviewer asks first.

Whoever controls the hub's ServiceAccount can read the trust configuration it
holds and mint what that configuration allows: a token for the named service
account, in the named namespace, in each registered cell. That is the hub's job,
so it cannot be designed away.

What it does **not** grant: any credential already issued (none are stored), any
access beyond what each cell's service account has, or anything in a cell whose
`TrustConfig` the hub does not hold. Scoping those service accounts tightly is
the mitigation, and it is why the minted identity is a `serviceAccountName` and
a `namespace` rather than a cluster-admin kubeconfig.

The full analysis is in the [threat model](threat-model.md).

# Placement policy

A `PlacementPolicy` answers two questions in order: **which callers may reach
which cells**, and **how the survivors are ranked**. The order is the security
control.

## Deny by default

A caller matching no policy is refused with `NoPolicy`. There is no "any cell"
fallback, and no flag turns one on. Everything else about the object follows
from that.

## The shape

```yaml
apiVersion: cellcast.io/v1alpha1
kind: PlacementPolicy
metadata:
  name: app-prod
  namespace: cellcast-system
spec:
  subjects:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:acme/checkout:ref:refs/heads/main
      claims:
        repository: acme/checkout
  permittedCells:
    matchLabels:
      env: prod
    matchExpressions:
      - key: region
        operator: In
        values: [euw1, euw2]
  strategy: LeastLoaded
  tokenTTL:
    default: 15m
    max: 30m
  allowDarkTargeting: false
```

### `subjects`

Who the policy is about, matched against the caller's verified token claims.
Every field present must match; a `SubjectSelector` is an AND.

`issuer` is required and must also be on the hub's own allowlist. Naming an
issuer here does not add it: the hub verifies signatures only against issuers it
was started with, so a policy naming an unconfigured issuer matches nothing.

`subject` and `claims` are exact-match. There is no pattern matching. A glob in
a policy reads as narrower than it is, and the failure stays silent until
somebody notices an unexpected branch deploying to production.

!!! warning "An issuer-only selector is broader than it looks"

    Against a Kubernetes cluster issuer, an issuer-only selector permits every
    workload in that cluster, because `sub` is the only claim distinguishing
    them. Against a CI issuer it permits every repository on the platform.
    Name a `subject`, or a `claims` entry, for anything but a development
    policy.

### `permittedCells`

A standard label selector over registered `Cluster` objects, evaluated at
decision time. Labelling a new cell `env: prod` adds it to every policy that
selects on that label, without editing any of them. That is the intended
behaviour and the thing to be careful about.

An empty selector matches every registered cell. Valid, occasionally useful in
development, and never correct in production.

### `strategy`

How the permitted, eligible survivors are ranked:

| Strategy | Ranking |
|---|---|
| `LeastLoaded` (default) | Lowest committed capacity, where committed is the larger of CPU and memory pressure |
| `RoundRobin` | Even distribution, ignoring load |

Utilisation is the **maximum** of CPU and memory pressure, not the mean. A cell
at 95% memory and 10% CPU is nearly full rather than half loaded, and averaging
is how a scheduler keeps sending work to a cell one pod away from evicting
things.

Scoring only ever runs on cells that already passed the filter. The least-loaded
cell in the estate is never returned to a caller not permitted to reach it.

### `tokenTTL`

`default` is granted when the caller asks for nothing. `max` is the ceiling; a
longer request is clamped, and the response says so. The hub's own
`--token-max-ttl` bounds every policy, so a policy cannot raise it.

The Kubernetes `TokenRequest` API enforces a floor of ten minutes and applies no
maximum of its own unless the cluster operator configured one. A `max` below ten
minutes therefore produces a policy that admits placements and can never mint
for them.

### `allowDarkTargeting`

Whether a caller under this policy may ask for a `DARK` cell with `--dark`. Off
by default. A dark cell is reachable **only** through a policy that permits it,
which is what makes a QA smoke test against one safe to run.

## Cell state, which is not policy

A cell's `spec.state` is set by an operator and applies to every policy:

| State | Receives new placements | Serves production traffic |
|---|---|---|
| `LIVE` | yes | yes |
| `DARK` | only when explicitly requested | no |
| `DRAINING` | no | yes |

`DRAINING` is the upgrade-window state: existing workloads keep serving and new
deploys route elsewhere, without anyone editing a pipeline. That case is why
there are three states rather than two.

## Seeing what a policy does

Before relying on a policy, ask what it does:

```console
$ cellcast place --workload checkout-api --dry-run --explain
would place checkout-api on prod-euw1 (policy app-prod, LeastLoaded, confidence high)

CELL        ADMITTED  STAGE       REASON                                        UTILISATION
prod-euw1   yes                                                                 0.42
prod-euw2   yes                                                                 0.77
dev-euw1    no        permission  cell is not permitted by this caller's policy  0.01
```

The `STAGE` column says where a cell dropped out:

| Stage | Meaning |
|---|---|
| `permission` | The policy's `permittedCells` does not select it |
| `state` | It is `DRAINING`, or `DARK` without a dark-targeting request |
| `capacity` | Its capacity is stale or missing, so it cannot be ranked |

A cell excluded at `capacity` is a monitoring problem, not a policy problem.

That table only exists once a policy has matched. When none does, the refusal is
`NoPolicy` and carries nothing else, because naming which selector missed would
tell a caller how the authorization rules are shaped. What the hub will report
is the caller's own token, which is the half a policy author cannot see:

```console
$ cellcast policy test --workload checkout-api
refused: NoPolicy
  no placement policy permits this caller

the hub read this token as:
  issuer   https://token.actions.githubusercontent.com
  subject  repo:acme@56138094/app@1332281435:pull_request
  claims   ref=refs/heads/main
           repository=acme/app

no policy names that caller. Compare the values above against spec.subjects:
  kubectl -n cellcast-system get placementpolicy -o yaml
```

Two strings side by side is usually the whole diagnosis. The subject above is
the immutable format, which every repository created after 15 July 2026 gets; a
policy written as `repo:acme/app:pull_request` authenticates that caller
perfectly and then refuses it.

It mints nothing and can be run as often as a policy is edited. It exits
non-zero on a refusal, so it works as a pipeline check.

## Policy readiness

The hub reports whether a policy currently selects anything:

```console
$ kubectl -n cellcast-system get placementpolicy
NAME       STRATEGY      READY   CELLS
app-prod   LeastLoaded   True    selector "env=prod" matches 2 registered cell(s)
qa-dark    LeastLoaded   False   selector "env=qa" matches no registered cell
```

`READY: False` means a policy that will refuse every caller it matches. Usually
a label typo, and much cheaper to see here than in a build log.

The `ISSUERS` column answers the other half, which is whether the hub can verify
anybody this policy names:

```console
$ kubectl -n cellcast-system get placementpolicy
NAME       STRATEGY      READY   ISSUERS   CELLS
app-prod   LeastLoaded   True    True      selector "env=prod" matches 2 registered cell(s)
gitlab     LeastLoaded   True    False     selector "env=prod" matches 2 registered cell(s)
```

The second one selects cells perfectly well and can still never match a caller.
Naming an issuer in a policy does not add it: the hub verifies signatures only
against the issuers it was started with, so a policy pointing anywhere else is
valid YAML that matches nobody. The condition says which issuer, and which ones
the hub actually holds:

```console
$ kubectl -n cellcast-system get placementpolicy gitlab \
    -o jsonpath='{.status.conditions[?(@.type=="IssuerTrusted")].message}'
this hub does not verify "https://gitlab.example.com"; it was started with
"https://token.actions.githubusercontent.com", and a policy does not add an issuer
```

The fix is a `--oidc-issuer` flag on the hub, not an edit to the policy. See
[Extending](extending.md) for which provider name to give it.

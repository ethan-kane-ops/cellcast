# Troubleshooting

Almost every cellcast problem shows up as a refusal, and the refusal reason says
which one.

## Read the reason, not the message

Every refusal carries a machine-readable `reason`. The prose beside it is for a
human reading a build log and may change. The reason will not.

```console
$ cellcast place --workload checkout-api --json | jq -r '.reason'
NoPolicy
```

| Reason | What it means | Where to look |
|---|---|---|
| `NoPolicy` | No `PlacementPolicy` matches this caller | The token's claims against `spec.subjects` |
| `NoPermittedCells` | A policy matched, but its selector permits no registered cell | `permittedCells` against the cells' labels |
| `NoEligibleCells` | Every permitted cell is `DRAINING`, or `DARK` without `--dark` | The cells' `spec.state` |
| `CapacityUnknown` | Every permitted, eligible cell has stale or missing capacity | The agents |
| `DarkNotPermitted` | `--dark` under a policy with `allowDarkTargeting: false` | The policy |
| `PlacementUnavailable` | The hub is running but cannot decide yet | Usually a replica still warming up. Retry |
| `MintUnavailable` / `MintFailed` | The decision was made and the credential was not | The cell's `TrustConfig` |
| `InvalidRequest` | The request itself was malformed | The client's arguments |

Two of them look alike and are not. `PlacementUnavailable` may be answered by an
`--on-unavailable` stance, because the caller was already found to be permitted.
`NoPolicy` may never be: a fallback that got past an authorization refusal would
make the flag a route around the policy engine.

## The hub refuses everything with 401

Check whether an issuer is configured at all:

```console
$ kubectl -n cellcast-system logs deploy/cellcast | grep -i oidc
{"level":"WARN","msg":"no --oidc-issuer configured; every API request will be rejected as unauthenticated"}
```

A hub with no trusted issuer authenticates nobody. It is healthy, it is ready,
and it authorizes nothing. Nothing else about the deployment looks wrong, which
is why `CellcastNoAuthenticator` is one of the shipped alerts.

If issuers **are** configured, the rejection reason says which check failed:

```promql
sum by (reason) (rate(cellcast_auth_rejections_total[5m]))
```

| Reason | Cause |
|---|---|
| `IssuerNotAllowed` | The token's `iss` is not on the hub's allowlist |
| `Audience` | The caller did not request the hub's `--oidc-audience` |
| `Expired` | A clock problem, or a token minted much earlier in the pipeline |
| `Signature` | The key is not in the issuer's JWKS |

A run of `Expired` is a clock problem or a retry loop. A run of
`IssuerNotAllowed` is somebody presenting tokens this hub was not configured to
accept.

`Signature` covers the whole verification step, including fetching the issuer's
keys, so it is also what a hub reports when it cannot reach the issuer at all.
On a self-hosted issuer that is usually a certificate the hub does not trust.
The hub logs the underlying error; look for a discovery failure naming the
issuer:

```bash
kubectl -n cellcast-system logs deploy/cellcast | grep -i 'certificate\|discovery'
```

Trust the issuer's CA with `hub.oidc.caConfigMap`, which the chart turns into
`--oidc-ca-file`. A hub run outside the chart takes that flag directly, pointing
at a PEM bundle. Either way the bundle is added to the system roots rather than
replacing them, so a public issuer configured alongside a private one keeps
working. See [getting started](getting-started.md).

## A cell never gets placements

Ask what the hub thinks of it:

```console
$ kubectl -n cellcast-system get cluster prod-euw2
NAME        PROVIDER   STATE   ACCEPTING   ENDPOINT
prod-euw2   eks        LIVE    False       https://prod-euw2.example.com
```

`ACCEPTING: False` on a `LIVE` cell means the hub has no usable capacity for it.
Then check the cell has actually reported:

```promql
cellcast_capacity_staleness_seconds{cell="prod-euw2"}
```

No series at all means the hub has **never** accepted a report for it. In order
of likelihood:

1. **`spec.reporter` is unset.** No capacity is accepted for the cell at all.
   Unset means nobody may report, not anybody may.
2. **`spec.reporter.issuer` does not match.** Read the cell's real issuer:
   ```bash
   kubectl get --raw /.well-known/openid-configuration | grep issuer
   ```
3. **The agent cannot reach the hub.** Its log says so, with backoff.
4. **The agent's token has the wrong audience.** It must match the hub's
   `--oidc-audience`, and it must not be the API server's audience.

A series that exists and is climbing means the agent reported once and stopped.
Look at the agent, not the hub.

## Every placement is refused with `PlacementUnavailable`

A replica that has not heard from the fleet refuses rather than scoring an empty
index. Check whether that is all of them:

```promql
min(cellcast_hub_warm)   # 0 means at least one replica cannot place
sum(cellcast_hub_warm)   # how many can
```

The hub says what it is waiting for:

```console
$ kubectl -n cellcast-system logs deploy/cellcast | grep warming
{"level":"WARN","msg":"becoming ready before capacity covers the fleet",
 "waited":"1m30s","cells_not_reporting":["prod-euw2"]}
```

If it names a cell, that cell's agent is the problem and the section above
applies. If it persists past a heartbeat interval on a fleet that is otherwise
reporting, the replica cannot reach the API server to list the registry.

## A deploy landed on the wrong cell

Ask the hub why, without deploying anything:

```console
$ cellcast place --workload checkout-api --dry-run --explain
```

The candidate table shows every cell and the stage at which it dropped out. A
cell showing `permission` is one the policy does not permit. A cell showing
`capacity` has an agent that is not reporting, so it was excluded rather than
ranked badly.

For a deploy that already happened, the audit record has the same table, keyed
by `request_id`. See the [audit trail reference](audit.md).

## A credential expires mid-deploy

Read the line the client printed:

```console
credential valid for 30m (requested 2h, capped by policy at 30m)
```

The cap is the policy's `tokenTTL.max`, bounded in turn by the hub's
`--token-max-ttl`. Raise the policy, or make the deploy step shorter. A
credential is never renewed in place: cellcast issues one per placement.

## `helm install` fails on the grace period

```text
Error: terminationGracePeriodSeconds (20) must exceed hub.drainDelay (5s) plus
hub.shutdownTimeout (20s); the kubelet would kill the pod mid-drain
```

Those two run in sequence and the kubelet has to accommodate their sum. Raise
`terminationGracePeriodSeconds`, or lower one of the other two.

## The chart installs and the pods crash-loop

Check the args the hub was given against the ones its version accepts:

```bash
kubectl -n cellcast-system logs deploy/cellcast --previous
```

A chart newer than the image can pass a flag the binary does not know. Keep the
chart's `appVersion` and the image tag in step.

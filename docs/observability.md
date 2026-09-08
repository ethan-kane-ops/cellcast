# Observability

Three views of the same events, with one instrumentation point behind them.

| View | What it is for | Where |
|---|---|---|
| **Audit trail** | What happened, per request, forensically | stdout, JSON |
| **Metrics** | Rates and trends, alerting | Prometheus |
| **Events** | "who has been deploying here", per cell | `kubectl describe cluster` |

## One record, three views

Every placement and every mint produces one `audit.Record`. The JSON trail, the
Kubernetes Events and the Prometheus counters are all views over it.

That is deliberate. A metric incremented at its own call site drifts from the
log line next to it the first time somebody adds an early return, and the drift
is invisible: both look fine, they just describe different things. Deriving all
three from one record means a refusal that is audited is counted by
construction.

The practical consequence for contributors: **new hub instrumentation goes on
the audit record**, not on a new call site.

## The audit trail

One JSON line per placement and per mint, tied together by `request_id`:

```json
{"time":"2026-09-08T12:31:50Z","level":"INFO","msg":"audit",
 "event":"placement","outcome":"granted","request_id":"03c2fb846fa6f746",
 "issuer":"https://token.actions.githubusercontent.com",
 "subject":"repo:acme/checkout:ref:refs/heads/main",
 "workload":"checkout-api","cell":"prod-euw1","policy":"app-prod",
 "strategy":"LeastLoaded","confidence":"high",
 "candidates":[{"cell":"dev-euw1","admitted":false,"stage":"permission",
                "reason":"cell is not permitted by this caller's policy"}]}
```

Three properties worth knowing:

- **It is written through its own handler at a fixed level.** `--log-level=error`
  suppresses the hub's chatter and never the trail. Sharing one logger would
  make the audit record a debugging convenience rather than a record.
- **It cannot carry token material.** A reflection test enumerates every field
  on the record and fails on any new one that has not been classified. Mints
  record `token_sha256`, a digest for correlation.
- **A refusal records what was considered.** The candidate table travels on the
  refusal, so "why was my deploy refused" is answerable from the log rather than
  only from a client that happened to pass `--explain`.

Worked queries are in the [audit trail reference](audit.md).

## Metrics

The full surface, the cardinality argument, and the three metrics that are not
what you would guess are in the [metrics reference](metrics.md). The short
version:

```promql
# Is cellcast adding time to every deploy?
histogram_quantile(0.99, sum by (le) (rate(cellcast_placement_duration_seconds_bucket[5m])))

# What share of placements is being refused, and why?
sum by (reason) (rate(cellcast_placement_requests_total{outcome="refused"}[5m]))

# Can every replica actually place?
min(cellcast_hub_warm)

# Which cells has the hub stopped hearing from?
cellcast_capacity_staleness_seconds > ignoring(cell) group_left() (2 * cellcast_capacity_staleness_window_seconds)
```

A dashboard ships in `dashboards/cellcast.json` and five alerting rules in
`config/prometheus/prometheusrule.yaml`. Both are templated by the hub chart:

```bash
helm upgrade cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast --reuse-values \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.prometheusRule.enabled=true
```

A contract test asserts the dashboard, the alerts and the documentation agree
with what the registry actually emits, **in both directions**: a metric nobody
plots fails the build, and so does a dashboard panel querying a metric that no
longer exists.

!!! warning "The metrics endpoint is unauthenticated"

    It discloses cell names, policy names and refusal counts, which an
    authenticated caller could already list through the API. It carries no
    caller identity, no workload name and no credential. Reaching it should
    still be a NetworkPolicy decision: `networkPolicy.enabled` in the chart.

## Kubernetes Events

The audit trail's second view, hung on the `Cluster` each record concerns, so
the question "who has been deploying here" is answerable with `kubectl`:

```console
$ kubectl -n cellcast-system describe cluster prod-euw1
...
Events:
  Type    Reason             Age    From           Message
  ----    ------             ----   ----           -------
  Normal  Placed             2m     cellcast-hub   placed checkout-api for repo:acme/checkout:ref:refs/heads/main under policy app-prod
  Normal  CredentialIssued   2m     cellcast-hub   issued a 15m0s credential to repo:acme/checkout:ref:refs/heads/main, scoped to apps/deployer
```

Events are lossy by design: they are rate-limited and expire. They are a
convenience on top of the trail, never a substitute for it. Dry runs produce no
event, because a dry run is not a deploy.

Only the leader emits them, which is why they appear once rather than once per
replica.

## What to alert on

The five shipped rules, and why each one is not merely a dashboard panel:

| Alert | Catches |
|---|---|
| `CellcastCapacityStale` | A cell whose agent stopped reporting, so deploys silently concentrate elsewhere |
| `CellcastCellNeverReported` | A registered `LIVE` cell that has never reported, so it can never be placed on |
| `CellcastReplicaCannotPlace` | A replica in the Service that still has no capacity for the fleet |
| `CellcastAuthRejectionRatioHigh` | A broken pipeline, or somebody probing |
| `CellcastNoAuthenticator` | A hub refusing everything because no OIDC issuer is configured, while passing its own health checks |

The last one is the misconfiguration that stops every deploy in the estate while
looking entirely healthy from the outside.

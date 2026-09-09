# Metrics

cellcast sits in the deploy critical path for a whole estate. What it costs every deploy, and how an
operator would know it broke.

## The endpoint

`cellcast-hub` serves Prometheus metrics on `:8082/metrics` by default. `--metrics-addr` moves it;
`--metrics-addr=0` turns it off.

It is a separate listener from the API (`:8080`) and from the probes (`:8081`), and it serves
controller-runtime's own collectors alongside cellcast's, because both register with the same
process-wide registry. There is no fourth port and no second registry.

**The endpoint is unauthenticated.** What it discloses is cell names, policy names, per-cell
utilisation and refusal counts. None of that is credential material and none of it is anything an
authenticated caller cannot already read through `GET /api/v1/clusters`, but reaching the port is still a
decision to make: bind it to the pod network only and let a NetworkPolicy say who may scrape it. The
hub chart ships that policy along with the `ServiceMonitor`. Recorded as an accepted risk in [the
threat model](threat-model.md).

## What is exported

Counters and histograms are driven by the [audit trail](audit.md) rather than by a second pass over
the placement handler. Every fact they need is already on an audit record, and one instrumentation
point cannot disagree with itself.

| Metric | Type | Labels | What it answers |
| --- | --- | --- | --- |
| `cellcast_placement_requests_total` | counter | `outcome`, `reason`, `policy`, `cell` | How many deploys were placed, refused, or only asked about, and why |
| `cellcast_placement_duration_seconds` | histogram | `outcome` | What cellcast costs every deploy |
| `cellcast_tokens_minted_total` | counter | `provider`, `env` | How many credentials were issued, and against what |
| `cellcast_mint_failures_total` | counter | `provider`, `reason` | Placement chose a cell and the hub could not reach it |
| `cellcast_token_ttl_seconds` | histogram | | Whether a policy is quietly granting longer credentials than it used to |
| `cellcast_auth_rejections_total` | counter | `reason` | A broken pipeline, or somebody probing |
| `cellcast_cluster_capacity_ratio` | gauge | `cell` | How full each cell is |
| `cellcast_capacity_staleness_seconds` | gauge | `cell` | How long since each cell was last heard from |
| `cellcast_capacity_staleness_window_seconds` | gauge | | The window this hub considers usable |
| `cellcast_cluster_state` | gauge | `cell`, `state` | Which cells are LIVE, DARK or DRAINING |
| `cellcast_hub_warm` | gauge | | Whether this replica has enough capacity to place at all |

`outcome` is `granted`, `refused` or `dry-run`. `reason` on a placement is the refusal taxonomy the
caller received, so an operator and a pipeline are reading the same word for the same event.

### Four definitions that are not obvious

**`cellcast_cluster_capacity_ratio` disappears when a cell goes stale.** It is not held at its last
value and it is not zeroed. A stale ratio is a number a dashboard reads as current and the placement
engine does not, and the two disagreeing is worse than a gap in the graph. Staleness keeps being
reported for the same cell, which is where the alert comes from.

**`cellcast_cluster_state` emits every state, not just the current one.** The current state is `1`
and the others are `0`, so `cellcast_cluster_state == 1` selects it and a transition moves the `1`
rather than leaving an old series to be read as still true.

**`cellcast_capacity_staleness_window_seconds` exists so an alert never hardcodes a threshold.** The
window is `--capacity-staleness`, which an operator can change. A rule written against this metric
retunes itself; one written against `180` has to be found and edited.

**`cellcast_hub_warm` only ever goes up.** It flips to `1` the first time a replica's capacity index
covers the fleet and stays there. Capacity going stale afterwards is the placement engine's problem
and it already has an answer: the affected cells become Unknown and are excluded. If warmth fell
back to `0`, readiness would follow it, and one bad minute across the fleet would pull every replica
out of the Service at once. Aggregate it with `min`, never `avg`: one cold replica in three refuses
a third of the estate's deploys.

Fleet gauges are all collected at scrape time from the live capacity index and the informer cache
rather than written when something changes. A gauge set on write keeps reporting a decommissioned
cell forever, because nothing deletes the series.

## Cardinality

`cell` and `policy` are bounded by the size of the fleet and the number of policies, both of which
are operator-authored and small. `reason` is a closed set. Nothing is labelled by caller subject,
workload, or request id: those are unbounded, and they are on the audit record, which is where an
unbounded field belongs.

`cellcast_auth_rejections_total` is labelled by a classification, never by the error message. An
error string is partly written by whatever the issuer returned, so labelling by it would make the
cardinality of this metric a function of somebody else's error messages.

## Dashboard

`dashboards/cellcast.json` imports into Grafana as-is. It picks a Prometheus datasource and a
namespace as variables and needs no editing.

Four rows: placement rate and latency, refusal and authentication reasons, per-cell utilisation,
staleness and state, then credentials issued and their lifetimes.

## Alerts

`config/prometheus/prometheusrule.yaml` is a `PrometheusRule` with five alerts. It needs the
Prometheus Operator CRDs, and the hub chart templates it in.

| Alert | Severity | Fires when |
| --- | --- | --- |
| `CellcastCapacityStale` | warning | A cell has been quiet for more than twice this hub's staleness window |
| `CellcastCellNeverReported` | warning | A registered LIVE cell has never reported capacity at all |
| `CellcastAuthRejectionRatioHigh` | warning | More than half of all attempts are failing authentication |
| `CellcastReplicaCannotPlace` | warning | A replica is in the Service and still has no capacity for the fleet |
| `CellcastNoAuthenticator` | critical | The hub is refusing everything because no OIDC issuer is configured |

The first two are the same failure at different stages, and they need separate rules because a cell
that has never reported has no staleness series for a threshold to exceed.

`CellcastNoAuthenticator` is the one unambiguous alert in the set. A hub started with no
`--oidc-issuer` refuses every caller, passes its own health checks, and stops every deploy in the
estate. Every other rule here describes a condition that may be benign.

## Scraping it by hand

```bash
kubectl -n cellcast-system port-forward deploy/cellcast-hub 8082:8082
curl -s localhost:8082/metrics | grep '^cellcast_'
```

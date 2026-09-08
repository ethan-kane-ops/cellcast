# High availability

cellcast sits in the deploy critical path, so a single replica is a single point
of failure for every deploy in the estate. The hub runs N replicas by default.

## Every replica answers, only the controllers elect

The hub is two programs in one process. The API answers placements and mints;
the controllers write `Cluster` status and emit Events.

Running N of the first is the point. Running N of the second means three
replicas fighting over the same status subresource and emitting every event
three times, so a lease covers the controllers alone.

This works because nothing in the decision path writes. Placement reads the
informer cache, which every replica keeps synced whether or not it holds the
lease, and ranks the survivors on a capacity index that is per-replica and
rebuilt from heartbeats. A follower answers a placement exactly as well as the
leader does.

## Replicas disagree about capacity, and that is fine

Agents heartbeat through the Service, so each report lands on whichever replica
the load balancer chose. Two replicas asked the same question in the same second
can pick different cells.

Placement is advisory, so a marginally worse cell is a worse cell rather than a
wrong one. Sharing the index between replicas would buy agreement on a number
that is stale by construction, in exchange for a distributed system to be wrong
about.

What is **not** acceptable is a replica with no capacity at all, which is the
next section.

## Readiness means the replica can actually answer

A newly started replica has an empty capacity index. Every cell is `Unknown`, so
it would exclude the entire fleet and refuse every placement handed to it.

Readiness therefore waits until the index holds a fresh report for every cell
expected to report: every registered `Cluster` that names a `reporter` and is not
`DRAINING`. Coverage rather than "any capacity at all", because a replica
holding one cell out of six is not warm; it is a replica that will send every
deploy to that one cell.

### Why the wait is bounded

Agents heartbeat through the Service. A Service routes only to ready pods. So a
fleet whose hub replicas all restarted at once is waiting for reports that
nothing can deliver.

Past `--warmup-timeout` (default 90s, one staleness window) a replica goes ready
anyway, logs that it did, and names the cells it never heard from:

```json
{"level":"WARN","msg":"becoming ready before capacity covers the fleet",
 "waited":"1m30s","cells_not_reporting":["prod-euw2"]}
```

It keeps refusing placements until it is warm, with `PlacementUnavailable`
rather than `CapacityUnknown`. The two resolve differently: the first is a
property of the replica the caller reached and clears itself, so a retry is
worth making.

Warmth latches. Capacity going stale later is the placement engine's problem and
it already has an answer. If warmth could fall back, readiness would follow it,
and one bad minute across the fleet would pull every replica out of the Service
at once.

Watch it per replica, and aggregate with `min`, never `avg`:

```promql
min(cellcast_hub_warm)
```

One cold replica in three refuses a third of the estate's deploys while the
average still looks healthy.

## Shutdown waits before it closes

Kubernetes removes a pod from Service endpoints asynchronously, and it sends
`SIGTERM` at the same moment it starts. A process that closes its listener on
the signal refuses everything routed to it while that removal propagates, which
is a deploy failing because cellcast was being upgraded.

So the hub:

1. Reports itself unready, which is what makes the next kubelet probe fail.
2. Keeps serving for `--drain-delay` (default 5s) while the removal propagates.
3. Drains in-flight requests within `--shutdown-timeout` (default 20s).

!!! warning "The grace period has to fit both"

    Those two run in sequence, so `terminationGracePeriodSeconds` must exceed
    their sum or the kubelet's `SIGKILL` lands mid-drain. The chart refuses to
    render if it does not, naming all three numbers.

The rollout uses `maxUnavailable: 0`, so a new replica is ready, which means
warm, before an old one goes away.

## What the chart gives you

| Value | Default | What it protects |
|---|---|---|
| `replicaCount` | `3` | The API path |
| `podDisruptionBudget.maxUnavailable` | `1` | A node drain evicting every replica |
| `topologySpreadConstraints` | one per node, `ScheduleAnyway` | Every replica landing on one node |
| `updateStrategy.rollingUpdate.maxUnavailable` | `0` | A rollout with fewer replicas than it started with |
| `hub.leaderElection.enabled` | `true` | Three replicas writing the same status |

`ScheduleAnyway` rather than `DoNotSchedule` so a single-node development
cluster still runs. Change it to `DoNotSchedule` where the estate can satisfy it.

## Failure behaviour

| Failure | What happens |
|---|---|
| One replica dies | The others serve. Endpoints removes it on the next probe |
| A replica is still warming up | Ready, so agents can reach it, but placements are refused with `PlacementUnavailable` until it is warm |
| A replica is being rolled | Reports unready, keeps serving for the drain delay, then drains |
| The leader loses its lease | That replica exits and restarts. The others keep serving placements from cache |
| The API server is unreachable | Only the leader exits. Followers stay in their acquisition loop and keep serving. The restarted replica cannot sync, so it stays unready and out of the Service until the partition clears |
| Every replica is down | The client applies its declared `--on-unavailable` stance, and the default is to fail |

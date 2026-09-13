# High availability

cellcast sits in the deploy critical path, so a single replica is a single point
of failure for every deploy in the estate. The hub runs N replicas by default.

## Every replica answers, only the controllers elect

The hub is two programs in one process. The API answers placements and mints;
the controllers write `Cluster` status and emit Events.

Running N of the first is the reason for the exercise. Running N of the second
means three replicas fighting over the same status subresource and emitting
every event three times, so a lease covers the controllers alone.

This works because nothing in the decision path writes. Placement reads the
informer cache, which every replica keeps synced whether or not it holds the
lease, and ranks the survivors on a capacity index that is per-replica and
rebuilt from heartbeats. A follower answers a placement exactly as well as the
leader does.

The one write a placement causes comes after the decision, once a credential
has been minted: the cell the workload landed in, so the next placement can
keep it there. Any replica makes it, it survives the rollout of the replica that
made it, and a write that fails costs only that workload's next placement its
memory ([stickiness](placement-policy.md#stickiness)).

## Every replica hears every cell

An agent keeps one connection to the hub open, and a Service balances
connections rather than requests, so every report from a cell reaches the same
replica. That replica relays each report it accepts to the others, which it
finds through a headless Service the chart creates (`cellcast-peers` for a
release named `cellcast`).

The relay carries the agent's own token. Each replica authenticates it and
checks the cell's `spec.reporter` itself, exactly as for the agent, so no
replica takes another's word for a cell's capacity. Each keeps its own index,
built from reports it verified.

Replicas can still disagree while a relay is in flight, or after one is lost,
and two replicas asked the same question in that moment can pick different
cells. That is acceptable. Placement is advisory, so a marginally worse cell is
a worse cell rather than a wrong one, and the next heartbeat repairs a lost
relay well inside the staleness window.

A replica that relays are not reaching is a different matter: it places only on
the cells whose agents connected to it. `CellcastCapacityStale` fires for that
replica alone, and the replicas relaying to it log it as failing.

A replica with **no** capacity at all is the next section.

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

A replica that starts while others are serving does not meet this. The peers
Service publishes replicas that are not ready yet, so the others relay to it
before it is ready, and it warms within a heartbeat. The deadline is for a fleet
whose replicas all restarted together, where nothing is serving to relay from.

Past `--warmup-timeout` (default 90s, one staleness window) a replica goes ready
anyway, logs that it did, and names the cells it never heard from:

```json
{"level":"WARN","msg":"becoming ready before capacity covers the fleet",
 "waited":"1m30s","cells_not_reporting":["prod-euw2"]}
```

It keeps refusing placements until it is warm, with `PlacementUnavailable`
rather than `CapacityUnknown`. The two resolve differently: the first is a
property of the replica the caller reached and clears on its own, so a retry
may succeed.

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

## What the chart sets

| Value | Default | What it protects |
|---|---|---|
| `replicaCount` | `3` | The API path |
| `podDisruptionBudget.maxUnavailable` | `1` | A node drain evicting every replica |
| `topologySpreadConstraints` | one per node, `ScheduleAnyway` | Every replica landing on one node |
| `updateStrategy.rollingUpdate.maxUnavailable` | `0` | A rollout with fewer replicas than it started with |
| `hub.leaderElection.enabled` | `true` | Three replicas writing the same status |

`ScheduleAnyway` rather than `DoNotSchedule` so a single-node development
cluster still runs. Set it to `DoNotSchedule` on an estate that can satisfy it.

The chart also creates the peers Service and passes it to `--peers`. There is no
value to turn that off: a single replica finds only itself there and relays to
nobody. With `networkPolicy.enabled`, the chart's policy admits the hub's pods
to each other on the API port. A NetworkPolicy written for the estate instead
has to do the same, or each replica hears only its own agents.

## Failure behaviour

| Failure | What happens |
|---|---|
| One replica dies | The others serve. Endpoints removes it on the next probe |
| A replica is still warming up | Ready, so agents can reach it, but placements are refused with `PlacementUnavailable` until it is warm |
| A replica is being rolled | Reports unready, keeps serving for the drain delay, then drains |
| The leader loses its lease | That replica exits and restarts. The others keep serving placements from cache |
| Relays to one replica fail | That replica excludes the cells whose agents are connected elsewhere once their capacity goes stale. `CellcastCapacityStale` fires for it alone, and the replicas relaying to it log it once |
| The API server is unreachable | Only the leader exits. Followers stay in their acquisition loop and keep serving. The restarted replica cannot sync, so it stays unready and out of the Service until the partition clears |
| Every replica is down | The client applies its declared `--on-unavailable` stance, and the default is to fail |

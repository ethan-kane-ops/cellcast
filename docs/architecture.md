# Architecture

cellcast answers two questions for a deploy pipeline that already exists:

1. **Where does this workload go?**
2. **What credential lets me deploy it there, right now, for as short a time as possible?**

It does not deploy anything. It does not propagate manifests, own a GitOps loop, or sit in the
data path of a running application. Every existing tool in this space (Open Cluster Management,
Karmada, Rancher Fleet) answers question 1 only once its control plane and propagation model have
been adopted. cellcast is a queryable oracle attached to a pipeline that already exists.

## Non-goals

Stated up front because the security argument depends on them:

- cellcast **never stores a credential**. Not encrypted, not in a vault, not in etcd.
- cellcast **never proxies traffic** to a managed cluster.
- cellcast **never retains a token** after returning it.
- cellcast **never propagates or applies** a manifest.

It holds trust configuration and mints on demand. If the hub process is compromised, the attacker
gains the ability to mint within the operator's policy ceiling. They do not gain a vault to drain.
That distinction is the security argument, and [the threat model](threat-model.md) states its limits
rather than asserting it holds.

## System shape

Three binaries, deployed in two places.

```
  CI / CD pipeline                       hub cluster
  (GitHub Actions,                 +---------------------------+
   Buildkite, ArgoCD)              |                           |
        |                          |   cellcast-hub            |
        |  1. OIDC workload        |   +-------------------+   |
        |     identity JWT         |   | HTTP API          |   |
        |  ----------------------> |   |  authn (OIDC)     |   |
        |                          |   |  policy filter    |   |
        |   cellcast (client CLI)  |   |  capacity score   |   |
        |                          |   |  token mint       |   |
        |  <---------------------- |   +-------------------+   |
        |  4. cell + short-lived   |   +-------------------+   |
        |     scoped token         |   | controllers       |   |
        |                          |   |  Cluster          |   |
        |                          |   |  PlacementPolicy  |   |
        |                          |   +-------------------+   |
        |                          |            |              |
        |                          |         etcd (CRDs)       |
        |                          +---------------------------+
        |                                  ^            ^
        |                       3. heartbeat|            |
        |  5. kubectl apply                 |            |
        |     using only that token         |            |
        v                                   |            |
  +------------------+  +------------------+|  +------------------+
  | spoke cell 01    |  | spoke cell 02    ||  | spoke cell 03    |
  | cellcast-agent --+--+ cellcast-agent --++--+ cellcast-agent   |
  +------------------+  +------------------+   +------------------+
```

| Binary | Runs in | Job |
| --- | --- | --- |
| `cellcast-hub` | the hub cluster | HTTP API, controllers, the only component that can mint |
| `cellcast-agent` | every registered spoke | reports capacity to the hub on a heartbeat, under its own projected ServiceAccount token |
| `cellcast` | the pipeline runner or an operator's laptop | client CLI |

## The placement path

One request, in order. Each step can reject, and the order is load-bearing.

1. **Authenticate.** The caller presents the workload identity JWT its CI platform already issues.
   The hub validates it against the issuer's JWKS. There is no shared secret anywhere in this
   chain.
2. **Filter by permission.** The authenticated claims resolve to a `PlacementPolicy`, which
   expresses the permitted cells as a label selector over the `Cluster` registry. No matching
   policy means rejection, never a fallback to "any cell".
3. **Filter by eligibility.** From the permitted set, drop cells that are `DRAINING`, cells whose
   capacity is `Unknown`, and cells that are `DARK` unless the caller explicitly asked for dark.
4. **Score.** Of the survivors, pick one: least-loaded by default, round-robin as an alternative,
   chosen by policy rather than by the caller. Ties break deterministically.
5. **Mint.** Request a short-lived credential scoped to the chosen cell and the workload's
   namespace boundary, with a TTL resolved from policy. Return it. Forget it.

Steps 2 and 3 are the filter phase; step 4 is the optimisation phase. Running them in the other
order is the bug that routes a dev pipeline into the least-loaded production cluster in another
region.

`--explain` renders steps 2 through 4 as a table so a surprising placement is legible without a
debug build. `--dry-run` runs 1 through 4 and stops before 5.

A refusal carries a machine-readable `reason` alongside its message, and the status code separates
refusals a caller must not retry from ones that may clear on their own. `NoPolicy` and
`DarkNotPermitted` are 403 and never become a yes. `NoPermittedCells` is 409, because retrying cannot
fix a selector that matches nothing. `NoEligibleCells` and `CapacityUnknown` are 503, because a
draining cell comes back and an agent starts reporting again. The client's fallback stance is built
on that split, so a client never has to match on prose to find it.

## Decision records

| # | Decision | Choice | Rejected |
| --- | --- | --- | --- |
| [ADR-001](#adr-001-the-registry-is-a-crd-not-a-database) | Registry storage | `Cluster` CRD in the hub cluster | Postgres, DynamoDB, Redis |
| [ADR-002](#adr-002-capacity-is-in-memory-with-a-staleness-guard) | Capacity storage | In-memory, TTL, rebuilt from heartbeats | Any durable store |
| [ADR-003](#adr-003-callers-authenticate-by-oidc-workload-identity-federation) | Caller identity | OIDC federation against the CI issuer's JWKS | Static bearer tokens, mTLS |
| [ADR-004](#adr-004-downstream-credentials-are-minted-never-stored) | Downstream credential | Kubernetes `TokenRequest` in v0.1, AWS STS in v0.2 | Stored kubeconfigs |
| [ADR-005](#adr-005-placement-is-filter-then-score-and-policy-is-a-crd) | Placement shape | Filter by policy, then score; policy is a CRD | Scoring alone, policy in a config file |
| [ADR-006](#adr-006-cellcast-is-advisory-not-authoritative) | Failure stance | Advisory, cacheable, pipeline falls back | Hard dependency in the deploy path |
| [ADR-007](#adr-007-three-binaries-not-one) | Packaging | Three binaries: hub, agent, client | One binary with subcommands |
| [ADR-008](#adr-008-cluster-state-has-three-values-not-two) | Cluster state | `LIVE`, `DARK`, `DRAINING` | `LIVE` and `DARK` only |
| [ADR-009](#adr-009-an-agent-may-only-report-for-the-cell-that-names-it) | Reporter identity | Each `Cluster` names the issuer and subject allowed to report for it | Trusting any authenticated caller, or matching on the subject alone |
| [ADR-010](#adr-010-the-audit-trail-is-structured-stdout-and-cannot-be-levelled-off) | Audit trail | Structured JSON on stdout, unlevelled, plus a lossy Events view | A database, a bespoke sink, a retention policy of our own |
| [ADR-011](#adr-011-the-api-path-runs-n-replicas-and-only-the-controllers-elect) | High availability | Every replica answers placements; a lease covers only the reconcilers | Leader-only serving, an active-standby pair, sharing capacity between replicas |

---

### ADR-001: the registry is a CRD, not a database

**Context.** The cluster registry is the durable half of the system: endpoint, CA bundle, provider,
trust configuration reference, labels, state. It is written rarely and read on every placement.

**Decision.** A `Cluster` custom resource in cellcast's own hub cluster. `POST /api/v1/clusters`
and `GET /api/v1/clusters` are a thin layer over it, so both `kubectl` and the HTTP API are
first-class interfaces to the same object.

**Consequences.** Durability, RBAC, audit logging, admission control, watch semantics and optimistic
concurrency all come from the API server rather than from code written here. Getting started is one
`helm install` with no StatefulSet, which is what makes the three-kind-cluster demo reproducible by
anyone who clones the repo. The cost is a hard dependency on running in a Kubernetes cluster, which
is acceptable: every deployment target this tool addresses is already Kubernetes.

**Rejected.** Postgres and DynamoDB add an operational dependency for data that is small, slowly
changing, and already has a perfectly good home. Redis was only ever a candidate for capacity, and
ADR-002 removes that need too.

---

### ADR-002: capacity is in-memory with a staleness guard

**Context.** Utilisation is the only high-write data in the system. It is also worthless the moment
it is stale, which means it never needs durability.

**Decision.** An in-memory index in the hub process, rebuilt from agent heartbeats on restart. Each
entry carries `observedAt`. Past a configurable staleness window (default 90 seconds, three
heartbeats) the cell is marked `Unknown`.

**Consequences.** No external store anywhere in the architecture. A hub restart costs one heartbeat
interval of blindness, during which affected cells are `Unknown` and therefore excluded rather than
mis-scored.

**The staleness guard is not optional, and this is why.** Naive least-loaded reads a missing capacity
entry as zero utilisation, and zero utilisation is the best possible score. Without the guard, the
first cluster to break badly enough that its agent stops reporting becomes the target for every
deploy in the estate. So:

- `Unknown` cells are excluded from scoring. They are never treated as empty.
- If every candidate is `Unknown`, placement fails closed with a distinct error rather than guessing.
- Staleness is a first-class metric with a documented alert.

**Implemented** as `internal/hub/capacity`, which knows nothing about Kubernetes, HTTP or policy: a
bounded map with an injectable clock, which is what makes the staleness guard testable without a
cluster. Four decisions in it are load-bearing:

- **Health is computed at read time, never written by the prune loop.** A stalled or crashed sweeper
  must not be able to leave a stale entry looking fresh, which is the failure mode that turns the
  guard into decoration.
- **Utilisation is the worst dimension, not the mean.** A cell at 95% memory and 10% CPU is not half
  loaded, it is nearly full. Averaging is how a scheduler keeps sending work to a cell that is one
  pod away from evicting things. A dimension with zero allocatable is skipped rather than counted as
  zero pressure, for the same reason.
- **Commitment is measured from pod requests, not usage.** The scheduler places on requests, so
  requests decide whether the next deploy fits. A cell can sit at 20% CPU usage and be completely
  unschedulable.
- **An invalid report is rejected, not clamped.** A clamped value produces a plausible utilisation
  that steers real deploys, and the agent that sent it never learns it is broken. A rejected report
  also leaves the previous good one in place, so a broken agent cannot blank out its cell.

A report is refused unless the cell is already a registered `Cluster`. That is the correctness rule
and it is also the bound on the index: without it an authenticated caller could fill hub memory with
heartbeats for names it invented.

---

### ADR-003: callers authenticate by OIDC workload identity federation

**Context.** An unauthenticated API that mints scoped cluster credentials is a privilege escalation
service, not a security tool. Something has to decide whether the caller is allowed to ask.

**Decision.** The caller presents the workload identity JWT its CI platform already issues. cellcast
validates the signature against the issuer's JWKS, enforces `iss` against an allowlist, binds `aud`
to this cellcast instance, checks `exp` and `nbf` with bounded clock skew, and extracts provider
specific claims behind an interface (GitHub Actions, Buildkite, more additive).

**Consequences.** No shared secret exists anywhere in the chain. Moving a CI credential to a better
hiding place does not solve the problem; removing it does. The cost is that cellcast is only as
trustworthy as the CI platform's issuer, which is a dependency stated plainly rather than hidden.

**Rejected.** Static bearer tokens reintroduce exactly the long-lived shared secret this project
exists to eliminate. mTLS is defensible but requires the adopter to run a certificate lifecycle for
ephemeral CI runners, which is a larger operational burden than the problem it replaces.

Pipelines authenticate; human callers are a separate design problem and are out of scope.

---

### ADR-004: downstream credentials are minted, never stored

**Context.** The credential returned to the pipeline is the thing that makes cellcast useful and the
thing that makes it dangerous.

**Decision.** cellcast holds trust configuration and mints on demand. Kubernetes `TokenRequest` is
the only implementation. AWS STS `AssumeRoleWithWebIdentity` sits behind the same trust-provider
interface, which was designed for both, and is [under
consideration](https://github.com/ethan-kane-ops/cellcast/blob/main/ROADMAP.md) rather than built.

TTL is policy, not a constant. It resolves from the target cell's `env` label: production short,
development longer, with a hard ceiling the operator sets. A caller may request a shorter TTL than
policy allows. A caller may never request a longer one.

**Consequences.** `TokenRequest` runs end to end on kind with no cloud account, which keeps the
demo reproducible. Shipping one provider first settles the broker semantics before a second
implementation calcifies them.

**Rejected.** Stored kubeconfigs handed to callers, in any form. A single global 15-minute TTL was
the original plan and is the wrong default in both directions at once: too long for production, too
short for a slow dev deploy.

#### As implemented

**The production default is 10 minutes, not the 5 this record originally specified.** The Kubernetes
`TokenRequest` API rejects any `expirationSeconds` below 600: *"may not specify a duration less than
10 minutes"*. Five minutes was never achievable. AWS STS has a 15-minute floor of its own, so the
constraint is not a Kubernetes quirk that a second provider would remove. A provider declares its
floor and the broker reasons about it.

The resolved bounds, narrowest layer last:

| Layer | Source | May widen? |
| --- | --- | --- |
| Built-in defaults | the cell's `env` label | n/a |
| Policy override | `PlacementPolicy.spec.tokenTTL` | yes, it is operator-authored |
| Hub ceiling | `--token-max-ttl` | never |

| `env` | Default | Max |
| --- | --- | --- |
| `prod`, `production` | 10m | 15m |
| `stage`, `staging` | 15m | 30m |
| `dev`, `development` | 30m | 60m |
| anything else, including unset | 10m | 15m |

An unlabelled cell gets the **tightest** bounds, not the loosest and not a middle value. The
realistic mistake is registering a production cell and forgetting the label, and that must not be
the thing that grants an hour of cluster access.

Two rules follow from the floor and they point in opposite directions. A *request* below the floor is
raised to it, because the operator's ceiling still holds. A *ceiling* below the floor is refused,
because minting a ten-minute credential under a five-minute policy would overrule the operator on the
one number they set to bound a compromise, in the direction that grants more access.

**The hub ceiling is the only ceiling that is certain to exist.** A Kubernetes API server applies no
maximum of its own unless `--service-account-max-token-expiration` was configured; a default cluster
will issue a token lasting years if asked for one. `--token-max-ttl` is therefore a load-bearing
control rather than defence in depth, and it is enforced against the token that came back as well as
the number that went out.

---

### ADR-005: placement is filter-then-score, and policy is a CRD

**Context.** "Least-loaded or round-robin" describes an optimisation step. On its own it is unsafe.

**Decision.** Two phases in a fixed order. Filter first: which cells is this caller permitted to
reach, and which are eligible right now. Score second, over the survivors only. Policy is a
`PlacementPolicy` custom resource that maps authenticated claims to a label selector over the
registry.

Scoring strategy is chosen by policy, not by the request. A caller must not be able to pick the
strategy that lands it in the cell it wants.

**Consequences.** This is the confused-deputy defence, and it is what gives the system an
authorization model rather than a scheduling heuristic. Deny by default: a caller with no matching
policy is rejected cleanly. Policy lives in Git, is reviewable in a pull request, and is auditable
through the API server, because policy that nobody can diff is policy that drifts.

**Implemented** as `internal/hub/placement`. Three filter stages run in a fixed order, and a cell is
recorded at the first one that refuses it:

| Stage | Refuses |
| --- | --- |
| `permission` | the cell is outside the caller's policy selector |
| `state` | the cell is `DRAINING`, or `DARK` without a permitted dark-targeting request |
| `capacity` | the cell has never reported, or its report is stale and it is `Unknown` |

Permission is first so that a rejection can never disclose the state or utilisation of a cell the
caller may not reach. Every registered cell appears in the trace exactly once with its verdict, which
is what `--explain` renders.

**Overlapping policies resolve to the most specific.** A policy constraining `{repository,
environment}` beats one constraining `{repository}`, because that is what an operator writes when
they mean to carve an exception out of a general rule. Equal specificity is broken by name so the
answer is stable, and logged so the ambiguity is visible rather than silently resolved.

**Targeting a dark cell without permission is refused, not downgraded.** Silently turning a QA smoke
test into an ordinary placement lands it on a live cell, which is the opposite of what was asked for.

**Four distinguishable refusals**, because they call for different operator action and different
client behaviour: no matching policy, policy permits no registered cell, no permitted cell is
accepting, and no permitted cell has usable capacity. Collapsing them would make an authorization
refusal indistinguishable from a fleet-wide capacity blackout.

---

### ADR-006: cellcast is advisory, not authoritative

**Context.** cellcast introduces a new single point of failure into the deploy critical path. If the
hub is down and every pipeline blocks, the cure is worse than the disease, and this is the first
objection any platform team will raise.

**Decision.** The placement response is a recommendation with a stated confidence, not a command.
The client caches the last successful placement per workload with a TTL. The caller declares its
stance up front: `--on-unavailable=fail`, `--on-unavailable=last-known`, or
`--on-unavailable=<cell>`. There is no implicit fallback, because an implicit fallback is how a
deploy silently lands in the wrong cluster.

**Fail closed on authorization, fail open on optimisation.** These are different failures and must
be treated differently:

- Cannot determine whether the caller is permitted: **refuse**.
- Cannot determine which permitted cell is least loaded: **any permitted cell is fine**.

**Where that sits against ADR-002, which says an all-`Unknown` fleet fails closed.** The two are not
in conflict once the layer is named. The hub always refuses and returns a *distinguishable* error;
falling open is a client stance, declared per caller by `--on-unavailable`, never a hub default. So
partial capacity loss is handled inside the hub by scoring the cells that did report, and total
capacity loss is handed to the caller as `ErrCapacityUnknown` for its declared fallback to act on. A
hub that picked a cell for itself when it could not tell which was least loaded would be guessing
with someone else's production traffic.

**Token minting never falls back.** A cached placement is a cached decision, never a cached
credential. The client re-mints or fails.

**What that costs.** Minting runs through the hub, so a hub that cannot be reached cannot issue a
credential either. A fallback therefore returns a cell and nothing else. That serves two kinds of
pipeline: one that needs only the cell name (`--dry-run`, to pick a values file or a target it
already holds access to), and one that keeps a break-glass credential for this situation. A pipeline
with neither cannot deploy through a hub outage, and `--on-unavailable` will not change that.

**Which failures a stance may answer.** The classification lives in `internal/refusal`, imported by
both the hub and the client so the two cannot drift apart:

| Refusal | Status | May a stance answer it |
| --- | --- | --- |
| `NoPolicy`, `DarkNotPermitted`, `NoPermittedCells` | 403, 409 | No. Authorization. A flag must not be a way around policy |
| `InvalidRequest` | 400 | No. A fallback would hide the typo and deploy anyway |
| `NoEligibleCells`, `CapacityUnknown` | 503 | Yes. The caller is permitted and the hub cannot rank |
| `PlacementUnavailable` | 503 | Yes. The hub is up and cannot decide |
| `MintUnavailable`, `MintFailed` | 503 | No. Minting never falls back |
| No response, or a gateway's 502/503/504 | any | Yes. Nothing was decided, so nothing was refused |

`PlacementUnavailable` and `MintUnavailable` were one code until this was implemented. They look
identical on the wire and call for opposite behaviour, which is the sort of thing that only shows up
when something downstream has to act on it.

**Stated confidence.** Every decision says where it came from and how much was known when it was
made. `source` is `hub`, `cache` or `pinned`. `confidence` is `high` when every permitted, eligible
cell reported fresh capacity and was ranked; `degraded` when at least one was excluded because its
capacity was stale or had never arrived; `stale` for a replayed decision; `none` for a pinned cell.
A drained or dark cell does not degrade a decision, because an operator excluded it deliberately.
Counting it would make the signal meaningless during any planned maintenance.

**Two details that are not obvious and matter more than the rest of the feature.** A fallback
deletes the kubeconfig at the target path, because a credential left by an earlier run is a live
credential for whichever cell *that* run chose, and the next step would use it without anyone having
selected where the deploy landed. And a fallback never writes the cache, or an entry would keep
renewing its own lifetime and a decision could outlive its TTL for as long as the outage lasted.

---

### ADR-007: three binaries, not one

**Context.** The system has three deployables with different runtimes, different distribution
channels, and different privilege levels.

**Decision.** `cmd/cellcast-hub`, `cmd/cellcast-agent`, `cmd/cellcast`.

**Consequences.** This is a threat-model argument, not tidiness. The agent runs in every registered
spoke, which is the largest attack surface in the system by count, and it must not link the broker's
minting code. Separate binaries make that a compile-time guarantee rather than a code review
convention. It also keeps the client small enough to ship as an archive a pipeline can fetch and
unpack, without dragging controller-runtime along.

**Rejected.** One binary with `serve` and `agent` subcommands. Simpler to build, but it puts the
minting code on every spoke in the estate.

---

### ADR-008: cluster state has three values, not two

**Context.** `LIVE` and `DARK` do not cover the most common real reason a cell must stop receiving
deploys: it is being upgraded.

**Decision.** A three-value state on the `Cluster` resource.

| State | Receives new placements | Serves production traffic |
| --- | --- | --- |
| `LIVE` | yes | yes |
| `DARK` | only when explicitly requested by a dark-targeting caller | no |
| `DRAINING` | no | yes |

**Consequences.** `DRAINING` is what makes cellcast useful during a cluster upgrade window: existing
workloads keep serving, new deploys route elsewhere automatically, and nobody edits a pipeline.
Transitioning a cell to `DRAINING` must not fail placements already issued. Because state lives on
the CRD, `kubectl patch` is a legitimate operator interface and every transition is visible in the
API server audit log.

**Implemented.** The semantics above live on the `ClusterState` type itself (`AcceptsPlacement`,
`ServesProductionTraffic`) rather than in the hub, so a consumer outside this repository reads the
rules from the API types instead of keeping a second copy of the table to drift from. The `ClusterReconciler` publishes the hub's view as
`status.conditions[AcceptingPlacements]` and `status.stateSince`, so "why is nothing landing here"
and "how long has this been draining" are both answered by `kubectl get cluster`.

**`DRAINING` refuses an explicitly dark-targeted request too.** A cell in the middle of an upgrade is
not a safe target for a smoke test either, so `DRAINING` is not a synonym for "dark to everyone
except QA".

**Nothing has to be done to protect placements already issued.** A placement is an advisory decision
returned to the caller and never retained by the hub (ADR-006). There is no in-flight record for a
state change to invalidate, which is why the guarantee is structural rather than a piece of
transition-handling code.

---

### ADR-009: an agent may only report for the cell that names it

**Context.** Capacity steers every placement, so whoever can write it can steer deploys. The ingest
path authenticated the caller from the start, but authentication alone does not separate cells:
every agent in the fleet holds a valid token, so any of them could report for any registered cell.

**Decision.** `Cluster.spec.reporter` names an `issuer` and a `subject`. A capacity report is
accepted only when the authenticated caller matches both, compared literally. The agent presents a
projected ServiceAccount token carrying cellcast's audience, verified through the same OIDC path as
a CI caller's token, so the hub has one authentication path rather than two.

**Consequences.** The subject alone would not have worked. Every cell runs the agent under the same
ServiceAccount, so `sub` is the identical string fleet-wide and matching on it would have bound
nothing. The issuer is what separates one cell from another, which makes a distinct service account
issuer URL per spoke a real prerequisite rather than a detail: EKS, GKE and AKS each give a cluster
its own, while a stock kubeadm or kind cluster issues as
`https://kubernetes.default.svc.cluster.local` and every one of them collides. Two clusters claiming
one issuer URL do not share a key set, so such a fleet fails as a rejected agent rather than as an
accepted impostor, but it does fail, and the fix is `--service-account-issuer` per cluster.

**An unset `spec.reporter` refuses everything.** A cell that names no reporter accepts no reports, so
it stays `Unknown` and drops out of scoring. That is the same place the staleness guard puts a cell
whose agent has died, which is a state the hub detects and an operator can see. Reading the absent
field as "anyone may report" would be the wildcard that made the field decorative.

**Rejected.** Trusting any authenticated caller, which is what the ingest path did before and what
docs/threat-model.md T-07 recorded as open. Also rejected: a shared secret per fleet, which makes
every cell a place to steal it from, and mutual TLS, which puts a private key and a rotation problem
in every spoke to answer a question the cluster's own token service already answers.

---

### ADR-010: the audit trail is structured stdout, and cannot be levelled off

**Context.** A credential broker with no audit trail is unadoptable. The first question in any
security review is "show me every token this thing has ever minted, and who asked for it", and there
has to be an answer that does not involve a debug build. The second question, which reviews ask less
often and operators ask constantly, is why a deploy was refused.

**Decision.** One structured JSON record per placement decision and per mint, written to stdout, and
the same events published a second time as Kubernetes Events on the `Cluster` they concern.
Refusals are recorded with the same fields as successes, including the candidate table naming every
registered cell and the filter stage that rejected it. `docs/audit.md` holds the record shape and a
worked example.

**Stdout and nothing else.** Records land in whatever log pipeline the adopter already runs, so
retention, access control and immutability stay where those decisions already live. A database would
mean cellcast owning a retention policy, a schema migration and a second failure mode in the deploy
critical path, in exchange for nothing an adopter's log pipeline does not already do better.

**The trail is not levelled by `--log-level`.** It is written through its own handler that admits
everything it is given, so `--log-level=error` still produces a complete trail. Sharing a handler
with the ordinary log would make the audit trail switchable by a flag nobody thinks of as a security
control, and a security log a verbosity flag can silence is not a security log.

**No record can hold a token.** The record type has no field that could carry credential material.
The closest is `token_sha256`, a digest of the whole token, which correlates a leaked token to the
record that issued it and cannot reconstruct it. A prefix would correlate too and is forbidden: a
JWT's leading bytes are its header and the segment boundaries move, so a prefix is not a fixed
amount of the secret. The invariant is held by a test that classifies every field on the record and
fails on any new one, rather than by a rule in this document (docs/threat-model.md T-05).

**Placement and mint are separate records, tied by `request_id`.** Either happens without the other:
a dry run places and never mints, and a mint fails against a cell that placement legitimately chose.
Collapsing them would make "every token issued" a query over records that mostly are not tokens.

**The Events view is lossy by design.** Kubernetes aggregates and spam-filters events, so a busy
hub will have some collapsed or dropped. Making that view complete would mean an API server write per
deploy across the estate, in the critical path. The JSON trail is authoritative; the Events exist so
an operator already looking at a cell can see who has been deploying to it.

**The Prometheus counters ride the same record.** Every fact a placement or mint metric needs is
already on an audit.Record, so the metrics view is a second `Notifier` rather than a second pass over
the placement handler. One instrumentation point cannot disagree with itself, so a refusal that is
audited is also counted (docs/metrics.md).

**Rejected.** A database or any bespoke sink, for the reasons above. Also rejected: auditing only
successes, which produces a log that cannot answer a refused pipeline's question; and emitting one
combined record per request, which conflates a decision with a credential.

---

### ADR-011: the API path runs N replicas, and only the controllers elect

**Status:** accepted

**Context.** cellcast is in the deploy critical path, so a single replica is a single point of
failure for every deploy in the estate. But the hub is two programs in one process. The API answers
placements and mints credentials; the controllers write `Cluster` status and emit Events. Running N
of the first is the point of the exercise. Running N of the second means three replicas fighting
over the same status subresource and emitting every event three times.

**Decision.** Every replica serves the API. A lease covers the controllers only.

This works because of ADR-002 and ADR-005 together: placement reads the informer cache, which every
replica keeps synced whether or not it holds the lease, and the capacity index that ranks the
survivors is per-replica and rebuilt from heartbeats. A follower therefore answers a placement
exactly as well as the leader does. Nothing in the decision path writes.

Three consequences follow, and each one needed code:

**Replicas disagree about capacity, and that is acceptable.** Agents heartbeat through the Service,
so each report lands on whichever replica the load balancer chose. Two replicas asked the same
question in the same second can pick different cells. Placement is advisory (ADR-006), so a
marginally worse cell is a worse cell, not a wrong one. What is *not* acceptable is a replica with
no capacity at all, which is the next point.

**A replica must be warm before it is ready.** A newly started replica has an empty capacity index.
Every cell is `Unknown`, so it excludes the entire fleet and refuses every placement it is handed.
Readiness therefore waits until the index holds a fresh report for every cell that is expected to
report: every registered `Cluster` that names a reporter and is not `DRAINING`.

That wait has to be bounded, and the reason is a cycle. Agents heartbeat through the Service; a
Service routes only to ready pods; so a fleet whose hub replicas all restarted at once is waiting
for reports that nothing can deliver. Past `--warmup-timeout` a replica goes ready anyway, says so
in the log, and names the cells it never heard from. It keeps refusing placements until it is warm,
with `PlacementUnavailable` rather than `CapacityUnknown`, because the two resolve differently: the
first is a property of the replica the caller reached and clears itself.

Warmth latches. Capacity going stale later is the placement engine's problem and it already has an
answer. If warmth could fall back, readiness would follow it, and one bad minute across the fleet
would pull every replica out of the Service at once.

**Shutdown waits before it closes.** Kubernetes removes a pod from Service endpoints
asynchronously, and it sends `SIGTERM` at the same moment it begins. A process that closes its
listener on the signal refuses everything routed to it while that removal propagates. So the hub
goes unready, keeps serving for `--drain-delay`, and only then drains in-flight requests within
`--shutdown-timeout`. The two together are sized to fit inside the default
`terminationGracePeriodSeconds`, because a drain the kubelet interrupts with `SIGKILL` is not a
drain.

**Rejected: leader-only serving.** One replica answering and the others idle is an active-standby
pair with extra steps. It converts every lease handover into an API outage and wastes the property
that makes this service easy to scale, which is that deciding does not write.

**Rejected: sharing the capacity index between replicas.** Gossip or a shared cache would make
replicas agree. It would also give the hub a distributed system to be wrong about, in exchange for
agreement on a number that is advisory and stale by construction. ADR-002 already decided that
capacity does not warrant durability. It does not warrant consensus either.

**Consequence.** Losing the lease stops the process, so whichever half fails takes the other down
and a partial failure surfaces as a restart. Under an API server partition
only the leader exits; followers stay in their acquisition loop and keep serving from cache. The
restarted replica cannot sync, so it stays unready and out of the Service until the partition
clears.

### Deployment

Two charts, not one: `charts/cellcast` installs the hub into the hub cluster and
`charts/cellcast-agent` installs the reporter into each cell. The split follows
ADR-007. Bundling them would make every spoke carry the hub's CRDs, RBAC and
minting configuration for a component it does not run, in a cluster where that
configuration is exactly what an attacker would want to find.

The hub chart templates its CRDs rather than using Helm's `crds/` directory,
which is installed once and never upgraded. Templating means `helm upgrade`
carries a schema change, and `crds.install=false` hands the CRDs to whatever
manages them out of band. They are annotated `helm.sh/resource-policy: keep`, so
uninstalling the hub does not delete the registry with it.

## Data model

Two custom resources, both `v1alpha1`, versioned from the start so a `v1beta1` is additive rather
than breaking.

**`Cluster`** is the durable registry entry: endpoint, CA bundle, provider (`eks`, `gke`, `aks`,
`generic`), a reference to trust configuration, the identity permitted to report capacity for the
cell, arbitrary labels (`group`, `env`, `region` and whatever the operator adds), and state.
Registration writes trust configuration only. A registration payload carrying a static kubeconfig
token is rejected, which is the invariant that keeps ADR-004 true.

**`PlacementPolicy`** maps authenticated caller claims to a permitted label selector, a scoring
strategy, and TTL bounds.

Capacity is not a resource. It lives in memory, per ADR-002.

## Failure behaviour

| Failure | Behaviour |
| --- | --- |
| Hub unreachable | Client applies its declared `--on-unavailable` stance, and the default stance is to fail (ADR-006) |
| Hub reachable but refusing on authorization | No stance applies. A fallback is never a way past a refusal (ADR-006) |
| Hub reachable but unable to rank | A stance may answer it, because the caller was already found to be permitted |
| A stance answered | A cell and no credential. The stale kubeconfig at the target path is removed |
| Agent stopped reporting | Cell goes `Unknown` after the staleness window and drops out of scoring |
| Agent cannot reach the hub | Retries with jittered backoff, buffering nothing; the cell goes `Unknown` rather than being scored on stale numbers |
| Cell names no reporter | No capacity is accepted for it, so it stays `Unknown` and is never scored (ADR-009) |
| All candidate cells `Unknown` | Placement fails closed with a distinct error |
| Caller JWT invalid or unverifiable | Reject, with a structured reason that is logged and metered |
| JWKS endpoint unreachable | Serve from cache; never fall back to accepting unverified tokens |
| No matching `PlacementPolicy` | Reject. There is no "any cell" fallback |
| Mint fails | Reject. Placement without a credential is not a partial success |
| Hub replica still warming up | Ready, so agents can reach it, but every placement is refused with `PlacementUnavailable` until it is warm (ADR-011) |
| Hub replica being rolled | Reports unready, keeps serving for `--drain-delay`, then drains in-flight requests (ADR-011) |
| Leader loses its lease | That replica exits and restarts; the others keep serving placements from cache (ADR-011) |

# Architecture

cellcast answers two questions for a deploy pipeline that already exists:

1. **Where does this workload go?**
2. **What credential lets me deploy it there, right now, for as short a time as possible?**

It does not deploy anything. It does not propagate manifests, own a GitOps loop, or sit in the
data path of a running application. Every existing tool in this space (Open Cluster Management,
Karmada, Rancher Fleet) answers question 1 only if you first adopt its control plane and its
propagation model. cellcast is a queryable oracle you bolt onto the pipeline you already have.

## Non-goals

Stated up front because the security argument depends on them:

- cellcast **never stores a credential**. Not encrypted, not in a vault, not in etcd.
- cellcast **never proxies traffic** to a managed cluster.
- cellcast **never retains a token** after returning it.
- cellcast **never propagates or applies** a manifest.

It holds trust configuration and mints on demand. If the hub process is compromised, the attacker
gains the ability to mint within the operator's policy ceiling. They do not gain a vault to drain.
That distinction is the entire security argument, and [the threat model](threat-model.md) tests it
honestly rather than asserting it.

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
| `cellcast-agent` | every registered spoke | reports capacity to the hub on a heartbeat |
| `cellcast` | the pipeline runner or an operator's laptop | client CLI |

## The placement path

One request, in order. Each step can reject, and the order is load-bearing.

1. **Authenticate.** The caller presents the workload identity JWT its CI platform already issues.
   The hub validates it against the issuer's JWKS. There is no shared secret anywhere in this
   chain. (ENG-172)
2. **Filter by permission.** The authenticated claims resolve to a `PlacementPolicy`, which
   expresses the permitted cells as a label selector over the `Cluster` registry. No matching
   policy means rejection, never a fallback to "any cell". (ENG-173)
3. **Filter by eligibility.** From the permitted set, drop cells that are `DRAINING`, cells whose
   capacity is `Unknown`, and cells that are `DARK` unless the caller explicitly asked for dark.
   (ENG-111, ENG-112)
4. **Score.** Of the survivors, pick one: least-loaded by default, round-robin as an alternative,
   chosen by policy rather than by the caller. Ties break deterministically. (ENG-173)
5. **Mint.** Request a short-lived credential scoped to the chosen cell and the workload's
   namespace boundary, with a TTL resolved from policy. Return it. Forget it. (ENG-113)

Steps 2 and 3 are the filter phase; step 4 is the optimisation phase. Running them in the other
order is the bug that routes a dev pipeline into the least-loaded production cluster in another
region.

`--explain` renders steps 2 through 4 as a table so a surprising placement is legible without a
debug build. `--dry-run` runs 1 through 4 and stops before 5. (ENG-114)

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
- Staleness is a first-class metric with a documented alert. (ENG-178)

---

### ADR-003: callers authenticate by OIDC workload identity federation

**Context.** An unauthenticated API that mints scoped cluster credentials is a privilege escalation
service, not a security tool. Something has to decide whether the caller is allowed to ask.

**Decision.** The caller presents the workload identity JWT its CI platform already issues. cellcast
validates the signature against the issuer's JWKS, enforces `iss` against an allowlist, binds `aud`
to this cellcast instance, checks `exp` and `nbf` with bounded clock skew, and extracts provider
specific claims behind an interface (GitHub Actions, Buildkite, more additive).

**Consequences.** No shared secret exists anywhere in the chain. This is the point: the CI credential
problem is not solved by moving the secret to a better hiding place, it is solved by removing the
secret. The cost is that cellcast is only as trustworthy as the CI platform's issuer, which is a
dependency worth stating plainly rather than hiding.

**Rejected.** Static bearer tokens reintroduce exactly the long-lived shared secret this project
exists to eliminate. mTLS is defensible but requires the adopter to run a certificate lifecycle for
ephemeral CI runners, which is a larger operational burden than the problem it replaces.

v0.1 authenticates pipelines only. Human callers are a separate design problem and are explicitly
out of scope.

---

### ADR-004: downstream credentials are minted, never stored

**Context.** The credential returned to the pipeline is the thing that makes cellcast useful and the
thing that makes it dangerous.

**Decision.** cellcast holds trust configuration and mints on demand. v0.1 implements Kubernetes
`TokenRequest` only. AWS STS `AssumeRole` lands in v0.2 behind the same trust-provider interface,
which is designed for both from day one.

TTL is policy, not a constant. It resolves from the target cell's `env` label: production short
(default 5 minutes), development longer (default 30 minutes), with a hard ceiling the operator sets.
A caller may request a shorter TTL than policy allows. A caller may never request a longer one.

**Consequences.** `TokenRequest` demos end to end on kind with no cloud account, which keeps the
recorded demo reproducible. Shipping one provider first gets the broker semantics right before a
second implementation calcifies them.

**Rejected.** Stored kubeconfigs, in any form. A single global 15-minute TTL was the original plan
and is the wrong default in both directions at once: too long for production, too short for a slow
dev deploy.

---

### ADR-005: placement is filter-then-score, and policy is a CRD

**Context.** "Least-loaded or round-robin" describes an optimisation step. On its own it is unsafe.

**Decision.** Two phases in a fixed order. Filter first: which cells is this caller permitted to
reach, and which are eligible right now. Score second, over the survivors only. Policy is a
`PlacementPolicy` custom resource that maps authenticated claims to a label selector over the
registry.

Scoring strategy is chosen by policy, not by the request. A caller must not be able to pick the
strategy that lands it in the cell it wants.

**Consequences.** This is the confused-deputy defence, and it is the difference between a scheduling
toy and something with an authorization model. Deny by default: a caller with no matching policy is
rejected cleanly. Policy lives in Git, is reviewable in a pull request, and is auditable through the
API server, because policy that nobody can diff is policy that drifts.

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

**Token minting never falls back.** A cached placement is a cached decision, never a cached
credential. The client re-mints or fails.

---

### ADR-007: three binaries, not one

**Context.** The system has three deployables with different runtimes, different distribution
channels, and different privilege levels.

**Decision.** `cmd/cellcast-hub`, `cmd/cellcast-agent`, `cmd/cellcast`.

**Consequences.** This is a threat-model argument, not tidiness. The agent runs in every registered
spoke, which is the largest attack surface in the system by count, and it must not link the broker's
minting code. Separate binaries make that a compile-time guarantee rather than a code review
convention. It also keeps the client small enough to ship through a Homebrew tap without dragging
controller-runtime along.

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

**Implemented in ENG-112.** The semantics above live on the `ClusterState` type itself
(`AcceptsPlacement`, `ServesProductionTraffic`) rather than in the hub, because darkgate (ENG-92)
consumes the same values from a different repository and a second copy of the table is how the two
drift apart. The `ClusterReconciler` publishes the hub's view as
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

## Data model

Two custom resources, both `v1alpha1`, versioned from the start so a `v1beta1` is additive rather
than breaking.

**`Cluster`** is the durable registry entry: endpoint, CA bundle, provider (`eks`, `gke`, `aks`,
`generic`), a reference to trust configuration, arbitrary labels (`group`, `env`, `region` and
whatever the operator adds), and state. Registration writes trust configuration only. A registration
payload carrying a static kubeconfig token is rejected, which is the invariant that keeps ADR-004
true.

**`PlacementPolicy`** maps authenticated caller claims to a permitted label selector, a scoring
strategy, and TTL bounds.

Capacity is deliberately not a resource. It lives in memory per ADR-002.

## Failure behaviour

| Failure | Behaviour |
| --- | --- |
| Hub unreachable | Client applies its declared `--on-unavailable` stance (ADR-006) |
| Agent stopped reporting | Cell goes `Unknown` after the staleness window and drops out of scoring |
| All candidate cells `Unknown` | Placement fails closed with a distinct error |
| Caller JWT invalid or unverifiable | Reject, with a structured reason that is logged and metered |
| JWKS endpoint unreachable | Serve from cache; never fall back to accepting unverified tokens |
| No matching `PlacementPolicy` | Reject. There is no "any cell" fallback |
| Mint fails | Reject. Placement without a credential is not a partial success |

## Related tickets

Every decision here has an implementing ticket: ENG-193 (foundation), ENG-110 (registry), ENG-111
(capacity), ENG-112 (state), ENG-113 (broker), ENG-114 (placement API and client), ENG-172 (OIDC),
ENG-173 (policy), ENG-174 (agent), ENG-175 (advisory mode), ENG-177 (test harness).

# Threat model

cellcast decides where a workload deploys and mints the credential that lets it. Both halves are
security-relevant, and the second one means a compromise here is a compromise of every cluster in
the registry, bounded only by policy. This document states what that bound actually is.

**Status of this document.** Written before implementation, as the design contract the code is held
to. Every mitigation below names its implementing ticket, or is listed explicitly as a risk accepted
for v0.1. Statuses are updated as tickets land: `Planned` means the control does not exist yet, and
`Implemented` means it is in the code with a test that names this threat.

## Scope

**In scope.** The hub API and controllers, the reporter agent, the client CLI, the credential
minting path, and the trust relationships between them.

**Out of scope.** The security of the CI platform issuing caller identity tokens, the security of
the managed clusters themselves, and the Kubernetes API server that backs the hub. cellcast inherits
the trust properties of all three. An adopter who cannot trust their CI platform's OIDC issuer
cannot trust cellcast, and that is a real limitation rather than a caveat to bury.

## Assets

Ranked by what an attacker actually wants:

1. **The ability to mint.** cellcast's own credentials against registered clusters. This is the
   crown jewel and the reason the tool is worth attacking.
2. **Minted tokens in flight.** Short-lived and scoped, but live.
3. **The registry.** Knowing every cluster in an estate, its endpoint, and its environment labels is
   reconnaissance worth having on its own.
4. **Placement policy.** Modifying it is how an attacker turns a dev pipeline into a prod deploy.

Note what is absent: there is no credential store. cellcast holds no long-lived downstream
credential to steal, by construction (see [ADR-004](architecture.md#adr-004-downstream-credentials-are-minted-never-stored)).

## Trust boundaries

```
   untrusted                     semi-trusted                    trusted
   ---------                     ------------                    -------

   caller JWT      --[B1]-->     cellcast-hub       --[B3]-->    hub API server
   (attacker                     (validates,                     (etcd, RBAC,
    controlled                    filters,                        audit)
    bytes)                        mints)
                                      ^
   agent heartbeat --[B2]-------------+
   (spoke cluster,
    lower trust
    than the hub)

   minted token    --[B4]-->     pipeline runner    --[B5]-->    spoke API server
```

| Boundary | Crossing | Assumption |
| --- | --- | --- |
| B1 | Caller JWT into the hub | Fully attacker-controlled until signature verification passes |
| B2 | Agent heartbeat into the hub | Authenticated, but a spoke is less trusted than the hub |
| B3 | Hub into its own API server | Bounded by cellcast's ServiceAccount RBAC |
| B4 | Minted token into a CI runner | The runner is shared, log-capturing, and frequently compromised |
| B5 | Token into a spoke API server | Bounded by the token's audience, scope and TTL |

## Threats

### T-01: compromised cellcast hub process

**What an attacker gets.** The ability to mint any credential the operator's policy permits, for as
long as they hold the process. Not a vault to drain: there is nothing stored to exfiltrate.

**Honest blast radius.** This is the worst case in the system and it is bad. An attacker with code
execution in the hub can mint into every cell in the registry, subject to two ceilings: the policy
ceiling the operator configured, and cellcast's own RBAC in each spoke. Those ceilings are the only
thing standing between a hub compromise and estate-wide cluster access. Anyone deploying this should
size cellcast's per-spoke RBAC as tightly as their deploys allow, and should not grant it
`cluster-admin` because it was convenient during the pilot.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| Policy ceiling on TTL that no request can raise | ENG-113 | Planned |
| Per-cell RBAC scoping documented and minimal by default | ENG-113, ENG-180 | Planned |
| Audit record for every mint, so a compromise is reconstructable | ENG-176 | Planned (v0.2) |
| Distroless non-root image, read-only root filesystem, no shell | ENG-180 | Planned |
| Signed images and SBOM so the running binary is the reviewed one | ENG-182 | Planned (v1.0) |

**Residual risk.** Detection depends on the audit trail, which is v0.2. For v0.1 a hub compromise is
reconstructable only from the spoke clusters' own API server audit logs. Stated as an accepted risk
below.

---

### T-02: forged or replayed caller JWT

**What an attacker gets.** If forgery succeeds, everything the impersonated pipeline could get. This
is the primary remote attack and the JWT parser is the most exposed code in the project.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| Issuer allowlist checked before any attacker-influenced parsing | ENG-172 | Implemented |
| Issuer compared literally, never by prefix or suffix | ENG-172 | Implemented |
| Full signature verification against the issuer's JWKS | ENG-172 | Implemented |
| Explicit algorithm allowlist; `alg: none` and algorithm confusion rejected | ENG-172 | Implemented |
| `aud` bound to this specific cellcast instance | ENG-172 | Implemented |
| `exp`, `nbf` and `iat` enforced with a bounded, configured clock skew | ENG-172 | Implemented |
| A token with no `exp` is refused rather than treated as non-expiring | ENG-172 | Implemented |
| JWKS cached, with key rotation handled by refetch on an unknown key id | ENG-172 | Implemented |
| JWKS unavailability never degrades to accepting unverified tokens | ENG-172 | Implemented |
| Token size bounded before any decoding | ENG-172 | Implemented |
| Fuzz target over claim extraction, per provider | ENG-184 | Planned (v0.2) |
| Structured rejection reasons, logged | ENG-172 | Implemented |
| Rejection reasons metered | ENG-178 | Planned (v0.2) |

**What is read before verification, and why.** Selecting a key set requires knowing which issuer
signed the token, and that can only come from the token itself. The pre-verification step is
therefore kept to the smallest thing that makes verification possible: a length bound, a split into
three segments, and a base64 decode of the header's `alg` and the payload's `iss`. Nothing read
there is trusted or retained. The issuer is re-read from the verified payload and compared again
after the signature checks out, so a token whose unsigned header names an allowlisted issuer and
whose signed body names another is rejected.

**Background refresh, stated accurately.** Key rotation is handled by refetching the JWKS when a key
id is not recognised, rate-limited so unknown key ids cannot be turned into load on the issuer. The
periodic loop re-resolves issuer metadata and swaps the key set only if `jwks_uri` has moved; it
does not pre-warm keys, because the underlying key set exposes no refresh hook. The cost is one
extra fetch on the first request after a rotation, not a rejected request.

**Replay, stated plainly.** A valid caller JWT replayed inside its own validity window **will** mint
again. cellcast does not maintain a nonce or `jti` replay cache in v0.1. The exposure is bounded by
the CI issuer's token lifetime (five to ten minutes for the platforms targeted) and by the fact that
capturing the token requires already being on the runner, at which point the attacker can request a
fresh one anyway. Accepted risk, listed below. If this changes, a `jti` cache is the fix and it
belongs in the hub, not the client.

**JWKS cache poisoning.** The JWKS URL is derived from the allowlisted issuer, never from the token.
An attacker cannot point cellcast at a JWKS they control without first getting their issuer onto the
allowlist, which is a configuration change in the hub's chart.

---

### T-03: confused deputy

**What an attacker gets.** A pipeline for repository A obtains a placement, and therefore a
credential, for repository B's cell. Cross-tenant deploy access without ever forging anything: the
attacker uses their own legitimate identity and simply asks for the wrong thing.

**This is the central authorization question in the project.** Authentication (T-02) proves who is
asking. It says nothing about what they may ask for.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| Filter-then-score: permission is evaluated before any scoring | ENG-173 | Planned |
| `PlacementPolicy` maps authenticated claims to a permitted label selector | ENG-173 | Planned |
| Deny by default; no matching policy is a clean rejection, never "any cell" | ENG-173 | Planned |
| Scoring strategy chosen by policy, not by the caller | ENG-173 | Planned |
| e2e test asserting a dev token cannot reach a prod-labelled cell | ENG-177 | Planned |
| `--explain` renders the filter step separately so a rejection is legible | ENG-114 | Planned |

**The subtle failure.** Scoring before filtering produces a working system that passes its happy-path
tests and routes a dev deploy into the least-loaded production cluster the first time production is
quiet. The ordering in
[ADR-005](architecture.md#adr-005-placement-is-filter-then-score-and-policy-is-a-crd) is the control,
and it is enforced by the e2e test above rather than by comments.

---

### T-04: privilege escalation via cluster registration

**What an attacker gets.** Whoever can register a cluster can point cellcast at an endpoint they
control. A malicious `Cluster` entry with attractive labels and low reported utilisation becomes the
target for legitimate deploys, which then apply real manifests, with real credentials, into the
attacker's cluster. That is exfiltration of both workload and token.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| Registration is a `Cluster` CRD write, gated by the hub cluster's own RBAC | ENG-110 | Implemented |
| Creating a `Cluster` is an operator-level action, documented as privileged | ENG-110, ENG-186 | Implemented in code; operator docs pending ENG-186 |
| Registration writes trust configuration only; a payload carrying a static token is rejected | ENG-110 | Implemented |
| Registration is refused outright if it carries a credential, rather than stripping the field | ENG-110 | Implemented |
| The `cellcast.io/` label namespace is reserved, so a registrant cannot forge a label a policy trusts | ENG-110 | Implemented |
| New cells are not scorable until an authenticated agent reports capacity | ENG-111, ENG-174 | Implemented |
| Registration and state transitions are visible in the API server audit log | ENG-110, ENG-112 | Implemented |
| State changes write `spec.state` only, so a stale read cannot revert an endpoint or label | ENG-112 | Implemented |

**Registration does not probe the endpoint.** `POST /api/v1/clusters` validates the endpoint's shape
and stores it. It does not connect to it to check reachability or certificate validity. Probing
would be friendlier, and it would also turn registration into a request-forgery primitive: the hub
would issue outbound connections to a URL the caller chose, from inside the hub cluster's network.
An unreachable endpoint is caught by the first mint attempt against it, which is a failure the
operator sees anyway. If reachability checking is ever added, it belongs behind an explicit
allowlist of destinations, not on the registration path.

**Documented consequence.** `create` on `clusters.cellcast.io` is equivalent to deploy access
across every policy whose selector the new cluster's labels can match. Treat it as an administrative
permission. The chart must not grant it to the same ServiceAccount that runs deploys, and the docs
must say so where an operator will actually read it.

---

### T-05: token exfiltration through CI logs

**What an attacker gets.** A live, scoped credential, harvested from build output that is often
readable by more people than the cluster is. The most likely real-world leak in the entire system,
and it happens by accident rather than by attack.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| The client never prints token material to stdout or stderr | ENG-114 | Planned |
| Default output path is a kubeconfig written to a file with restrictive permissions | ENG-114 | Planned |
| Token never appears in a process argument, where any user on the runner can read it | ENG-114 | Planned |
| GitHub Actions integration registers the value as a mask before use | ENG-185 | Planned (v1.0) |
| Audit records log a hash for correlation, never the token or any prefix of it | ENG-176 | Planned (v0.2) |
| Short TTL resolved from policy limits the value of a leaked token | ENG-113 | Planned |

**Note for reviewers.** The usual way this control fails is a debug log line added later by someone
who did not read this document. A test asserting that no token material appears in any log output is
worth more than the rule itself, and it is part of ENG-176's done-when.

---

### T-06: denial of service

**What an attacker gets.** Placement sits in the deploy critical path. Taking the hub down stops
every deploy in the estate, which is a worse outage than the problem cellcast solves.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| Advisory by default: a placement is a recommendation, not a gate | ENG-175 | Planned |
| Client-side decision cache with TTL, so a short outage is invisible | ENG-175 | Planned |
| Explicitly declared fallback stance per caller, never implicit | ENG-175 | Planned |
| Fail closed on authorization, fail open on optimisation | ENG-175 | Planned |
| Rate limiting per authenticated caller identity | ENG-172 | Planned |
| Unauthenticated requests rejected before expensive work | ENG-172 | Planned |
| Multi-replica stateless API path with a PodDisruptionBudget | ENG-179 | Planned (v0.2) |
| Agent heartbeats jittered to avoid a synchronised herd | ENG-174 | Planned |
| Capacity index bounded, and reports for unregistered cells refused | ENG-111 | Implemented |
| Capacity report payloads size-bounded before decoding | ENG-111 | Implemented |

**Fallback never extends to credentials.** A cached placement is a cached decision. The client
re-mints or fails. A cached credential would reintroduce exactly the long-lived secret this project
exists to remove.

---

### T-07: compromised reporter agent

**What an attacker gets.** The agent runs in every registered spoke, which makes it the largest
attack surface in the system by instance count. A compromised agent can report false capacity and
therefore attract or repel deploys.

**Mitigations.**

| Control | Ticket | Status |
| --- | --- | --- |
| The agent binary does not contain minting code at all | ENG-193 | Planned |
| Agent RBAC limited to `list` and `watch` on nodes and pods | ENG-174 | Planned |
| Agent authenticates with its own projected ServiceAccount token, not a shared secret | ENG-174 | Planned |
| An agent can only report capacity for its own cell | ENG-174 | Planned |
| A malformed or negative report is refused, never clamped into a plausible value | ENG-111 | Implemented |
| A refused report leaves the previous good one in place | ENG-111 | Implemented |
| Staleness measured by the hub's clock, never the agent's | ENG-111 | Implemented |

**Open until ENG-174.** The capacity ingest path authenticates the caller but does not yet bind an
identity to a cell, so any authenticated caller may report for any registered cell. Closing it
requires the shape of the agent's own projected ServiceAccount token, which is ENG-174's to define.
Until then the control above is the registration gate, not the reporter's identity.

**Residual risk.** A compromised agent can still lie about its own cell's utilisation and attract
deploys to a cluster the attacker already controls. That is a strictly smaller win than T-04, because
the attacker must already own a registered production cluster. Not separately mitigated in v0.1.

---

## Accepted risks for v0.1

Listed so nobody has to discover them by reading code.

| Risk | Why accepted | Revisit |
| --- | --- | --- |
| A caller JWT replayed inside its validity window mints twice | Bounded by the issuer's short token lifetime; capture requires runner access, which already permits requesting a fresh token | If an adopter's issuer uses long-lived tokens |
| No audit trail | Deferred to ENG-176 in v0.2; spoke API server audit logs are the interim record | v0.2, before any production adoption |
| Single hub replica | Mitigated by advisory mode rather than by availability | ENG-179, v0.2 |
| Unsigned artifacts | Repo is private and pre-release; nothing is distributed yet | ENG-182, before the ENG-188 public flip |
| A compromised agent can misreport its own capacity | Requires already owning a registered cluster | If capacity attestation becomes worth its complexity |
| Human callers unsupported | Pipelines only in v0.1; a human path is a separate design problem | Post-v1.0 |

## Explicitly not defended against

- A malicious or compromised CI platform OIDC issuer. cellcast trusts allowlisted issuers by design.
- An operator who grants cellcast `cluster-admin` in every spoke. The policy ceiling cannot be
  smaller than the RBAC the operator configured.
- An attacker who already has `create` on `clusters.cellcast.io`. See T-04: that permission is
  administrative and is documented as such.
- Anything inside a managed cluster after a legitimate deploy lands there.

## Reporting a vulnerability

See `SECURITY.md` once the repository is public (ENG-186, ENG-188). Until then, this is a private
repository with a single maintainer.

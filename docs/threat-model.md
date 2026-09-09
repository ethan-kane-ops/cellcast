# Threat model

cellcast decides where a workload deploys and mints the credential that lets it. Both halves are
security-relevant, and the second one means a compromise here is a compromise of every cluster in
the registry, bounded only by policy. This document states what that bound actually is.

**Status of this document.** Written before implementation, as the design contract the code is held
to. Every mitigation below is either implemented or listed as an accepted risk. `Implemented` means
the control is in the code with a test that names this threat. `Planned` means it does not exist
yet.

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
   reason the tool is worth attacking at all.
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
| B6 | Hub into a spoke API server to mint | Bounded by the spoke RBAC granted to the hub's own identity there |

## Threats

### T-01: compromised cellcast hub process

**What an attacker gets.** The ability to mint any credential the operator's policy permits, for as
long as they hold the process. Not a vault to drain: nothing the hub returns to a caller is stored
anywhere, so there is no accumulated stock of credentials to take. The one thing it can reach is its
own way in to each spoke, which is T-08 and which grants the same minting ability by another route
rather than a broader one.

**Honest blast radius.** This is the worst case in the system and it is bad. An attacker with code
execution in the hub can mint into every cell in the registry, subject to two ceilings: the policy
ceiling the operator configured, and cellcast's own RBAC in each spoke. Those ceilings are the only
thing standing between a hub compromise and estate-wide cluster access. Anyone deploying this should
size cellcast's per-spoke RBAC as tightly as their deploys allow, and should not grant it
`cluster-admin` because it was convenient during the pilot.

**Mitigations.**

| Control | Status |
| --- | --- |
| Policy ceiling on TTL that no request can raise | Implemented |
| Hub-wide `--token-max-ttl` no policy can exceed | Implemented |
| Credential lifetime checked against the ceiling on the way back, not only on the way out | Implemented |
| Per-cell RBAC scoping documented and minimal by default | Partial: the boundary is the service account named in the `TrustConfig`. What that account may do in the spoke is the operator's to set |
| Audit record for every mint, so a compromise is reconstructable | Implemented: one record per mint carrying the caller, the cell, the scope and a digest of the token, and one per refusal (docs/audit.md) |
| Distroless non-root image, read-only root filesystem, no shell | Implemented: `gcr.io/distroless/static-debian12:nonroot`, uid 65532, and both charts set the pod and container security contexts |
| Signed images and SBOM so the running binary is the reviewed one | Implemented in the release pipeline: cosign keyless signatures, an SPDX SBOM and SLSA provenance on every image |

**Residual risk.** Detection depends on the audit trail reaching somewhere the hub cannot rewrite.
The records are written to stdout and the adopter's log pipeline owns them from there, so a hub
compromised before its output is shipped can still suppress or forge records about itself. The
spoke clusters' own API server audit logs are the independent record, and they are what a
reconstruction should be reconciled against.

---

### T-02: forged or replayed caller JWT

**What an attacker gets.** If forgery succeeds, everything the impersonated pipeline could get. This
is the primary remote attack and the JWT parser is the most exposed code in the project.

**Mitigations.**

| Control | Status |
| --- | --- |
| Issuer allowlist checked before any attacker-influenced parsing | Implemented |
| Issuer compared literally, never by prefix or suffix | Implemented |
| Full signature verification against the issuer's JWKS | Implemented |
| Explicit algorithm allowlist; `alg: none` and algorithm confusion rejected | Implemented |
| `aud` bound to this specific cellcast instance | Implemented |
| `exp`, `nbf` and `iat` enforced with a bounded, configured clock skew | Implemented |
| A token with no `exp` is refused rather than treated as non-expiring | Implemented |
| JWKS cached, with key rotation handled by refetch on an unknown key id | Implemented |
| JWKS unavailability never degrades to accepting unverified tokens | Implemented |
| Token size bounded before any decoding | Implemented |
| Fuzz targets over every pre-verification parse | Implemented: `peekIssuer`, `bearerToken`, `standardClaims` and claim extraction per provider, in `internal/hub/oidc/fuzz_test.go` |
| Structured rejection reasons, logged | Implemented |
| Rejection reasons metered | Implemented: `cellcast_auth_rejections_total` by reason, with an alert on the ratio |

**What the fuzz targets assert, which is more than "it does not crash".** Claim extraction must
never emit a claim the provider does not declare, and must never flatten a JSON object or array into
a string a `PlacementPolicy` could then match on. `peekIssuer` must never return a usable issuer or
algorithm alongside an error, because both select what happens next. `bearerToken` must never accept
past the size bound, which is what stops an oversized header being used to make the decoder work.
Run them with `just fuzz`.

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
again. cellcast does not maintain a nonce or `jti` replay cache. The exposure is bounded by
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

| Control | Status |
| --- | --- |
| Filter-then-score: permission is evaluated before any scoring | Implemented |
| `PlacementPolicy` maps authenticated claims to a permitted label selector | Implemented |
| Deny by default; no matching policy is a clean rejection, never "any cell" | Implemented |
| Scoring strategy chosen by policy, not by the caller | Implemented |
| e2e test asserting a dev token cannot reach a prod-labelled cell | Implemented: `just verify-e2e`, where the least-loaded cell in the fixture is one the caller may not reach |
| `--explain` renders the filter step separately so a rejection is legible | Implemented |

**The subtle failure.** Scoring before filtering produces a working system that passes its happy-path
tests and routes a dev deploy into the least-loaded production cluster the first time production is
quiet. The ordering in
[ADR-005](architecture.md#adr-005-placement-is-filter-then-score-and-policy-is-a-crd) is the control,
and it is enforced by the e2e test above rather than by comments.

`TestDevPipelineCannotReachAProdCell` pins it at the engine level: the prod cell is the emptiest in
the fleet, so a score-then-filter implementation returns it. The test asserts both the rejection and
the stage it happened at, because being refused late means the cell's capacity was consulted first.
`just verify-e2e` runs the same shape over the full token-to-decision path against a real cluster.

**A second, quieter way this control fails.** A policy constraining `repo` against an authenticator
emitting `repository` matches nothing, and a policy that matches nothing is a deny-all that looks
correct in review. Neither package's tests would catch it alone, so
`TestPolicyClaimKeysMatchWhatTheAuthenticatorProduces` compares the two vocabularies directly, and
the `PlacementPolicy` `Ready` condition reports the live match count so a mislabelled selector is
visible in `kubectl get placementpolicy` before anyone deploys against it.

---

### T-04: privilege escalation via cluster registration

**What an attacker gets.** Whoever can register a cluster can point cellcast at an endpoint they
control. A malicious `Cluster` entry with attractive labels and low reported utilisation becomes the
target for legitimate deploys, which then apply real manifests, with real credentials, into the
attacker's cluster. That is exfiltration of both workload and token.

**Mitigations.**

| Control | Status |
| --- | --- |
| Registration is a `Cluster` CRD write, gated by the hub cluster's own RBAC | Implemented |
| Creating a `Cluster` is an operator-level action, documented as privileged | Implemented in code, and stated under **Documented consequence** below |
| Registration writes trust configuration only; a payload carrying a static token is rejected | Implemented |
| Registration is refused outright if it carries a credential, rather than stripping the field | Implemented |
| The `cellcast.io/` label namespace is reserved, so a registrant cannot forge a label a policy trusts | Implemented |
| New cells are not scorable until an authenticated agent reports capacity | Implemented |
| Registration and state transitions are visible in the API server audit log | Implemented |
| State changes write `spec.state` only, so a stale read cannot revert an endpoint or label | Implemented |

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

| Control | Status |
| --- | --- |
| The client never prints token material to stdout or stderr | Implemented |
| Default output path is a kubeconfig written to a file with restrictive permissions | Implemented: 0600, created with O_EXCL so an existing symlink is never followed |
| Token never appears in a process argument, where any user on the runner can read it | Implemented: there is no `--token` flag, and a test asserts there never is one |
| `--json` output carries the decision with the token stripped | Implemented |
| GitHub Actions integration registers the value as a mask before use | Planned: the purpose-built integrations are not shipped |
| Audit records log a hash for correlation, never the token or any prefix of it | Implemented: `audit.Record` has no field that can hold token material, and a test classifies every field so a new one fails until somebody has thought about it |
| Short TTL resolved from policy limits the value of a leaked token | Implemented |
| The credential type redacts its own token under `%v`, `String()` and `slog` | Implemented |

**Note for reviewers.** The usual way this control fails is a debug log line added later by someone
who did not read this document, so there are two tests rather than a rule: one at the HTTP boundary
against a sentinel token, and one in `TestLiveEndToEnd` against a token a real API server issued.
The second is the stronger, because a sentinel only proves the handler does not copy a string it was
handed.

The type system carries the rest. `broker.Credential` implements `String` and `slog.LogValue`, so
printing one, wrapping it in an error, or logging it with `slog.Any` emits `Token:[redacted]`. There
is no unsafe log line for a reviewer to spot.

---

### T-06: denial of service

**What an attacker gets.** Placement sits in the deploy critical path. Taking the hub down stops
every deploy in the estate, which is a worse outage than the problem cellcast solves.

**Mitigations.**

| Control | Status |
| --- | --- |
| Advisory by default: a placement is a recommendation, not a gate | Implemented |
| Client-side decision cache with TTL, so a short outage is invisible | Implemented: `--on-unavailable last-known`, bounded by `--cache-ttl` |
| Explicitly declared fallback stance per caller, never implicit | Implemented |
| Fail closed on authorization, fail open on optimisation | Implemented: no stance answers an authorization refusal |
| Unauthenticated requests rejected before expensive work | Implemented: authentication is middleware over the whole API surface, and rejections are counted before a handler runs |
| Multi-replica stateless API path with a PodDisruptionBudget | Implemented: see [ADR-011](architecture.md#adr-011-the-api-path-runs-n-replicas-and-only-the-controllers-elect) |
| Agent heartbeats jittered to avoid a synchronised herd | Implemented: full jitter on the first heartbeat, plus or minus 10% after that |
| Rate limiting per authenticated caller identity | Planned |
| Capacity index bounded, and reports for unregistered cells refused | Implemented |
| Capacity report payloads size-bounded before decoding | Implemented |
| Fuzz targets over the placement and registration request bodies | Implemented: both handlers, asserting the status set, that every response is JSON, and that a refusal carries a reason the client contract defines |

**Fallback never extends to credentials.** A cached placement is a cached decision. The client
re-mints or fails. A cached credential would reintroduce exactly the long-lived secret this project
exists to remove.

---

### T-07: compromised reporter agent

**What an attacker gets.** The agent runs in every registered spoke, which makes it the largest
attack surface in the system by instance count. A compromised agent can report false capacity and
therefore attract or repel deploys.

**Mitigations.**

| Control | Status |
| --- | --- |
| The agent binary does not contain minting code at all, enforced by a build-graph test | Implemented |
| Agent RBAC limited to `list` and `watch` on nodes and pods, plus one named Lease | Implemented |
| Agent authenticates with its own projected ServiceAccount token, not a shared secret | Implemented |
| An agent can only report capacity for the cell whose registration names it | Implemented |
| A cell that names no reporter accepts no reports at all | Implemented |
| A malformed or negative report is refused, never clamped into a plausible value | Implemented |
| A refused report leaves the previous good one in place | Implemented |
| Staleness measured by the hub's clock, never the agent's | Implemented |
| Fuzz target over the capacity arithmetic an agent drives | Implemented: whatever `Validate` accepts scores as a real number in range, so a crafted report can never produce a NaN that makes every comparison in the scorer false and hands the placement to whichever cell was first |

**How the binding works.** `Cluster.spec.reporter` names an `issuer` and a `subject`, both compared
literally against the authenticated caller. The agent presents a projected ServiceAccount token with
cellcast's audience, verified by the same OIDC path as a CI caller's token, so there is one
authentication path in the hub rather than two.

**The issuer is the half that does the work.** Every cell runs the agent under the same
ServiceAccount, so the `sub` claim is byte-identical across the fleet:
`system:serviceaccount:cellcast-system:cellcast-agent`. Binding on the subject alone would let the
development cell's agent report for the production one, which is the attack this control exists to
stop.

**That imposes a real prerequisite.** Each spoke must have a distinct service account issuer URL.
EKS, GKE and AKS each give a cluster its own, so an adopter on a managed platform has nothing to do.
A stock kubeadm or kind cluster issues as `https://kubernetes.default.svc.cluster.local` and every
one of them collides. The signature check is what stops that silently degrading into a shared
identity: two clusters claiming one issuer URL do not share a key set, so the hub can only ever hold
one of them in its allowlist and the second cell's agent fails verification outright. The failure is
a rejected agent, not an accepted impostor, but it is a failure, and the fix is configuring
`--service-account-issuer` per cluster. See docs/architecture.md ADR-009.

**Fail-closed on an unset field.** A `Cluster` with no `spec.reporter` refuses every report rather
than accepting any. The cell then stays `Unknown` and drops out of scoring, which is exactly where
the staleness guard puts a cell whose agent has died: a state the hub detects and an operator can
see. Treating the absent field as a wildcard is what left this open before.

**Residual risk.** A compromised agent can still lie about its own cell's utilisation and attract
deploys to a cluster the attacker already controls. That is a strictly smaller win than T-04, because
the attacker must already own a registered production cluster. Not separately mitigated.

---

### T-08: the hub's own credential for reaching a spoke

**What this is.** To mint through a cell's `TokenRequest` API, the hub has to authenticate to that
cell. This is the hub's own identity in the spoke, and it is a different thing from the credential
the caller receives. Conflating the two is the fastest way to misread ADR-004: what cellcast never
stores is the credential it hands out. It necessarily holds some way to reach each spoke, exactly as
any multi-cluster control plane does.

**What an attacker gets.** Whatever that identity can do in the spoke. Because that identity's job
is to mint tokens for a named service account, an attacker who takes it can mint those tokens, which
is the same win as T-01 by a different route rather than a new one.

**Mitigations.**

| Control | Status |
| --- | --- |
| `inCluster` configuration stores nothing at all, and is what the demo uses | Implemented |
| Credential Secrets are read uncached, straight from the API server, at the moment of use | Implemented |
| The hub never holds a resident map of spoke credentials, and needs no `watch` on Secrets | Implemented |
| Client construction is per mint with no pooling, so nothing retains the material | Implemented |
| A parse failure on a kubeconfig never puts its contents in an error or a log | Implemented |
| The spoke identity is scoped to `create` on `serviceaccounts/token` for named accounts | Operator-configured: cellcast's charts install the hub and the agent, not the RBAC the hub holds in another cluster |

**Residual risk.** A `secretRef` configuration is a stored credential, and calling it anything else
would be dishonest. It is bounded by the RBAC the operator grants it in the spoke, and the intended
shape of that grant is the ability to mint for specific service accounts and nothing else, so it is
not an administrative credential. The configuration that stores nothing is `inCluster`, and it only
covers cells in the hub's own cluster. Replacing `secretRef` with ServiceAccount token federation,
where the spoke trusts the hub cluster's issuer and no material is stored for the multi-cluster case
either, is listed as an accepted risk below.

---

## Accepted risks

Listed so nobody has to discover them by reading code.

| Risk | Why accepted | Revisit |
| --- | --- | --- |
| A caller JWT replayed inside its validity window mints twice | Bounded by the issuer's short token lifetime; capture requires runner access, which already permits requesting a fresh token | If an adopter's issuer uses long-lived tokens |
| The metrics endpoint is unauthenticated | It exposes cell names, policy names, per-cell utilisation and refusal counts, all of which an authenticated caller can already read through `GET /api/v1/clusters`. Reaching the port is a network decision rather than an identity one | The chart ships a NetworkPolicy and a ServiceMonitor. Revisit if a caller exists that is not operator-controlled |
| The audit trail is only as durable as the log pipeline collecting it | cellcast writes to stdout and owns no sink, so retention and immutability are the adopter's existing decisions rather than a second set of ours to get wrong | If an adopter needs a tamper-evident trail, which is a shipping problem rather than a cellcast one |
| No published artifacts to verify | The release pipeline signs everything it publishes and verifies its own signatures, but has never run: keyless signing has to carry the release workflow's identity, which needs the repository public | At the first tagged release |
| A compromised agent can misreport its own capacity | Requires already owning a registered cluster | If capacity attestation becomes worth its complexity |
| Human callers unsupported | Pipelines only; a human path is a separate design problem | Post-v1.0 |
| Any authenticated caller can enumerate the fleet | `GET /api/v1/clusters` is unfiltered and `--explain` names cells the caller may not reach. Cell names and endpoints are not secrets, and withholding them from `--explain` alone would hide the answer to "why did my deploy land there" without withholding anything a caller could not already list | When a caller exists that is not operator-controlled; filtering both by policy is the fix, not filtering one |
| A cell outside the hub's cluster needs a stored kubeconfig for the hub to mint through | Bounded by the spoke RBAC granted to it, which is minting rights rather than administrative access; `inCluster` stores nothing and covers the demo | When ServiceAccount token federation to the spoke replaces it (T-08) |

## Explicitly not defended against

- A malicious or compromised CI platform OIDC issuer. cellcast trusts allowlisted issuers by design.
- An operator who grants cellcast `cluster-admin` in every spoke. The policy ceiling cannot be
  smaller than the RBAC the operator configured.
- An attacker who already has `create` on `clusters.cellcast.io`. See T-04: that permission is
  administrative and is documented as such.
- Anything inside a managed cluster after a legitimate deploy lands there.

## Reporting a vulnerability

Privately, through a GitHub Security Advisory. The route, the response targets and what a useful
report contains are in
[SECURITY.md](https://github.com/ethan-kane-ops/cellcast/blob/main/SECURITY.md).

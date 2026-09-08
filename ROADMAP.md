# Roadmap

What is built, what is next, and what has been decided against. Dates are
deliberately absent: this is a side project and a date would be fiction.

## Shipped

**v0.1 — the decision path.** A hub that authenticates a pipeline by OIDC
workload identity federation, filters the fleet by a deny-by-default
`PlacementPolicy`, scores the survivors on capacity that agents report, and
mints a short-lived scoped credential for the cell it chose. A client CLI with a
declared fallback stance, an in-cluster reporter agent, and an envtest harness
covering the CRD schema and the controllers.

**v0.2 — production posture.** An audit trail that records every placement and
every mint. A Prometheus surface with a dashboard and alerting rules tied to the
code by a contract test. Multi-replica HA with a warmup-gated readiness probe
and a drain that survives a rolling update. Fuzz targets on every parser that
reads bytes somebody else chose. Helm charts for the hub and the agent.

## Next

**Release and supply chain.** Multi-arch images and OCI charts to GHCR, the
client binary through goreleaser and a Homebrew tap, cosign keyless signatures
and SBOMs on everything published, and vulnerability scanning in CI.

**Pipeline integrations.** The pitch is that cellcast slots into an existing
pipeline rather than owning deploys. That deserves a working GitHub Action, an
Argo CD pre-sync hook, and a Buildkite plugin, not a claim in a README.

**A docs site and a recorded demo.** Three cells with skewed load, a placement
that explains its own scoring table, and a credential that expires while you
watch.

## Under consideration

**More trust providers.** The broker interface exists and Kubernetes
`TokenRequest` is the only implementation. AWS STS `AssumeRoleWithWebIdentity`
is the obvious second, and it is a real one rather than a variation: the TTL
floor, the failure modes and the audit fields all differ.

**Cost and locality as scoring inputs.** Least-loaded is one strategy among
several. Ranking on spot price or on distance from the caller is the same engine
with a different score function, and the value is in the data, not the code.

**A read-only web view of the fleet.** The audit trail already answers "who
deployed where and why". A page rendering it would be useful and is not
obviously cellcast's job rather than Grafana's.

## Decided against

**Owning the deploy.** cellcast answers where and hands back a credential. It
does not template, apply, roll back, or watch. Every multi-cluster tool that
grew a deploy engine had to grow an opinion about Helm, Kustomize and Argo with
it, and the pitch here is precisely that you keep the one you have.

**Being authoritative.** A placement is a recommendation. The client declares up
front what happens when there is no answer, and the default is to fail. A hard
dependency in the deploy path would make cellcast's own availability a ceiling
on every deploy in the estate.

**Durable capacity.** Capacity is high-write and worthless once stale, so it
lives in memory and is rebuilt from heartbeats. A database for it would add an
operational component to hold numbers that are wrong within a minute.

**A cellcast-managed cluster registry service.** The registry is CRDs in the hub
cluster. `kubectl` is the interface, the API server is the store, and its audit
log is already the record of who changed what.

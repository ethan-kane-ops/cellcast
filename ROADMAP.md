# Roadmap

What is built, what is next, and what has been decided against. There are no dates: this is a side
project and a date would be fiction.

## Shipped

**v0.1, the decision path.** A hub that authenticates a pipeline by OIDC workload identity
federation, filters the fleet by a deny-by-default `PlacementPolicy`, scores the survivors on
capacity that agents report, and mints a short-lived scoped credential for the cell it chose. A
client CLI with a declared fallback stance, an in-cluster reporter agent, and an envtest harness
covering the CRD schema and the controllers.

**v0.2, production posture.** An audit trail recording every placement and every mint. A Prometheus
surface with a dashboard and alerting rules tied to the code by a contract test. Multi-replica HA
with a warmup-gated readiness probe and a drain that survives a rolling update. Fuzz targets on
every parser that reads untrusted input. Helm charts for the hub and the agent.

**The release and supply chain pipeline.** Multi-arch images and OCI charts for GHCR, the client
through goreleaser, cosign keyless signatures and SBOMs on everything published, SLSA build
provenance on every image, and `govulncheck` over both the source and the built binaries. Built and
rehearsed end to end with `just release-check`; nothing is published until the repository is public
and the release runs from a workflow rather than a laptop.

## Next

**Publishing the first release.** The pipeline exists and has never run for real. The first tag
needs the repository public, so that keyless signatures carry the release workflow's identity
rather than a personal one.

**Pipeline integrations.** A GitHub Action, an Argo CD pre-sync hook and a Buildkite plugin.
cellcast attaches to an existing pipeline, and the integrations are what make that true in practice
rather than in a README.

**A recorded demo.** Three cells with skewed load, a placement that explains its own scoring table,
and a credential expiring in real time.

## Under consideration

**More trust providers.** The broker interface exists and Kubernetes `TokenRequest` is the only
implementation. AWS STS `AssumeRoleWithWebIdentity` is the obvious second. It is not a variation on
the first: the TTL floor, the failure modes and the audit fields all differ.

**Cost and locality as scoring inputs.** Least-loaded is one strategy among several. Ranking on
spot price or on distance from the caller is the same engine with a different score function, and
the work is in sourcing the data.

**A read-only web view of the fleet.** The audit trail already answers who deployed where and why.
Rendering it as a page would be useful, and Grafana may be the better place for it.

## Decided against

**Owning the deploy.** cellcast answers where and hands back a credential. It does not template,
apply, roll back, or watch. Every multi-cluster tool that grew a deploy engine had to grow an
opinion about Helm, Kustomize and Argo with it.

**Being authoritative.** A placement is a recommendation. The client declares up front what happens
when there is no answer, and the default is to fail. A hard dependency in the deploy path would
make cellcast's own availability a ceiling on every deploy in the estate.

**Durable capacity.** Capacity is high-write and worthless once stale, so it lives in memory and is
rebuilt from heartbeats. A database for it would add an operational component to hold numbers that
are wrong within a minute.

**A cellcast-managed cluster registry service.** The registry is CRDs in the hub cluster. `kubectl`
is the interface, the API server is the store, and its audit log already records who changed what.

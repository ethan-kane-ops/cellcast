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
rehearsed end to end with `just release-check`, and run for real from the release workflow so that
the keyless signatures carry that workflow's identity rather than a personal one.

**Pipeline integrations.** A composite GitHub Action, an Argo CD PreSync hook and a Buildkite
pipeline step, each with the policy it needs. An integration workflow deploys a sample app to one of
three real cells through the action, with no secret configured anywhere in it, so the claim that
cellcast attaches to an existing pipeline is checked on every push rather than asserted in a README.

**A recorded demo.** Three cells with skewed load, a placement that explains its own scoring table,
and a credential bounded by what it can do. The emptiest cell in the fleet is the one policy refuses,
which is the argument the recording exists to make.

## Next

Shipping the launch milestone made the imbalance obvious: the engine is further along than the
product around it. The client has two commands, `place` and `version`, and everything an operator
does between installing the hub and running a pipeline is hand-written YAML.

**A policy you can test without deploying.** Placement is deny by default, which is right for a
broker, and a refusal reaches the caller as a reason and nothing else. Finding out why a policy did
not match currently means reading the hub's logs, which an adopter does not have. Asking the hub
"would this token be admitted, and if not, which selector missed" should be one command.

**One command to enrol a cell.** Registering a cell is three objects, and one of them needs the
reporter's issuer and subject written exactly right. Getting it wrong is silent: the cell registers,
reports capacity that is attributed to nothing, and never wins a placement.

**Keeping a workload where it already is.** Nothing in the engine knows where a workload ran last
time, so two deploys minutes apart can land in different cells as utilisation shifts, splitting a
service across two cells with nobody having decided that. A placement should prefer the cell a
workload is already in, and say so in the explain table when it does not.

**OpenTelemetry.** The metrics surface is Prometheus and the placement path has no tracing at all.
OTLP covers both and reaches a Datadog agent without linking a vendor SDK into a binary that mints
credentials.

**A second trust provider.** The broker interface exists and Kubernetes `TokenRequest` is its only
implementation, so nothing has tested whether it is an interface or a description of that one case.
AWS STS `AssumeRoleWithWebIdentity` is the obvious second and it is not a variation on the first: the
TTL floor, the ceiling, the failure modes and the audit fields all differ. It should also settle
whether the client writing a kubeconfig is a decision taken on behalf of providers that do not exist
yet.

## Under consideration

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

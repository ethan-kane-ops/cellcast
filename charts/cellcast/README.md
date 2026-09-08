# cellcast

Multi-cluster deployment placement oracle and short-lived credential broker

![Version: 0.2.0](https://img.shields.io/badge/Version-0.2.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.2.0](https://img.shields.io/badge/AppVersion-0.2.0-informational?style=flat-square)

The hub is the only cellcast component that can mint credentials. It holds trust
configuration, never credentials: see
[docs/threat-model.md](https://github.com/ethan-kane-ops/cellcast/blob/main/docs/threat-model.md)
for what a compromise of this process does and does not grant.

## Install

```console
helm install cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast \
  --namespace cellcast-system --create-namespace \
  --set hub.oidc.issuers[0].url=https://token.actions.githubusercontent.com \
  --set hub.oidc.issuers[0].provider=github
```

The issuer is not optional in practice. Without one the hub authenticates
nobody and refuses every API request, which is the correct state for a broker
that cannot tell who is asking, and is not a working install.

## Things worth knowing before you install

**The CRDs are in `templates/`, not `crds/`.** Helm installs a `crds/`
directory once and never upgrades it, which means a schema change would need a
manual `kubectl apply` that nobody remembers. These are templated instead, so
`helm upgrade` carries them, and `crds.install=false` hands them to whatever
manages CRDs out of band. They carry `helm.sh/resource-policy: keep`, so
`helm uninstall` leaves the registry behind rather than deleting every
`Cluster` in it.

**`terminationGracePeriodSeconds` is checked, not merely defaulted.** A
terminating hub reports itself unready, keeps serving for `hub.drainDelay` so
endpoint removal can propagate, and only then drains within
`hub.shutdownTimeout`. Those run in sequence, so the grace period has to exceed
their sum or the kubelet interrupts the drain. The chart refuses to render if it
does not.

**`updateStrategy.rollingUpdate.maxUnavailable` is `0` on purpose.** A new
replica starts with an empty capacity index and refuses placements until the
fleet has reported to it. Surging before terminating is what keeps a rollout
from being a window in which deploys fail.

**Metrics are unauthenticated.** The endpoint discloses cell names, policy names
and refusal counts, which an authenticated caller could already list through the
API. Reaching it should be a NetworkPolicy decision; `networkPolicy.enabled`
is where that lives.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| replicaCount | int | `3` | Number of hub replicas. Every replica answers placements; only the controllers take a lease, so this scales the API path linearly. |
| image.registry | string | `"ghcr.io"` | Container image registry. |
| image.repository | string | `"ethan-kane-ops/cellcast-hub"` | Image repository, without the registry. |
| image.tag | string | `""` | Image tag. Defaults to the chart's appVersion. |
| image.digest | string | `""` | Image digest. Takes precedence over `tag` when set. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| imagePullSecrets | list | `[]` | Image pull secrets for a private registry. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| fullnameOverride | string | `""` | Override the full resource name prefix. |
| hub.logLevel | string | `"info"` | Log level: debug, info, warn or error. The audit trail is written at a fixed level and is never suppressed by this. |
| hub.logFormat | string | `"json"` | Log format: json or text. |
| hub.registryNamespace | string | `""` | Namespace holding the Cluster, PlacementPolicy and TrustConfig registry. Empty means the release namespace, which is what a single-tenant install wants. Set it only when the hub reads a registry it is not deployed beside. |
| hub.tokenMaxTTL | string | `"1h"` | Absolute ceiling on minted credential lifetime. No policy and no request may exceed it. The Kubernetes TokenRequest API applies no maximum of its own unless the operator configured one, so this is the bound that is certain to exist. |
| hub.capacity.staleness | string | `"90s"` | How long an agent report stays usable. Past it the cell is Unknown and excluded from scoring rather than read as empty. |
| hub.capacity.retention | string | `"1h"` | How long an Unknown entry is kept before being dropped, which is what keeps "went quiet" distinguishable from "was never here". |
| hub.capacity.maxCells | int | `1000` | Bound on the in-memory capacity index. |
| hub.warmupTimeout | string | `"90s"` | How long a starting replica waits for the fleet to report before it goes ready anyway. Agents heartbeat through the Service and a Service routes only to ready pods, so this wait cannot be unbounded. |
| hub.drainDelay | string | `"5s"` | How long a terminating replica keeps serving while already reporting unready, so endpoint removal can propagate before the listener closes. |
| hub.shutdownTimeout | string | `"20s"` | Bound on draining in-flight requests, measured from the end of drainDelay. |
| hub.readHeaderTimeout | string | `"10s"` | Maximum time a client may take to send request headers. |
| hub.leaderElection.enabled | bool | `true` | Elect a leader for the reconciler path. The API path never elects. |
| hub.leaderElection.namespace | string | `""` | Namespace holding the lease. Empty means the release namespace. |
| hub.oidc.audience | string | `"cellcast"` | Audience callers must request, identifying this cellcast instance. It is also the audience the agent's projected token must carry. |
| hub.oidc.issuers | list | `[]` | Trusted issuers. Until at least one is set the hub authenticates nobody and every API request is refused, which is the correct state for a broker that cannot tell who is asking. Each entry is `{url, provider}`, for example: `- url: https://token.actions.githubusercontent.com` `  provider: github` |
| hub.oidc.clockSkew | string | `"60s"` | Tolerance applied to token exp, nbf and iat. |
| hub.oidc.refreshInterval | string | `"1h"` | How often issuer metadata is re-resolved. |
| hub.oidc.httpTimeout | string | `"10s"` | Timeout for issuer discovery and JWKS fetches. |
| hub.extraArgs | list | `[]` | Extra arguments appended to the hub command. Anything settable by flag can be set here without waiting for the chart to grow a value for it. |
| ports.api | int | `8080` | Port the placement and registry API listens on. |
| ports.probe | int | `8081` | Port health and readiness probes listen on. Separate from the API so probes work when the API is not exposed. |
| ports.metrics | int | `8082` | Port the Prometheus endpoint listens on. Set to 0 to disable it. |
| crds.install | bool | `true` | Install the CRDs with the chart. Set false where CRDs are managed out of band, which many shops require. |
| crds.keep | bool | `true` | Annotate the CRDs so `helm uninstall` leaves them, and the registry with them. Deleting a CRD deletes every Cluster and PlacementPolicy in it. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount for the hub. |
| serviceAccount.name | string | `""` | Name of the ServiceAccount. Generated from the release when empty. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount, which is where a cloud provider's workload identity binding goes. |
| rbac.create | bool | `true` | Create the Role and RoleBinding the hub needs. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Port the Service exposes the API on. |
| service.annotations | object | `{}` | Annotations for the Service. |
| service.labels | object | `{}` | Extra labels for the Service. |
| podDisruptionBudget.enabled | bool | `true` | Create a PodDisruptionBudget. A hub in the deploy critical path should not be fully evictable by a node drain. |
| podDisruptionBudget.maxUnavailable | int | `1` | Pods that may be unavailable during a voluntary disruption. Expressed as maxUnavailable rather than minAvailable so that a single-replica development install can still be drained. |
| podDisruptionBudget.minAvailable | string | `""` | Minimum available pods. Mutually exclusive with maxUnavailable. |
| updateStrategy.type | string | `"RollingUpdate"` |  |
| updateStrategy.rollingUpdate.maxUnavailable | int | `0` | Zero, deliberately. A new replica has to be ready, which means warm, before an old one goes away, or a rollout is a window in which placements are refused. |
| updateStrategy.rollingUpdate.maxSurge | int | `1` |  |
| terminationGracePeriodSeconds | int | `40` | Grace period for a terminating pod. It must exceed drainDelay plus shutdownTimeout, or the kubelet's SIGKILL lands mid-drain. The chart refuses to render if it does not. |
| resources.requests.cpu | string | `"50m"` |  |
| resources.requests.memory | string | `"128Mi"` |  |
| resources.limits.memory | string | `"512Mi"` |  |
| topologySpreadConstraints | list | `[{"labelSelector":{"matchLabels":{}},"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway"}]` | Spread replicas across nodes. ScheduleAnyway rather than DoNotSchedule so a single-node development cluster still runs, while a real one spreads. |
| affinity | object | `{}` | Pod affinity rules. Left empty because topologySpreadConstraints above already spreads across nodes; set this for zone rules or co-scheduling. |
| nodeSelector | object | `{}` | Node selector for hub pods. |
| tolerations | list | `[]` | Tolerations for hub pods. |
| priorityClassName | string | `""` | PriorityClass for hub pods. A service in the deploy critical path is worth naming here. |
| podAnnotations | object | `{}` | Extra annotations for hub pods. |
| podLabels | object | `{}` | Extra labels for hub pods. |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `65532` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor. Requires the Prometheus Operator CRDs. |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| metrics.serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout. |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels, for a Prometheus that selects ServiceMonitors by label. |
| metrics.serviceMonitor.metricRelabelings | list | `[]` | Metric relabelings. |
| metrics.serviceMonitor.relabelings | list | `[]` | Relabelings. |
| metrics.prometheusRule.enabled | bool | `false` | Install the shipped alerting rules. Requires the Prometheus Operator CRDs. |
| metrics.prometheusRule.labels | object | `{}` | Extra labels, for a Prometheus that selects PrometheusRules by label. |
| networkPolicy.enabled | bool | `false` | Restrict who may reach the hub. Off by default because a policy that names the wrong callers is an outage, and the right callers differ per estate. |
| networkPolicy.allowedIngress | list | `[]` | Selectors allowed to reach the API port. Ingress from agents and from whatever runs the client belongs here. |
| networkPolicy.metricsAllowedIngress | list | `[]` | Selectors allowed to reach the metrics port. |
| extraEnv | list | `[]` | Extra environment variables for the hub container. |
| extraVolumes | list | `[]` | Extra volumes for the hub pod. |
| extraVolumeMounts | list | `[]` | Extra volume mounts for the hub container. |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| ethan-kane-ops |  | <https://github.com/ethan-kane-ops> |

# cellcast-agent

In-cluster capacity reporter for a cellcast cell

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.1.0](https://img.shields.io/badge/AppVersion-v0.1.0-informational?style=flat-square)

This chart installs into a cell, not into the hub cluster. Capacity is the only
thing it publishes, and it holds no credential for the hub or for any other
cell.

## Install

```console
helm install cellcast-agent oci://ghcr.io/ethan-kane-ops/charts/cellcast-agent \
  --namespace cellcast-system --create-namespace \
  --set cellName=prod-euw1 \
  --set hub.endpoint=https://cellcast.example.com
```

`cellName` must match the `Cluster` resource this cell is registered as. The hub
accepts capacity for a cell only from the identity that registration names, so a
wrong name here is a rejected report rather than a misattributed one.

## The identity

The agent authenticates with a projected ServiceAccount token carrying
cellcast's own audience, not the default token mounted at
`/var/run/secrets/kubernetes.io/serviceaccount`. The default one is minted for
the API server, and a hub that accepted it would be accepting a credential
issued to a different audience.

The hub matches on both claims in the cell's `spec.reporter`:

```yaml
spec:
  reporter:
    issuer: https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE
    subject: system:serviceaccount:cellcast-system:cellcast-agent
```

The issuer is the load-bearing half. Every spoke runs this chart under the same
ServiceAccount name, so the subject is byte-identical across the fleet, and
binding on the subject alone would let the development cell's agent report
capacity for the production one. Read the issuer this cluster uses with:

```console
kubectl get --raw /.well-known/openid-configuration | grep issuer
```

A stock kubeadm or kind cluster issues as
`https://kubernetes.default.svc.cluster.local` and every one of them collides,
so those clusters need their own service account issuer URL before they can be
told apart.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| cellName | string | `""` | Name of the Cluster resource in the hub's registry that this cell is registered as. The hub accepts capacity for a cell only from the identity that cell's registration names, so a wrong name here is a rejected report rather than a misattributed one. |
| hub.endpoint | string | `""` | URL of the cellcast hub, for example `https://cellcast.example.com` or the in-cluster Service when the hub runs alongside this cell. |
| hub.audience | string | `"cellcast"` | Audience the projected ServiceAccount token is minted for. It must match the hub's `--oidc-audience`, and it must NOT be the API server's audience: a hub that accepted the default token would be accepting a credential minted for somebody else. |
| hub.tokenExpirationSeconds | int | `3600` | Lifetime of the projected token. The kubelet rewrites the file at 80% of this and the agent re-reads it on every heartbeat, so nothing depends on a token living as long as the pod. |
| hub.caConfigMap | string | `""` | ConfigMap holding a PEM bundle that verifies the hub's certificate. Leave empty to use the system roots. |
| hub.caConfigMapKey | string | `"ca.crt"` | Key inside that ConfigMap. |
| replicaCount | int | `2` | Number of agent replicas. One reports; the rest hold the lease's runner-up position and take over without waiting for a reschedule. |
| image.registry | string | `"ghcr.io"` | Container image registry. |
| image.repository | string | `"ethan-kane-ops/cellcast-agent"` | Image repository, without the registry. |
| image.tag | string | `""` | Image tag. Defaults to the chart's appVersion. |
| image.digest | string | `""` | Image digest. Takes precedence over `tag` when set. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| imagePullSecrets | list | `[]` | Image pull secrets for a private registry. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| fullnameOverride | string | `""` | Override the full resource name prefix. |
| agent.heartbeatInterval | string | `"30s"` | How often capacity is published. The hub's staleness window is three of these by default, so a single dropped report never takes a healthy cell out of scoring. |
| agent.requestTimeout | string | `"10s"` | Timeout for one report. Must be shorter than heartbeatInterval, or a hung hub leaves the previous attempt running when the next tick fires. |
| agent.maxBackoff | string | `"2m"` | Ceiling on the retry interval while the hub is unreachable. Deliberately short: a cell that is not reporting is a cell that is not being placed on. |
| agent.leaderElection | bool | `true` | Report from one replica at a time. |
| agent.logLevel | string | `"info"` | Log level: debug, info, warn or error. |
| agent.logFormat | string | `"json"` | Log format: json or text. |
| agent.extraArgs | list | `[]` | Extra arguments appended to the agent command. |
| ports.probe | int | `8081` | Port health and readiness probes listen on. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount for the agent. |
| serviceAccount.name | string | `""` | Name of the ServiceAccount. Generated from the release when empty. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| rbac.create | bool | `true` | Create the ClusterRole, Role and their bindings. |
| podDisruptionBudget.enabled | bool | `true` | Create a PodDisruptionBudget. A cell whose agent is fully evicted stops reporting, goes Unknown at the hub, and drops out of scoring. |
| podDisruptionBudget.maxUnavailable | int | `1` | Pods that may be unavailable during a voluntary disruption. |
| podDisruptionBudget.minAvailable | string | `""` | Minimum available pods. Mutually exclusive with maxUnavailable. |
| resources.requests.cpu | string | `"20m"` |  |
| resources.requests.memory | string | `"64Mi"` |  |
| resources.limits.memory | string | `"256Mi"` |  |
| topologySpreadConstraints | list | `[{"maxSkew":1,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway"}]` | Spread replicas across nodes, softly, so a single-node cell still runs. |
| affinity | object | `{}` | Pod affinity rules. |
| nodeSelector | object | `{}` | Node selector for agent pods. |
| tolerations | list | `[]` | Tolerations for agent pods. A reporter that cannot be scheduled on a tainted node still sees it, because capacity is read from the API server rather than from the node it runs on. |
| priorityClassName | string | `""` | PriorityClass for agent pods. |
| podAnnotations | object | `{}` | Extra annotations for agent pods. |
| podLabels | object | `{}` | Extra labels for agent pods. |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `65532` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| extraEnv | list | `[]` | Extra environment variables for the agent container. |
| extraVolumes | list | `[]` | Extra volumes for the agent pod. |
| extraVolumeMounts | list | `[]` | Extra volume mounts for the agent container. |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| ethan-kane-ops |  | <https://github.com/ethan-kane-ops> |

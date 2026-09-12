# Observability

Four views of the same events, with one instrumentation point behind them.

| View | What it is for | Where |
|---|---|---|
| **Audit trail** | What happened, per request, forensically | stdout, JSON |
| **Metrics** | Rates and trends, alerting | Prometheus, or OTLP |
| **Events** | "who has been deploying here", per cell | `kubectl describe cluster` |
| **Traces** | Where the time in one request went | OTLP, once configured |

## One record, three views

Every placement and every mint produces one `audit.Record`. The JSON trail, the
Kubernetes Events, the Prometheus counters and the attributes on a trace's spans
are all views over it.

A metric incremented at its own call site drifts from the log line next to it
the first time somebody adds an early return, and the drift is invisible: both
look fine, they just describe different things. Deriving all three from one
record means a refusal that is audited is also counted.

The consequence for contributors: **new hub instrumentation goes on the audit
record**, not on a new call site.

## The audit trail

One JSON line per placement and per mint, tied together by `request_id`:

```json
{"time":"2026-09-08T12:31:50Z","level":"INFO","msg":"audit",
 "event":"placement","outcome":"granted","request_id":"03c2fb846fa6f746",
 "issuer":"https://token.actions.githubusercontent.com",
 "subject":"repo:acme/checkout:ref:refs/heads/main",
 "workload":"checkout-api","cell":"prod-euw1","policy":"app-prod",
 "strategy":"LeastLoaded","confidence":"high",
 "candidates":[{"cell":"dev-euw1","admitted":false,"stage":"permission",
                "reason":"cell is not permitted by this caller's policy"}]}
```

Three properties of the trail:

- **It is written through its own handler at a fixed level.** `--log-level=error`
  suppresses the hub's chatter and never the trail. Sharing one logger would
  make the audit record a debugging convenience rather than a record.
- **It cannot carry token material.** A reflection test enumerates every field
  on the record and fails on any new one that has not been classified. Mints
  record `token_sha256`, a digest for correlation.
- **A refusal records what was considered.** The candidate table travels on the
  refusal, so "why was my deploy refused" is answerable from the log rather than
  only from a client that happened to pass `--explain`.

Worked queries are in the [audit trail reference](audit.md).

## Metrics

The full surface, the cardinality argument, and the four metrics whose
definitions are not obvious are in the [metrics reference](metrics.md). The
short version:

```promql
# Is cellcast adding time to every deploy?
histogram_quantile(0.99, sum by (le) (rate(cellcast_placement_duration_seconds_bucket[5m])))

# What share of placements is being refused, and why?
sum by (reason) (rate(cellcast_placement_requests_total{outcome="refused"}[5m]))

# Can every replica actually place?
min(cellcast_hub_warm)

# Which cells has the hub stopped hearing from?
cellcast_capacity_staleness_seconds > ignoring(cell) group_left() (2 * cellcast_capacity_staleness_window_seconds)
```

A dashboard ships in `dashboards/cellcast.json` and five alerting rules in
`config/prometheus/prometheusrule.yaml`. Both are templated by the hub chart:

```bash
helm upgrade cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast --reuse-values \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.prometheusRule.enabled=true
```

A contract test asserts the dashboard, the alerts and the documentation agree
with what the registry actually emits, **in both directions**: a metric nobody
plots fails the build, and so does a dashboard panel querying a metric that no
longer exists.

!!! warning "The metrics endpoint is unauthenticated"

    It discloses cell names, policy names and refusal counts, which an
    authenticated caller could already list through the API. It carries no
    caller identity, no workload name and no credential. Reaching it should
    still be a NetworkPolicy decision: `networkPolicy.enabled` in the chart.

## Kubernetes Events

The audit trail's second view, hung on the `Cluster` each record concerns, so
the question "who has been deploying here" is answerable with `kubectl`:

```console
$ kubectl -n cellcast-system describe cluster prod-euw1
...
Events:
  Type    Reason             Age    From           Message
  ----    ------             ----   ----           -------
  Normal  Placed             2m     cellcast-hub   placed checkout-api for repo:acme/checkout:ref:refs/heads/main under policy app-prod
  Normal  CredentialIssued   2m     cellcast-hub   issued a 15m0s credential to repo:acme/checkout:ref:refs/heads/main, scoped to apps/deployer
```

Events are lossy by design: rate-limited, and they expire. They are a
convenience on top of the trail rather than a substitute for it. Dry runs
produce no event, because a dry run is not a deploy.

Only the leader emits them, which is why they appear once rather than once per
replica.

## OTLP export

The hub exports traces and metrics over OTLP, and the agent exports traces, to
any collector: an OpenTelemetry Collector, Grafana Alloy or a Datadog agent.
Nothing is exported until an endpoint is set.

```bash
helm upgrade cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast --reuse-values \
  --set observability.otlp.endpoint=http://otel-collector.observability:4318
```

| Flag | Chart value | Default |
|---|---|---|
| `--otlp-endpoint` | `observability.otlp.endpoint` | none, so nothing is exported |
| `--otlp-protocol` | `observability.otlp.protocol` | `http/protobuf` |
| `--trace-sample-ratio` | `observability.otlp.traceSampleRatio` | `1` |

The endpoint is a base URL, as `OTEL_EXPORTER_OTLP_ENDPOINT` is: traces go to
`/v1/traces` under it and metrics to `/v1/metrics`. Its scheme decides TLS. A
backend that needs an API key gets it from a Secret named in
`observability.otlp.headersSecret`, which reaches the process as
`OTEL_EXPORTER_OTLP_HEADERS` and never as an argument.

The standard environment variables work with none of the above:
`OTEL_EXPORTER_OTLP_ENDPOINT` and its per-signal forms, `_PROTOCOL`, `_HEADERS`,
`_CERTIFICATE`, `_COMPRESSION` and `_TIMEOUT`, plus `OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`, `OTEL_METRIC_EXPORT_INTERVAL`,
`OTEL_TRACES_EXPORTER=none`, `OTEL_METRICS_EXPORTER=none` and
`OTEL_SDK_DISABLED`. A flag wins over its variable. A collector that is down is
logged at most once a minute and never stops the hub.

### Traces

One placement is one trace, under the caller's own span when the request
carries a `traceparent` header:

```text
POST /api/v1/placement
├── authenticate          issuer discovery and key fetches
├── place
│   ├── select policy
│   ├── filter
│   └── score
└── mint                  the TokenRequest round trip to the spoke
```

Each heartbeat is one trace too: `POST /api/v1/clusters/{name}/capacity` in the
agent, with the hub's span for the report under it. The mint sends its trace
context on to the spoke, so a spoke API server with tracing enabled joins the
trace. A trace that arrives sampled stays sampled whatever the ratio says.

**Spans carry the audit record and nothing else.** Apart from the method, route,
status code and request id, every attribute is an audit field under the name
the trail uses, prefixed `cellcast.`: `cellcast.subject` on a span is `subject`
in the audit line. Four fields stay off spans, because a trace backend is
usually read by more people than the audit trail: the claims, the candidate
table, `token_sha256` and the internal `error`. `cellcast.request_id` leads from
a span to the full record, and the request log line carries `trace_id` for the
way back.

The classification that decides this is the one that keeps token material out
of the trail, and the build fails if any file other than the tracing files puts
anything on a span.

### Metrics

The OTLP metrics are the Prometheus registry, bridged as it stands: the names,
labels and buckets a scrape of the metrics endpoint reports, so the dashboard,
the alerts and the [metrics reference](metrics.md) apply to either path. That
includes the controller-runtime and Go runtime metrics the endpoint also serves;
drop them in the collector if the backend bills per series.

### Datadog

Point OTLP at the Datadog agent rather than linking a vendor SDK into the binary
that mints credentials. With the Datadog agent's OTLP receiver enabled on every
node, in the Datadog chart's values:

```yaml
datadog:
  otlp:
    receiver:
      protocols:
        http:
          enabled: true
```

then point cellcast at the agent on its own node:

```bash
helm upgrade cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast --reuse-values \
  --set 'observability.otlp.endpoint=http://$(HOST_IP):4318'
```

`HOST_IP` is set from the node's address whenever an endpoint is. Tag the
service through `extraEnv`, for example
`OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=prod`. If the Datadog
agent already scrapes the metrics endpoint, set `OTEL_METRICS_EXPORTER=none` to
keep one copy.

## What to alert on

The five shipped rules, and what each catches that a dashboard panel would not:

| Alert | Catches |
|---|---|
| `CellcastCapacityStale` | A cell whose agent stopped reporting, so deploys silently concentrate elsewhere |
| `CellcastCellNeverReported` | A registered `LIVE` cell that has never reported, so it can never be placed on |
| `CellcastReplicaCannotPlace` | A replica in the Service that still has no capacity for the fleet |
| `CellcastAuthRejectionRatioHigh` | A broken pipeline, or somebody probing |
| `CellcastNoAuthenticator` | A hub refusing everything because no OIDC issuer is configured, while passing its own health checks |

The last is the misconfiguration that stops every deploy in the estate while
looking healthy from the outside.

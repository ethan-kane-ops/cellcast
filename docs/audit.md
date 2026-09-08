# Audit trail

Every placement decision and every issued credential is recorded. This document says what a record
holds, where it goes, and how to answer the questions a security review actually asks from it.

## Where records go

Structured JSON on the hub's stdout, alongside the rest of its output. There is no bespoke sink, no
database and no retention policy of cellcast's own: records land in whatever log pipeline the
adopter already runs, and the retention, access control and immutability decisions stay where they
already are.

Every record carries `"msg":"audit"`. That is the field to select on, in preference to matching on
prose that is free to change.

```bash
kubectl -n cellcast-system logs deploy/cellcast-hub | jq -c 'select(.msg == "audit")'
```

The trail is **not** levelled by `--log-level`. A hub started with `--log-level=error` still emits
every record, because a security log that a verbosity flag can silence is not a security log.

## The two records

One request produces up to two records, tied together by `request_id`, which is also the
`X-Request-Id` the caller was handed and the id on the hub's ordinary request log line.

| `event` | Written when | `outcome` |
| --- | --- | --- |
| `placement` | the hub decided which cell the caller may reach, or refused to | `granted`, `dry-run`, `refused` |
| `mint` | the hub issued a credential for that cell, or failed to | `granted`, `refused` |

They are separate because either can happen without the other. A dry run places and never mints. A
mint can fail against a cell that placement legitimately chose, and that is a fault in the cell
rather than in the request.

A refusal is recorded with the same fields as a success, including the candidate table showing every
registered cell and which filter stage rejected it. A trail that only holds what worked is not a
trail.

## What a record holds

```json
{
  "time": "2026-09-08T09:14:22.181Z",
  "level": "INFO",
  "msg": "audit",
  "event": "mint",
  "outcome": "granted",
  "request_id": "8f2c1d4ab90e5577",
  "issuer": "https://token.actions.githubusercontent.com",
  "subject": "repo:acme/checkout:ref:refs/heads/main",
  "claims": { "repository": "acme/checkout", "ref": "refs/heads/main" },
  "workload": "checkout-api",
  "requested_ttl": "20m0s",
  "cell": "prod-eu-1",
  "policy": "acme-prod",
  "strategy": "LeastLoaded",
  "confidence": "high",
  "namespace": "apps",
  "service_account": "deployer",
  "granted_ttl": "15m0s",
  "expires_at": "2026-09-08T09:29:22Z",
  "token_sha256": "sha256:1d1cb1a1...c0ffee"
}
```

`namespace` and `service_account` are the blast radius of the token this record describes: they are
what the credential can act as, and the only reason the record is worth keeping after the credential
has expired.

`confidence` says how much of the permitted fleet the hub could see when it decided. `degraded`
means at least one permitted cell was excluded because its capacity was stale, so the chosen cell is
the best of what the hub could see, which is not the same claim as the best there is.

## Token material

**No record holds a token, or any prefix of one.** `token_sha256` is a SHA-256 digest of the whole
token, which is enough to match a token found in a build log against the record that issued it and
is not enough to use one.

To identify the record that issued a leaked token:

```bash
printf %s "$LEAKED_TOKEN" | shasum -a 256 | awk '{print "sha256:" $1}'
# then find it
kubectl -n cellcast-system logs deploy/cellcast-hub \
  | jq -c --arg h "sha256:..." 'select(.token_sha256 == $h)'
```

A prefix of the token would correlate just as well and is forbidden. A JWT's leading bytes are its
header and the segment boundaries move, so "just a prefix" is not a fixed amount of the secret. See
[the threat model](threat-model.md), T-05.

`internal/hub/audit` enforces this structurally rather than by convention. The record type has no
field that can carry credential material, and a test classifies every field and fails on any new one
until somebody has thought about it.

## Worked example: which pipeline deployed to prod-eu-1 last Tuesday

Everything below comes out of the log alone. No cluster access, no cellcast API call.

Assume the pipeline collected the hub's stdout into `hub.log`, one JSON object per line.

**Every credential issued for that cell, that day:**

```bash
jq -c 'select(.msg == "audit" and .event == "mint" and .outcome == "granted"
              and .cell == "prod-eu-1"
              and (.time | startswith("2026-09-01")))' hub.log
```

**Reduced to who, what and when:**

```bash
jq -r 'select(.msg == "audit" and .event == "mint" and .outcome == "granted"
              and .cell == "prod-eu-1"
              and (.time | startswith("2026-09-01")))
       | [.time, .claims.repository // .subject, .workload, .service_account, .granted_ttl]
       | @tsv' hub.log
```

```
2026-09-01T08:41:03.774Z  acme/checkout   checkout-api   deployer   15m0s
2026-09-01T11:02:55.118Z  acme/checkout   checkout-api   deployer   15m0s
2026-09-01T16:20:41.902Z  acme/billing    ledger-api     deployer   10m0s
```

That is the answer: two repositories deployed to `prod-eu-1` on the 1st, three times between them,
each holding a credential scoped to the `deployer` service account in `apps` for fifteen minutes or
less.

**What was refused, which is the half the question usually forgets:**

```bash
jq -r 'select(.msg == "audit" and .outcome == "refused")
       | [.time, .subject, .workload, .event, .reason] | @tsv' hub.log
```

```
2026-09-01T09:15:12.006Z  repo:acme/experiments:ref:refs/heads/spike  spike-api  placement  NoPolicy
2026-09-01T14:47:30.551Z  repo:acme/billing:ref:refs/heads/main       ledger-api placement  NoEligibleCells
```

The first is an authorization refusal: no `PlacementPolicy` matched that caller, so the hub denied by
default. The second is a fleet condition: the caller was permitted and every cell it may reach was
draining or stale. The candidate table on that record names each one and the stage that refused it.

**One request end to end**, when a pipeline reports a failure and quotes its `X-Request-Id`:

```bash
jq -c 'select(.request_id == "8f2c1d4ab90e5577")' hub.log
```

That returns the placement record, the mint record and the ordinary request log line, in order.

## The `kubectl describe` view

The same events are also written as Kubernetes Events on the `Cluster` they concern, so an operator
already looking at a cell can see who has been deploying there without leaving the terminal.

```
$ kubectl -n cellcast-system describe cluster prod-eu-1
...
Events:
  Type     Reason            Age    From           Message
  ----     ------            ----   ----           -------
  Normal   Placed            12m    cellcast-hub   placed checkout-api for repo:acme/checkout:ref:refs/heads/main under policy acme-prod
  Normal   CredentialIssued  12m    cellcast-hub   issued a 15m0s credential to repo:acme/checkout:ref:refs/heads/main, scoped to apps/deployer
  Warning  MintFailed        4m     cellcast-hub   could not mint a credential for repo:acme/billing:ref:refs/heads/main: MintFailed
```

**This view is deliberately lossy and the JSON trail is the authoritative one.** Kubernetes Events
are aggregated, spam-filtered and expire on the cluster's own schedule, so a busy hub will have some
of these collapsed or dropped. Making them complete would mean an API server write on every deploy
in the estate, in the critical path. Use them to orient; answer the question from the log.

Dry runs produce no Event. A refused placement produces none either, because it never reached a cell
to hang one on; it is in the JSON trail like everything else.

# Security Policy

cellcast issues credentials for production clusters. A vulnerability here is not
a bug in a tool that watches a cluster; it is a way to obtain access to one. The
policy below is written accordingly.

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security-related reports.

Report vulnerabilities privately via the GitHub Security Advisory page for this repository:

https://github.com/ethan-kane-ops/cellcast/security/advisories/new

When submitting a report, please include:

- **Affected component**: hub, agent, client, or chart. The hub is the only one
  that can mint, so a finding there is more serious by default.
- **Description**: what the vulnerability is and its impact, stated as what an
  attacker ends up holding.
- **Reproduction**: step-by-step instructions, including the `Cluster`,
  `PlacementPolicy` and `TrustConfig` manifests, and the claims of the token
  presented. **Redact token material**; the claims are what matters.
- **Mitigation**: any temporary workaround you have identified.

### Response targets

| Stage | Target |
|---|---|
| Acknowledge receipt | 48 hours |
| Triage and confirm severity | 7 days |
| Fix released for critical severity | 14 days |

Reporters will be credited in the release notes for the fix unless they request anonymity.

## What is in scope

The documented trust boundaries are in [docs/threat-model.md](./docs/threat-model.md),
and anything that crosses one is in scope. In particular:

- **Authentication bypass.** Any way to have the hub accept a token it should
  have rejected: a forged or replayed assertion, an issuer or audience that is
  not the configured one, a signature the hub does not actually verify.
- **Policy bypass.** Any way to be placed on, or minted for, a cell that no
  matching `PlacementPolicy` permits. This includes reaching a `DARK` cell
  without a policy that allows it, and any path where a client-supplied value
  changes which policy applies.
- **TTL bypass.** Any minted credential outliving `--token-max-ttl`, or a
  policy's own ceiling.
- **Credential disclosure.** Token material reaching a log line, an audit
  record, a metric label, a Kubernetes Event, or an error returned to a caller.
  The audit record has a test asserting no field can hold it; a way around that
  is a finding.
- **Confused deputy in capacity ingest.** Any way for one cell's agent to
  publish capacity for a different cell, which steers real deploys.
- **Privilege escalation through the charts.** RBAC in either chart that grants
  more than the component needs.

## What is not in scope

- An operator granting a policy more than they meant to. cellcast enforces the
  policy that is written, and `--dry-run --explain` shows what a policy does
  before it is relied on.
- A compromised hub cluster. Whoever controls the hub's ServiceAccount can read
  the trust configuration it holds and mint what that configuration allows;
  that is the hub's whole job, and the threat model says so.
- Denial of service by a caller who is already authenticated and permitted.
- A cell whose service account issuer URL collides with another cell's, on a
  cluster that was configured that way. The hub binds a reporter identity to an
  issuer, so distinct issuers are a prerequisite the docs state plainly.

## Supported Versions

Only the latest minor release line receives security updates.

| Version | Supported |
|---|---|
| v0.2.x | Yes |
| < v0.2 | No |

Pre-1.0, the API and the CRD schema may change between minor versions.

## Signed releases

Published images and OCI charts are signed with [cosign](https://docs.sigstore.dev/)
keyless signing, with no long-lived keys. The verification command lives in the
README under **Verifying what you install**.

Verifying is worth the two minutes here more than on most projects: the image
you are about to run is the one that will hold your clusters' trust
configuration.

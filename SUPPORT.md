# Support

Where to go, by what is needed.

## Documentation

- Project docs: https://ethan-kane-ops.github.io/cellcast/
- README: [README.md](./README.md)
- Chart values: [hub](./charts/cellcast/README.md), [agent](./charts/cellcast-agent/README.md)

## Questions and usage help

Open a [GitHub issue](https://github.com/ethan-kane-ops/cellcast/issues/new/choose)
using the question or feature-request template. Please search existing issues first.

## Bugs

File a bug with the bug-report template at
https://github.com/ethan-kane-ops/cellcast/issues/new/choose.

Two things make a bug report actionable, and both come from the hub rather than
from the client:

- The **audit record** for the request, which is a single JSON line on the hub's
  stdout carrying the request id, the policy that matched, and every cell that
  was considered. See [docs/audit.md](./docs/audit.md) for how to find it.
- The `Cluster` and `PlacementPolicy` that were in play, as YAML.

**Redact before pasting.** An audit record cannot contain token material, but a
`kubectl get -o yaml` of anything else in the namespace can.

## Security vulnerabilities

Do **not** open a public issue for a security problem. Report it privately via a
[GitHub Security Advisory](https://github.com/ethan-kane-ops/cellcast/security/advisories/new).
See [SECURITY.md](./SECURITY.md) for the disclosure policy.

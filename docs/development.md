# Development

## Setup

```bash
mise install     # pinned Go, helm, helm-docs, golangci-lint, just
just             # list every recipe
just build       # all three binaries into bin/, with version stamping
just check       # the gate: tidy, verify-generate, lint, chart-lint, test
```

`just check` must pass before every commit. `just check-all` adds the race
detector and a real kube-apiserver.

## Layout

```
api/v1alpha1/          Cluster, PlacementPolicy and TrustConfig types + generated deepcopy
cmd/
  cellcast/            client CLI; must not link controller-runtime
  cellcast-hub/        API + controllers; the ONLY binary that can mint
  cellcast-agent/      in-cluster capacity reporter, runs in every cell
internal/
  cli/                 client command tree
  hub/                 config, HTTP server, middleware, auth seam, manager wiring
  hub/audit/           the single description of what the hub did
  hub/broker/          the minting path
  hub/capacity/        the in-memory capacity index
  hub/metrics/         Prometheus views over the audit record
  hub/oidc/            caller authentication
  hub/placement/       the filter-then-score engine
  agent/               reporter config and loop
  boundaries/          architectural tests, not behaviour tests
charts/                the hub and agent Helm charts
config/crd/bases/      GENERATED CRD manifests, never hand-edit
dashboards/            Grafana JSON, tied to the code by a contract test
```

**The three-binary split is a security boundary, not tidiness.** The agent runs
in every registered cell and must never link the minting code. `internal/boundaries`
enforces this by inspecting what each binary actually links, so a change that
merges the binaries, or moves minting into a package the agent imports, fails
the build.

## Code generation

`api/v1alpha1` is the source of truth. Generated: `zz_generated.deepcopy.go`,
everything under `config/crd/bases/`, and the copies the chart installs.

```bash
just generate manifests
```

`just verify-generate` runs inside `just check` and fails on a stale diff.

The chart holds copies of the CRDs and the alerting rules because Helm's
`.Files` cannot read outside a chart directory. Anything else generated that the
chart installs has to join that copy step in `just manifests`, or it will drift.

## Testing

```bash
just test        # unit tests
just cover       # statement coverage, fails below the gate
just envtest     # CRD schema and controllers against a real kube-apiserver
just fuzz        # every fuzz target, 30s each; `just fuzz 10m` for a real campaign
just check-all   # check + race + envtest
```

### Tests that are not behaviour tests

Three groups exist to hold design decisions in place, and they are worth knowing
about before you trip one:

- **`internal/boundaries`** asserts the binary split and that the charts pass
  only flags the binaries accept.
- **The metrics contract test** asserts the dashboard, the alerting rules and
  `docs/metrics.md` agree with what the registry emits, in both directions.
  Adding a metric without plotting it fails the build; so does a panel querying
  a metric that no longer exists.
- **`TestRecordHasNoFieldThatCouldHoldAToken`** enumerates every field on the
  audit record by reflection and fails on any new one that has not been
  classified. Classifying it is the point.

None of these are obstacles to route around. Failing one means the change needs
a decision, not a workaround.

### Verify by breaking

The bar is that a test fails when the behaviour it names breaks, and the only
way to know is to try:

1. **Commit first.** The revert is `git checkout HEAD --`, which destroys
   uncommitted work.
2. Change the code so the behaviour is wrong.
3. Run the test. Confirm it fails, and that the failure names a real test rather
   than a build error.
4. Revert **every** directory the change touched, not just the obvious one.

A test that cannot fail is worse than the gap it appeared to fill, because it
also stops anyone looking there again.

### Assert the property, not the text

`strings.Contains(output, "alert: Foo")` passes on an alert renamed to `FooBar`.
That is not hypothetical; it happened here and the break-test caught it. Compare
whole values.

## Local end-to-end

```bash
just verify-e2e       # place a workload, then kill the hub and check every fallback stance
just verify-agent     # three real cells with real agents; watch one drop out of scoring
just verify-mint      # mint a real credential and check what it can and cannot do
```

Each builds its own kind clusters and tears them down. They are not part of
`just check` because each one takes minutes and needs Docker.

Every kind cluster is created with its own service account issuer, because the
hub tells one cell's agent from another by the `iss` claim, and every stock kind
cluster issues as the same URL.

## Charts

```bash
just chart-lint       # lint and render both, with every optional block on
just chart-docs       # regenerate the values tables with helm-docs
just chart-show       # print what would be installed
```

The values tables in each chart's README are generated from the comments in
`values.yaml`. Edit the comment, not the table.

## Docs site

```bash
just docs-serve       # live reload at http://127.0.0.1:8000
just docs-build       # static build into ./site, --strict
```

`--strict` turns a broken internal link into a build failure, which is the only
reason link rot ever gets fixed.

## CI

There is none yet, on purpose. `just check` plus pre-commit is the verification
layer until the repository is public. Do not add a workflow before then; satisfy
the same code path locally and say so on the ticket.

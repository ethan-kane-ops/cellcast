# Contributing to cellcast

Thank you for considering a contribution. This document describes the development workflow, the coding standards, and what a reviewable pull request looks like here.

By participating, you agree to abide by the [Code of Conduct](./CODE_OF_CONDUCT.md).

---

## Development setup

The project uses [mise](https://mise.jdx.dev/) to manage language runtimes and tooling, and [just](https://just.systems/) as a task runner.

1. Install **mise**: https://mise.jdx.dev/installing-mise.html
2. Activate it in your shell: https://mise.jdx.dev/getting-started.html#activate-mise
3. Provision the pinned tools (Go, helm, helm-docs, golangci-lint, just):
   ```bash
   mise install
   ```
4. Install **pre-commit** (https://pre-commit.com/#install) and enable the hooks:
   ```bash
   just hooks
   ```

Then:

```bash
just              # list every recipe
just build        # all three binaries into bin/
just check        # the gate: tidy, verify-generate, lint, chart-lint, test
```

`just check` must pass before every commit. `just check-all` adds the race detector and a real kube-apiserver.

## The shape of the project

Three binaries, deployed in two places:

| Binary | Runs in | Job |
|---|---|---|
| `cellcast-hub` | the hub cluster | API, controllers, the only component that can mint |
| `cellcast-agent` | every registered cell | reports capacity on a heartbeat |
| `cellcast` | the pipeline runner | client CLI |

**The split is a security boundary, not tidiness.** The agent runs in every registered cell, so it must never link the minting code. A test in `internal/boundaries` enforces this by inspecting what each binary actually links, and a change that merges the binaries or moves minting into a shared package the agent imports will fail it. That test is not the obstacle; it is the design.

Read [docs/architecture.md](./docs/architecture.md) before changing anything structural, and [docs/threat-model.md](./docs/threat-model.md) before touching auth, policy, or the minting path. They are the design contract rather than a description written afterwards.

## Code generation

`api/v1alpha1` is the source of truth. `zz_generated.deepcopy.go`, everything under `config/crd/bases/`, and the chart's copies are generated:

```bash
just generate manifests
```

`just verify-generate` runs inside `just check` and fails on a stale diff, so generated output cannot drift from the types.

## Conventions

- Error strings: lowercase, no trailing punctuation, wrapped with `%w`.
- Tests: table-driven, `t.TempDir()` for filesystem fixtures.
- Structured logging via `log/slog` to stdout.
- **Never log token material**, not even a prefix. Log a hash if you need correlation. The request logger deliberately omits headers, bodies and query strings.
- **New hub instrumentation goes on the audit record, not on a new call site.** `internal/hub/audit` is the single description of what the hub did; Events and metrics are views over it. Adding a field to `audit.Record` requires classifying it in `TestRecordHasNoFieldThatCouldHoldAToken`, which is deliberate.
- **Adding a metric means updating `dashboards/cellcast.json` or `config/prometheus/prometheusrule.yaml`, and `docs/metrics.md`.** A contract test asserts the three agree with what the registry emits, in both directions.
- Comments explain why, not what. A comment restating the line below it is noise; a comment saying which failure the line prevents is the reason the line survives a refactor.

## Tests

The bar is that a test fails when the behaviour it names breaks. Two habits get you there:

- **Verify by breaking.** Change the code so the behaviour is wrong, run the test, and confirm it fails for the stated reason. Commit first: the revert is `git checkout HEAD --`, which destroys uncommitted work.
- **Assert the property, not the text.** `strings.Contains(output, "alert: Foo")` passes on an alert renamed to `FooBar`. Compare whole values.

Coverage has a floor (`just cover`), but a test that raises coverage without being able to fail is worse than the gap it filled.

## Pull requests

- One logical change per PR. A refactor and a behaviour change in the same diff cannot be reviewed.
- Conventional commit format for the title: `type(scope): summary`.
- The PR body describes the diff and the problem it solves. Not the journey, not the branch, not what a reviewer should do next.
- `just check` passes.
- If the change touches the minting path, the policy engine, or the authenticator, say in the PR what an attacker gains if you got it wrong. If the answer is "nothing", say why.

## Security

Do not open a public issue or PR for a vulnerability. See [SECURITY.md](./SECURITY.md).

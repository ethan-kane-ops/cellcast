# Pipeline integrations

cellcast attaches to the pipeline you already have. It answers where a workload
should go and hands back a short-lived credential for that cell; your existing
deploy step does the deploying.

Every integration here works the same way. The pipeline presents the identity
token its platform already issues, the hub verifies it against that platform's
public keys, policy decides which cells this caller may reach, and the survivors
are ranked on capacity. No secret is configured anywhere, and revoking a
pipeline's access is an edit to a `PlacementPolicy` rather than a hunt for which
repositories hold which credential.

| | What it is | Files |
|---|---|---|
| [GitHub Actions](#github-actions) | A composite action, three lines in a caller's workflow | [`github-actions/`](github-actions/) |
| [Argo CD](#argo-cd) | A PreSync hook that stops a sync into the wrong cell | [`argocd/`](argocd/) |
| [Buildkite](#buildkite) | A pipeline step using the agent's built-in OIDC token | [`buildkite/`](buildkite/) |

Each needs a `PlacementPolicy` naming the caller. The policy beside each example
is the half people forget: without one, the hub authenticates the caller and
then refuses it, because there is no implicit "any cell".

## GitHub Actions

```yaml
permissions:
  contents: read
  id-token: write          # without this there is no token to present

steps:
  - id: cellcast
    uses: ethan-kane-ops/cellcast/.github/actions/place@v0.4.0
    with:
      hub: https://cellcast.example.com
      workload: checkout-api
      ttl: 15m

  - run: kubectl -n apps apply -f deploy/checkout-api.yaml
```

`KUBECONFIG` is already set for the second step, and points at a credential for
the cell cellcast chose that expires in fifteen minutes. The chosen cell is also
available as `steps.cellcast.outputs.cell`.

The full workflow is in [`github-actions/workflow.yml`](github-actions/workflow.yml)
and the policy it needs in [`github-actions/policy.yaml`](github-actions/policy.yaml).
Pin the actions to commit SHAs in a repository that cares; the example uses tags
so it stays readable.

The one thing worth checking before writing that policy is the subject format.
Every repository created after 15 July 2026 gets an immutable subject carrying
numeric owner and repository IDs, and a policy written in the other form
authenticates the caller and then refuses it with `NoPolicy`:

```bash
gh api repos/OWNER/REPO/actions/oidc/customization/sub --jq .sub_claim_prefix
```

### What the action does

1. Downloads the client for the runner, checks it against the published
   `checksums.txt`, and verifies that file's keyless signature when `cosign` is
   already installed.
2. Requests an identity token for the audience the hub expects, masks it, and
   writes it to a file readable only by the job. It is removed when the step
   ends and is never passed as an argument.
3. Runs the placement once and publishes `cell`, `source` and `kubeconfig` as
   outputs.

`source` is the one to branch on. Anything other than `hub` means the hub did
not answer and no credential was minted, so a deploy step that runs anyway will
fail on a missing kubeconfig rather than on anything cellcast said.

### Inputs

| Input | Default | |
|---|---|---|
| `hub` | required | Base URL of the hub |
| `workload` | required | What is being deployed; policy and the cache are keyed on it |
| `audience` | `cellcast` | Must match the hub's `--oidc-audience` |
| `ttl` | | Requested lifetime, for example `15m`. Policy may grant less |
| `kubeconfig` | under `RUNNER_TEMP` | Outside the workspace, so an artifact upload cannot collect it |
| `on-unavailable` | `fail` | `fail`, `last-known`, or a cell name to pin |
| `dark` | `false` | Target a dark cell, if policy permits it |
| `dry-run` | `false` | Run the whole decision without minting |
| `explain` | `false` | Log every candidate cell and why it was or was not chosen |
| `export-kubeconfig` | `true` | Set `KUBECONFIG` for the rest of the job |
| `version` | `v0.2.0` | Release of the client to install |
| `client-path` | | Use this binary instead of downloading one |

## Argo CD

Argo keeps the deploy. The hook asks cellcast one question before the sync
starts, and the answer is its exit code: is this cell still somewhere this
workload may go. A cell moved to `DRAINING`, or dropped from the policy, fails
the hook and the sync does not run.

```bash
kubectl apply -f argocd/presync-job.yaml
```

That works as a gate because of the policy beside it: the identity the Job
carries is permitted exactly one cell, so "where should this go" and "may this
go here" are the same question. Moving the workload somewhere else is then a
change to the Application's destination, reviewed in a pull request.

Each cell needs its own copy of both files, because the caller is a pod in that
cell and every cell has a different service account issuer.

## Buildkite

```yaml
steps:
  - label: "deploy checkout-api"
    env:
      CELLCAST_HUB: https://cellcast.example.com
    commands:
      - export TOKEN_FILE="$$(mktemp)"
      - trap 'rm -f "$$TOKEN_FILE"' EXIT
      - buildkite-agent oidc request-token --audience cellcast --lifetime 300 > "$$TOKEN_FILE"
      - cellcast place --workload checkout-api --ttl 15m --token-file "$$TOKEN_FILE"
      - kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy/checkout-api.yaml
```

`$$` matters. Buildkite interpolates `$VAR` when the pipeline is uploaded and
`$$VAR` when the step runs, so a single `$` here resolves on the wrong machine.

The policy matches on claims rather than on the subject, because Buildkite's
`sub` carries the commit and is therefore different on every build. See
[`buildkite/policy.yaml`](buildkite/policy.yaml).

## The hub has to trust the issuer

A policy naming an issuer does not add it. The hub verifies signatures only
against the issuers it was started with, and the provider name after the `=`
selects which claims are extracted and so what a policy can match on:

```bash
--oidc-issuer https://token.actions.githubusercontent.com=github
--oidc-issuer https://agent.buildkite.com=buildkite
--oidc-issuer https://oidc.euw1.example.com=generic
```

A self-hosted issuer presenting a certificate from an internal CA needs
`--oidc-ca-file` as well, or the hub cannot fetch its keys and so cannot
authenticate anyone it issues for.

## Checked, not asserted

[`.github/workflows/integration.yml`](../../.github/workflows/integration.yml)
builds three real cells with three real agents, starts a hub that trusts
GitHub's issuer, and deploys [`github-actions/app.yaml`](github-actions/app.yaml)
through the action with no secret configured anywhere in the job. It then checks
the app reached the cell cellcast named and neither of the other two.

The policy that run applies is `github-actions/policy.yaml`, unedited, so an
example that stops working stops the build.

## Related

- [Pipeline integration](../../docs/pipeline-integration.md) for using the
  client directly, on a platform with no integration here.
- [Placement policy](../../docs/placement-policy.md) for the full field
  reference.
- [Extending](../../docs/extending.md) for adding a CI platform's claims.

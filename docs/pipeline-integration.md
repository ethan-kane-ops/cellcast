# Pipeline integration

cellcast attaches to an existing pipeline. It answers where and hands back a
credential; the deploy step already in place does the deploying.

The client needs two things: the hub's URL, and the identity token the CI
platform already issues. There is no secret to configure and nothing to rotate.

!!! tip "There is an action, a hook and a pipeline step"

    A GitHub Action, an Argo CD PreSync hook and a Buildkite pipeline step are
    in
    [examples/integrations](https://github.com/ethan-kane-ops/cellcast/tree/main/examples/integrations),
    each with the policy it needs. Everything below is the same thing done by
    hand, which is what a platform with no integration of its own needs.

## The shape, in any pipeline

```bash
export CELLCAST_HUB=https://cellcast.example.com
export CELLCAST_TOKEN="$(...)"        # the CI platform's OIDC token

cellcast place --workload checkout-api --ttl 15m
kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

`cellcast place` writes a kubeconfig at `0600`. The token is never printed and
never passed as a command argument, because on a shared runner both are visible
to every other job on the machine. There is no `--token` flag.

## GitHub Actions

The action does all of this. Three lines, and `KUBECONFIG` is set for the step
after it:

```yaml
jobs:
  deploy:
    runs-on: ubuntu-latest
    permissions:
      id-token: write     # required for the OIDC token
      contents: read
    steps:
      - id: cellcast
        uses: ethan-kane-ops/cellcast/.github/actions/place@v0.4.0
        with:
          hub: https://cellcast.example.com
          workload: checkout-api
          ttl: 15m

      - run: kubectl -n apps apply -f deploy/checkout-api.yaml
```

It publishes the chosen cell as `steps.cellcast.outputs.cell`, and `source`,
which is what to branch on: anything other than `hub` means the hub did not
answer and no credential was minted. The inputs are listed in
[examples/integrations](https://github.com/ethan-kane-ops/cellcast/tree/main/examples/integrations).

The same thing without it, on a runner that already has the client:

```yaml
jobs:
  deploy:
    runs-on: ubuntu-latest
    permissions:
      id-token: write     # required for the OIDC token
      contents: read
    steps:
      - uses: actions/checkout@v4

      - name: Get a cellcast placement
        env:
          CELLCAST_HUB: https://cellcast.example.com
        run: |
          CELLCAST_TOKEN=$(curl -sf \
            -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=cellcast" | jq -r .value)
          export CELLCAST_TOKEN
          cellcast place --workload checkout-api --ttl 15m

      - name: Deploy
        run: kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

The `audience` must match the hub's `--oidc-audience`. The matching policy:

```yaml
spec:
  subjects:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:acme/checkout:ref:refs/heads/main
```

That `subject` is what pins deploys to one branch of one repository. An
issuer-only selector would permit every repository on GitHub.

!!! warning "The subject format depends on when the repository was created"

    Every repository created after 15 July 2026 gets an immutable subject
    carrying numeric owner and repository IDs, so the value above is
    `repo:acme@123456/checkout@789012:ref:refs/heads/main` rather than the form
    shown. Older repositories keep the form shown unless they opt in.

    A policy written in the wrong one of the two authenticates the caller and
    then refuses it with `NoPolicy`, which reads like a hub problem and is not.
    Ask for the prefix rather than assuming it:

    ```bash
    gh api repos/OWNER/REPO/actions/oidc/customization/sub --jq .sub_claim_prefix
    ```

    Matching on `claims.repository` instead pins the policy to one repository in
    either format, at the cost of permitting every branch of it.

## GitLab CI

```yaml
deploy:
  id_tokens:
    CELLCAST_TOKEN:
      aud: cellcast
  script:
    - export CELLCAST_HUB=https://cellcast.example.com
    - cellcast place --workload checkout-api --ttl 15m
    - kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

GitLab puts the token straight into the environment variable, which is the name
the client already reads.

## Buildkite

`buildkite-agent oidc request-token` is built into the agent, so there is no
plugin to install:

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
      - kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

Buildkite interpolates `$VAR` when the pipeline is uploaded and `$$VAR` when the
step runs, so a single `$` here resolves on the wrong machine.

The policy for a Buildkite caller matches on claims rather than on the subject,
because Buildkite's `sub` carries the commit and is different on every build.

## Argo CD

Argo keeps the deploy. A PreSync hook asks cellcast one question before the sync
starts, and the answer is its exit code: is this cell still somewhere this
workload may go. A cell moved to `DRAINING`, or dropped from the policy, fails
the hook and the sync does not run.

That works as a gate because the identity the hook carries is permitted exactly
one cell, so "where should this go" and "may this go here" are the same
question. Moving a workload is then a change to the Application's destination,
reviewed in a pull request. The Job and its policy are in
[examples/integrations/argocd](https://github.com/ethan-kane-ops/cellcast/tree/main/examples/integrations/argocd).

## Branching on the result

The human-readable output is for a build log. Anything the pipeline decides on
should come from `--json`:

```bash
result=$(cellcast place --workload checkout-api --json)

cell=$(jq -r '.cell'   <<<"$result")
source=$(jq -r '.source' <<<"$result")   # hub, cache, or pinned

case "$source" in
  hub)    kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml ;;
  cache|pinned)
    # A fallback returns a cell and never a credential.
    echo "cellcast did not answer; deploying to $cell with the break-glass credential"
    kubectl --kubeconfig "$BREAK_GLASS_KUBECONFIG" -n apps apply -f deploy.yaml ;;
esac
```

Branch on `source`, not on whether a file appeared. On a fallback the client
removes the kubeconfig at the target path, so a credential left by an earlier
run cannot be picked up by the next step.

## Choosing a fallback stance

```bash
cellcast place --workload checkout-api --on-unavailable fail        # default
cellcast place --workload checkout-api --on-unavailable last-known
cellcast place --workload checkout-api --on-unavailable prod-euw1
```

| Stance | Use it when |
|---|---|
| `fail` | The pipeline must not guess. The right default for production |
| `last-known` | The pipeline needs only the cell name, or holds a break-glass credential |
| `<cell>` | One cell is the standing fallback, decided in advance |

Nothing falls back implicitly, and no stance gets past a refusal. If the hub
answers that the caller is not permitted, the command fails whatever the stance
says. If the hub answers that it cannot rank the caller's permitted cells, a
stance may answer it: the cost is a suboptimal cell, never an unauthorised one.

`last-known` reads a decision cache written on the last successful placement,
keyed by workload, valid for `--cache-ttl` (default one hour). On an ephemeral
runner it is empty on every run, so either point `--cache-dir` at something that
survives the job, or treat `last-known` as equivalent to `fail`.

## Seeing what a change would do

Both flags belong in a pre-merge job:

```bash
cellcast place --workload checkout-api --dry-run --explain
```

`--dry-run` runs the whole decision, policy and capacity lookups included, and
stops before minting. Nothing is issued and nothing is deployed. `--explain`
prints every cell and why it was or was not chosen, which is how a policy change
gets reviewed before anything relies on it.

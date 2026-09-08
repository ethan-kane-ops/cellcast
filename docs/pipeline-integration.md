# Pipeline integration

cellcast slots into the pipeline you have. It answers where and hands back a
credential; your existing deploy step does the deploying.

The client needs two things: the hub's URL, and the identity token your CI
platform already issues. There is no secret to configure and nothing to rotate.

!!! note "Purpose-built integrations are not shipped yet"

    A GitHub Action, an Argo CD pre-sync hook and a Buildkite plugin are on the
    [roadmap](https://github.com/ethan-kane-ops/cellcast/blob/main/ROADMAP.md).
    Everything below uses the client binary directly and works today.

## The shape, in any pipeline

```bash
export CELLCAST_HUB=https://cellcast.example.com
export CELLCAST_TOKEN="$(...)"        # your platform's OIDC token

cellcast place --workload checkout-api --ttl 15m
kubectl --kubeconfig cellcast.kubeconfig -n apps apply -f deploy.yaml
```

`cellcast place` writes a kubeconfig at `0600`. The token is never printed and
never passed as a command argument, because on a shared runner both are visible
to every other job on the machine. There is deliberately no `--token` flag.

## GitHub Actions

The token comes from the OIDC endpoint the runner exposes. `id-token: write` is
the permission that makes it available.

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
| `<cell>` | There is a cell you would always fall back to, decided in advance |

Nothing falls back implicitly, and no stance gets past a refusal. If the hub
answers that the caller is not permitted, the command fails whatever the stance
says. If the hub answers that it cannot rank the caller's permitted cells, a
stance may answer it: the cost is a suboptimal cell, never an unauthorised one.

`last-known` reads a decision cache written on the last successful placement,
keyed by workload, valid for `--cache-ttl` (default one hour). On an ephemeral
runner it is empty on every run, so point `--cache-dir` at something that
survives the job or treat `last-known` as equivalent to `fail`.

## Seeing what a change would do

Both flags are worth having in a pre-merge job:

```bash
cellcast place --workload checkout-api --dry-run --explain
```

`--dry-run` runs the whole decision, policy and capacity lookups included, and
stops before minting. Nothing is issued and nothing is deployed. `--explain`
prints every cell and why it was or was not chosen, which is how a policy change
gets reviewed before it is relied on.

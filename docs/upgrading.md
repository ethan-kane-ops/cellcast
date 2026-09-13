# Upgrading

Upgrade the hub first and the cells after it. The hub chart carries its CRDs, so `helm upgrade`
moves the schema and the hub together, and an agent from the previous release keeps reporting to the
upgraded hub while its cell waits for its own upgrade.

```bash
helm upgrade cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast --version <version> -f values.yaml
```

Pass the same values file the install used. `--reuse-values` replaces the new chart's defaults with
the previous release's, so every value added since then is missing and the chart fails to render or
passes an empty flag. Helm 3.14 added `--reset-then-reuse-values` for installs that were never given
a values file.

## From a release that does not relay capacity

Replicas relay each agent's reports to one another ([high availability](high-availability.md)).
Replicas from a release before that pass nothing on, so while they are being replaced a new replica
hears only from the agents whose connections reach it. A new replica holding none of the fleet's
reports waits out `--warmup-timeout` before going ready, and refuses placements with
`PlacementUnavailable` until the agents' connections move to new replicas, which they do as the old
ones go. Nothing needs doing: the rollout can take one warmup timeout longer per replica, and once
the last old replica has gone every replica hears every cell within a heartbeat.

## If the chart does not manage the CRDs

With `crds.install=false`, apply the new release's CRDs before upgrading the chart. A hub reads the
API version its release was built for, and cannot start against CRDs that do not serve it.

```bash
helm template cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast --version <version> \
    --show-only templates/crds.yaml | kubectl apply --server-side -f -
```

Server-side, because a CRD is too large for the annotation client-side apply records.

## From v1alpha1 to v1beta1

Every resource is served and stored at `v1beta1`. `v1alpha1` is still served with the same schema,
so an upgrade changes nothing that exists: objects created before it keep working, and manifests that
name `v1alpha1` keep applying, with a warning.

```text
Warning: cellcast.io/v1alpha1 is deprecated; use cellcast.io/v1beta1
```

Change `apiVersion` in those manifests to `cellcast.io/v1beta1` when convenient. Nothing else in them
changes, and a GitOps controller applying the edited manifest updates the same object rather than
creating another.

### Before a release that stops serving v1alpha1

An object created before the upgrade stays stored at `v1alpha1` until something writes it, and an
object stored at a version its CRD no longer lists cannot be read:

```text
Error from server (StorageReadError): failed to read one or more clusters.cellcast.io from the
storage: ... request to convert CR from an invalid group/version: cellcast.io/v1alpha1
```

Before installing a release that removes `v1alpha1`, rewrite every object at the storage version and
then record that none is left at `v1alpha1`:

```bash
for r in clusters placementpolicies trustconfigs workloadplacements; do
  kubectl get "$r.cellcast.io" -A \
      -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
    | while read -r ns name; do
        kubectl -n "$ns" patch "$r.cellcast.io" "$name" --type=merge -p '{}'
      done
  kubectl patch crd "$r.cellcast.io" --subresource=status --type=merge \
    -p '{"status":{"storedVersions":["v1beta1"]}}'
done
```

An empty patch is still a write, and every write stores the object at the storage version, without a
change for a controller's own write to conflict with. The loop covers every namespace rather than
only the hub's, because a cellcast object anywhere in the cluster is stored under the same CRD.

The API server does not check the trimmed `storedVersions` against what is actually stored; it
records what it is told. Trim it only after every patch in the loop has succeeded. Then:

```bash
kubectl get crd -o custom-columns=NAME:.metadata.name,STORED:.status.storedVersions | grep cellcast
```

Every row reads `[v1beta1]`.

`just verify-upgrade` runs these commands against a fleet built on the previous release, then
removes `v1alpha1` from the CRDs and reads every object back, which fails if the loop missed one
([ADR-013](architecture.md#adr-013-the-api-graduates-to-v1beta1-and-v1alpha1-stays-served-with-the-same-schema)).

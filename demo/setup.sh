#!/usr/bin/env bash
# Builds the three-cell fleet the recording runs against. Not recorded itself.
#
# Everything here is real: three kind clusters, each with its own service
# account issuer, real agents reporting real capacity, and a hub that verifies
# every token against the cluster that signed it. Nothing is stubbed, because
# the site's caption says the recording is a live run and that has to stay true.
#
# The fleet is shaped so the placement is not the obvious one. apse1 is the
# emptiest cell in the fleet and the policy does not permit it, so a scoring
# pass that ran before the permission filter would choose it. That is the
# argument the recording exists to make.
#
#   cell     load    label       permitted
#   euw1     4       env=prod    yes    <- the answer
#   use1     8       env=prod    yes
#   apse1    0       env=dev     no     <- emptiest, and unreachable
#
# Writes everything to demo/.work, which is gitignored and torn down by
# `just demo-down`.
set -euo pipefail

cd "$(dirname "$0")/.."

work="demo/.work"
ns="cellcast-system"
cells="euw1 use1 apse1"
base_port=6451

rm -rf "$work"
mkdir -p "$work"

echo "==> building binaries"
just build > /dev/null

# One bundle holding every cell's cluster CA. The hub fetches each issuer's
# discovery document and keys over HTTPS, and a kind API server signs its own
# certificate, so without this the hub cannot verify a token from any of them.
: > "$work/issuer-roots"

port=$base_port
load=4
for cell in $cells; do
    issuer="https://127.0.0.1:$port"
    kubeconfig="$work/$cell.kubeconfig"

    echo "==> creating cell $cell (issuer $issuer, parked load $load)"

    # The issuer has to be the cluster's own address. A stock kind cluster
    # issues as https://kubernetes.default.svc.cluster.local, which every
    # cluster shares, and the hub tells one cell's agent from another by
    # exactly that claim (docs/architecture.md ADR-009).
    {
        printf 'kind: Cluster\n'
        printf 'apiVersion: kind.x-k8s.io/v1alpha4\n'
        printf 'networking:\n'
        printf '  apiServerAddress: "127.0.0.1"\n'
        printf '  apiServerPort: %s\n' "$port"
        printf 'kubeadmConfigPatches:\n'
        printf '  - |\n'
        printf '    kind: ClusterConfiguration\n'
        printf '    apiServer:\n'
        printf '      extraArgs:\n'
        printf '        - name: service-account-issuer\n'
        printf '          value: %s\n' "$issuer"
        printf '        - name: service-account-jwks-uri\n'
        printf '          value: %s/openid/v1/jwks\n' "$issuer"
    } > "$work/$cell.kind.yaml"

    kind create cluster --name "cellcast-demo-$cell" --config "$work/$cell.kind.yaml" \
        --kubeconfig "$kubeconfig" --wait 120s
    export KUBECONFIG="$kubeconfig"

    # The hub verifies each token against the issuing cluster's key set, so
    # discovery has to be readable without already holding a token. A managed
    # cluster does this for free.
    kubectl create clusterrolebinding oidc-discovery \
        --clusterrole=system:service-account-issuer-discovery \
        --group=system:unauthenticated > /dev/null

    kubectl create ns "$ns" > /dev/null
    kubectl create ns apps > /dev/null

    # ServiceAccount and RBAC rendered from the chart that actually ships, so
    # the fixture cannot drift from what a real cell installs. The Deployment is
    # deliberately not applied: these agents run as host processes so the
    # recording can stop one and show the cell drop out of scoring.
    helm template cellcast-agent charts/cellcast-agent \
        --namespace "$ns" \
        --set cellName="$cell" --set hub.endpoint=https://placeholder.invalid \
        --show-only templates/serviceaccount.yaml \
        --show-only templates/rbac.yaml \
        | kubectl apply -f - > /dev/null

    # The account a deploy credential is minted for. Deliberately narrow: the
    # recording shows that the minted credential gets exactly this and not the
    # permissions the hub itself holds.
    kubectl -n apps create sa deployer > /dev/null
    kubectl -n apps create role deployer \
        --verb=get,list,watch,create,update,patch --resource=pods,deployments > /dev/null
    kubectl -n apps create rolebinding deployer \
        --role=deployer --serviceaccount=apps:deployer > /dev/null

    # A second identity that no policy names, for the refusal at the end.
    kubectl -n apps create sa intern > /dev/null

    # Parked load, so the scoring order is a property of the fixture rather than
    # of whichever cluster happened to be busier during the take.
    if [ "$load" -gt 0 ]; then
        kubectl -n apps create deployment filler \
            --image=registry.k8s.io/pause:3.10 --replicas="$load" > /dev/null
        kubectl -n apps set resources deployment filler \
            --requests=cpu=200m,memory=64Mi > /dev/null
    fi

    kubectl config view --raw --minify \
        -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' \
        | base64 -d > "$work/$cell.ca"
    cat "$work/$cell.ca" >> "$work/issuer-roots"

    kubectl -n "$ns" create token cellcast-agent \
        --audience cellcast --duration 4h > "$work/$cell.agent.jwt"

    printf '%s\n' "$issuer" > "$work/$cell.issuer"

    port=$((port + 1))
    case "$cell" in
        euw1) load=8 ;;
        use1) load=0 ;;
    esac
done

# The first cell's cluster also holds the hub's registry.
export KUBECONFIG="$work/euw1.kubeconfig"

echo "==> installing the registry CRDs"
kubectl apply -f config/crd/bases/ > /dev/null
kubectl wait --for=condition=Established --timeout=60s \
    crd/clusters.cellcast.io crd/placementpolicies.cellcast.io crd/trustconfigs.cellcast.io > /dev/null

# Caller identities. Minted from the hub cluster, which is also a cell, so both
# tokens carry euw1's issuer.
kubectl -n apps create token deployer --audience cellcast --duration 4h > "$work/caller.jwt"
kubectl -n apps create token intern --audience cellcast --duration 4h > "$work/intern.jwt"

echo "==> registering the fleet"
env_for() { case "$1" in apse1) echo dev ;; *) echo prod ;; esac; }

for cell in $cells; do
    # The credential goes from disk to the API server and never through
    # cellcast, which is why cell add prints the TrustConfig and the Cluster and
    # leaves this Secret to kubectl. These are throwaway kind clusters that
    # `just demo-down` deletes, so the kubeconfig here is the admin one.
    kubectl -n "$ns" create secret generic "$cell-kubeconfig" \
        --from-file=kubeconfig="$work/$cell.kubeconfig" > /dev/null

    # The command an operator runs, rather than a fixture describing what it
    # would print. The reporter issuer comes from the cell's own discovery
    # document, so this fails if enrolment cannot build the demo fleet.
    if ! manifests=$(./bin/cellcast cell add --kubeconfig "$work/$cell.kubeconfig" \
        --name "$cell" --hub-namespace "$ns" --agent-namespace "$ns" \
        --labels "env=$(env_for "$cell"),region=$cell" \
        --mint-service-account deployer --mint-namespace apps \
        2> "$work/$cell.enrol"); then
        cat "$work/$cell.enrol" >&2
        exit 1
    fi
    printf '%s\n' "$manifests" | kubectl apply -f - > /dev/null
done

# Pinned to the deploying account. Without the subject the policy would also
# cover the agent, which authenticates through the same issuer (ADR-009).
kubectl apply -f - > /dev/null <<YAML
apiVersion: cellcast.io/v1alpha1
kind: PlacementPolicy
metadata:
  name: checkout-prod
  namespace: $ns
spec:
  subjects:
    - issuer: $(cat "$work/euw1.issuer")
      subject: system:serviceaccount:apps:deployer
  permittedCells:
    matchLabels:
      env: prod
  strategy: LeastLoaded
  tokenTTL:
    default: 15m
    max: 30m
YAML

echo "==> fleet registered"
kubectl -n "$ns" get clusters
echo
echo "next: just demo        (run the scenario)"
echo "      just demo-cast   (record it)"
echo "      just demo-down   (delete the clusters)"

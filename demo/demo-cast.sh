#!/usr/bin/env bash
# The scenario behind the asciinema recording on ethankane.net.
#
# Every command here runs against the fleet demo/setup.sh built: three real kind
# clusters, three real agents publishing real capacity, and a hub verifying each
# token against the cluster that signed it. Nothing is stubbed and no output is
# retyped for the camera.
#
#   just demo-setup   # three clusters and the registry, not recorded
#   just demo         # this script, in the terminal
#   just demo-cast    # this script, under asciinema rec
#
# Kept to 96 columns, which is the width the site's player and its no-JavaScript
# text fallback are sized for.
set -euo pipefail

cd "$(dirname "$0")/.."

work="demo/.work"
BIN="${BIN:-./bin/cellcast}"
intern="$work/intern.jwt"
KUBECONFIG_OUT="cellcast.kubeconfig"

export CELLCAST_HUB="http://127.0.0.1:18080"
export CELLCAST_TOKEN="$(cat "$work/caller.jwt")"
export KUBECONFIG="$work/euw1.kubeconfig"

# Pinned to the fleet, and cleared, so a take cannot inherit the cell a previous
# one cached. The default is the user cache directory, which outlives the demo.
export CELLCAST_CACHE_DIR="$work/cache"
rm -rf "$CELLCAST_CACHE_DIR"

# Set once, silently, so the kubectl lines on screen stay inside the 96 columns
# the site's player is sized for.
kubectl config set-context --current --namespace=cellcast-system > /dev/null

cleanup() { rm -f "$KUBECONFIG_OUT"; }
trap cleanup EXIT

type_line() {
    local line="$1" i
    printf '\033[38;5;108m$\033[0m '
    for ((i = 0; i < ${#line}; i++)); do
        printf '%s' "${line:i:1}"
        sleep 0.02
    done
    printf '\n'
}

run() {
    type_line "$1"
    eval "$1" || true
    echo
}

note() {
    printf '\033[38;5;245m# %s\033[0m\n' "$1"
    sleep 1.3
}

clear

note "Three clusters. CELLCAST_HUB and CELLCAST_TOKEN are set the way CI sets them:"
note "the token is the one the platform already issues. No secret is configured."
echo

run "kubectl get clusters"

note "apse1 is empty. euw1 and use1 are carrying load."
sleep 0.8
echo

run "$BIN place --workload checkout-api --dry-run --explain"

note "apse1 is the emptiest cell in the fleet and it did not win."
note "Policy filters first, and only the survivors are scored."
sleep 1.4
echo

run "$BIN place --workload checkout-api --ttl 15m --kubeconfig $KUBECONFIG_OUT"

note "That credential was minted on demand. Nothing was stored to hand it out."
echo

# jsonpath, not the plain table: `auth whoami` prints 125 columns of UID and
# credential id, which overflows the 96-column frame the site renders this in.
run "kubectl --kubeconfig $KUBECONFIG_OUT auth whoami -o jsonpath='{.status.userInfo.username}'"
run "kubectl --kubeconfig $KUBECONFIG_OUT auth can-i create deploy -n apps"
run "kubectl --kubeconfig $KUBECONFIG_OUT auth can-i list secrets -n kube-system"

note "Enough to run the deploy and nothing else, expiring in fifteen minutes."
sleep 1.2
echo

run "kubectl patch cluster euw1 --type=merge -p '{\"spec\":{\"state\":\"DRAINING\"}}'"
run "$BIN place --workload checkout-api --kubeconfig $KUBECONFIG_OUT"

note "A cell that stops taking deploys stops being an answer."
sleep 1.4
echo

note "Now a different pipeline, carrying an identity no policy names."
echo

run "$BIN place --workload checkout-api --on-unavailable last-known --token-file \$intern"

note "Refused, and the fallback did not apply. A refusal is not an outage, and a"
note "stance that answered one would be a route around the policy engine."
sleep 1.6
echo

# SIGKILL, not SIGTERM. On SIGTERM the hub reports itself unready and keeps
# serving for --drain-delay so that a rolling update does not fail the deploys
# already in flight, which is correct in production and means a polite stop
# here answers the very request that is supposed to find it gone.
#
# A pipeline meeting an unreachable hub is a partition, not a graceful
# shutdown, so the demo produces a partition and then waits for the socket to
# actually stop answering rather than assuming it has.
hub_pid=$(awk '$1 == "hub" { print $2 }' "$work/pids")
kill -9 "$hub_pid" 2> /dev/null || true
for _ in $(seq 1 20); do
    # No -f: the hub answers 401 unauthenticated, and that is still an answer.
    # Only a refused connection means it is gone.
    curl -s -o /dev/null --max-time 1 "$CELLCAST_HUB/api/v1/clusters" || break
    sleep 0.5
done

note "Now the hub is gone. A pipeline declares up front what that means."
echo

run "$BIN place --workload checkout-api --on-unavailable last-known"

note "A cell, from cache. No credential: minting runs through the hub, so a hub"
note "that cannot be reached cannot issue one. The stance says where, never how."
sleep 2

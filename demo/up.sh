#!/usr/bin/env bash
# Starts the hub and one agent per cell as host processes. Not recorded.
#
# The agents run outside the clusters they report for, which is not how they are
# deployed, but it is what lets the recording stop one and show its cell drop
# out of scoring. Everything they publish is read from the real API server they
# are pointed at.
set -euo pipefail

cd "$(dirname "$0")/.."

work="demo/.work"
cells="euw1 use1 apse1"
hub_addr="127.0.0.1:18080"

[ -d "$work" ] || { echo "no fleet; run: just demo-setup" >&2; exit 1; }

# Stop anything from a previous run first. Starting a second hub on a port the
# first still holds does not fail loudly: the new process exits, the pid file
# records it, and every check passes against the old hub that is still serving.
# The scenario then "stops the hub" by killing a pid that is already dead, and
# the recording shows a fallback that never happened.
./demo/down.sh --keep-clusters > /dev/null

mkdir -p "$work/logs"
: > "$work/pids"

issuer_flags=""
for cell in $cells; do
    issuer_flags="$issuer_flags --oidc-issuer=$(cat "$work/$cell.issuer")=generic"
done

# Issuers beyond the three cells, as space-separated "url=provider" pairs. The
# integration workflow sets this to GitHub's issuer so that a real Actions token
# can reach this hub. Empty for the recording, where nothing presents one.
#
# The provider after the "=" selects which claims are extracted, and so what a
# policy can match on. The cells get "generic" because a Kubernetes service
# account token carries nothing else worth matching.
for extra in ${CELLCAST_EXTRA_ISSUERS:-}; do
    issuer_flags="$issuer_flags --oidc-issuer=$extra"
done

echo "==> starting the hub on $hub_addr"
# --oidc-ca-file is what makes this possible without a publicly-trusted issuer.
# Each cell signs its own certificate, so the hub cannot fetch any cell's keys
# until it is told to trust them.
# nohup, so the hub outlives the shell that started it. Without it the hub is
# a child of this script and can go down with the terminal that ran it, which
# looks exactly like a hub that crashed on startup.
KUBECONFIG="$work/euw1.kubeconfig" nohup ./bin/cellcast-hub \
    --addr "$hub_addr" \
    --probe-addr 127.0.0.1:18081 \
    --metrics-addr 0 \
    --namespace cellcast-system \
    --leader-election=false \
    --oidc-audience cellcast \
    --oidc-ca-file "$work/issuer-roots" \
    $issuer_flags \
    --log-level warn --log-format text \
    > "$work/logs/hub.log" 2>&1 &
hub_pid=$!
disown "$hub_pid" 2> /dev/null || true
echo "hub $hub_pid" >> "$work/pids"

probe=18082
for cell in $cells; do
    echo "==> starting the $cell agent"
    nohup ./bin/cellcast-agent \
        --hub-endpoint "http://$hub_addr" \
        --cell-name "$cell" \
        --kubeconfig "$work/$cell.kubeconfig" \
        --token-path "$work/$cell.agent.jwt" \
        --namespace cellcast-system \
        --heartbeat-interval 5s \
        --request-timeout 3s \
        --leader-election=false \
        --probe-addr "127.0.0.1:$probe" \
        --log-level warn --log-format text \
        > "$work/logs/$cell.log" 2>&1 &
    agent_pid=$!
    disown "$agent_pid" 2> /dev/null || true
    echo "$cell $agent_pid" >> "$work/pids"
    probe=$((probe + 1))
done

echo "==> waiting for the fleet to report"
# Deliberately not /readyz. Readiness is bounded on purpose: a replica that has
# waited out --warmup-timeout reports ready and starts refusing placements with
# PlacementUnavailable, logging "becoming ready before capacity covers the
# fleet". That is correct for a hub behind a Service and useless as a signal
# here, because it is indistinguishable from a fleet that is actually reporting.
#
# A placement that succeeds is the only thing that means what this needs it to
# mean, so ask for one.
export CELLCAST_HUB="http://$hub_addr"
export CELLCAST_TOKEN="$(cat "$work/caller.jwt")"
for _ in $(seq 1 60); do
    # Checked every time round, so a hub that died on startup is reported as
    # dead rather than as a fleet that never reported.
    if ! kill -0 "$hub_pid" 2> /dev/null; then
        echo "the hub exited during startup:" >&2
        tail -10 "$work/logs/hub.log" >&2
        exit 1
    fi
    if ./bin/cellcast place --workload readiness-probe --dry-run > /dev/null 2>&1; then
        echo "==> fleet reporting (hub pid $hub_pid)"
        exit 0
    fi
    sleep 2
done

echo "the fleet never reported; see $work/logs/" >&2
tail -20 "$work/logs/hub.log" >&2
for cell in $cells; do
    echo "--- $cell ---" >&2
    tail -5 "$work/logs/$cell.log" >&2
done
exit 1

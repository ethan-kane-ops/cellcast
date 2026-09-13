#!/usr/bin/env bash
# Stops the demo. With --keep-clusters, stops only the hub and agents.
#
# The two are separate because the clusters take minutes to build and the
# scenario takes seconds to change. Iterating on the recording means restarting
# the processes, not rebuilding the fleet.
set -euo pipefail

cd "$(dirname "$0")/.."

work="demo/.work"
keep=false
[ "${1:-}" = "--keep-clusters" ] && keep=true

if [ -f "$work/pids" ]; then
    pids=""
    while read -r name pid; do
        [ -n "${pid:-}" ] || continue
        kill "$pid" 2> /dev/null && echo "stopped $name" || true
        pids="$pids $pid"
    done < "$work/pids"
    rm -f "$work/pids"

    # Wait for them to exit, not just for the signal to be sent. SIGTERM starts
    # the hub's drain: it keeps serving on both ports for --drain-delay, then
    # drains for up to --shutdown-timeout. A hub started inside that window
    # cannot bind, exits, and leaves the old one answering every check, which is
    # the trap up.sh stops on and a restart would otherwise walk straight into.
    for pid in $pids; do
        for _ in $(seq 1 60); do
            kill -0 "$pid" 2> /dev/null || break
            sleep 0.5
        done
        if kill -0 "$pid" 2> /dev/null; then
            echo "pid $pid is still running after 30s; the next start will not own its ports" >&2
        fi
    done
fi

if [ "$keep" = true ]; then
    echo "clusters left running; restart with: just demo-up"
    exit 0
fi

for cell in euw1 use1 apse1; do
    kind delete cluster --name "cellcast-demo-$cell" 2> /dev/null || true
done

rm -rf "$work"
echo "torn down"

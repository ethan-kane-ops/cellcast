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
    while read -r name pid; do
        [ -n "${pid:-}" ] || continue
        kill "$pid" 2> /dev/null && echo "stopped $name" || true
    done < "$work/pids"
    rm -f "$work/pids"
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

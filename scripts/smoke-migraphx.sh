#!/bin/sh
# Run inside the candidate image with GPU devices and a writable /state.
# No vault access, network, Qdrant, or service listener is needed.
set -eu
model=${1:-/opt/attndb/models}
: "${ATTNDB_MIGRAPHX_CACHE_DIR:?set a writable cache root}"
mkdir -p "$ATTNDB_MIGRAPHX_CACHE_DIR"
smoke_cache=$(mktemp -d "$ATTNDB_MIGRAPHX_CACHE_DIR/smoke.XXXXXX")
export ATTNDB_MIGRAPHX_CACHE_DIR="$smoke_cache"
printf 'Isolated smoke cache (retained for inspection): %s\n' "$smoke_cache"
attndb-gpu-smoke -devices-only
printf 'Cold process: compile and infer both models\n'
attndb-gpu-smoke -model "$model"
printf 'Second process: infer with the same persisted cache\n'
attndb-gpu-smoke -model "$model"
find "$smoke_cache" -type f -size +0c -print
printf 'Smoke passed; verify cache-hit diagnostics/timings before claiming cache reuse speedup.\n'

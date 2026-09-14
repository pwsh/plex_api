#!/usr/bin/env bash
#
# Integration smoke test for plex-api inside the linuxserver/plex container.
#
# Copies the real (already repaired) Plex databases into a scratch /config
# tree, builds deploy/Dockerfile.layer, starts a container, waits for
# /health, exercises a few endpoints, then tears everything down.
#
# The originals under $SRC_DB_DIR are only ever read (cp --reflink=auto).
#
# Usage: deploy/test-container.sh [--keep]
#   --keep   leave the container running and the scratch dir in place

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

SRC_DB_DIR="${SRC_DB_DIR:?set SRC_DB_DIR to a directory holding com.plexapp.plugins.library.db and .blobs.db}"
SCRATCH="${SCRATCH:-${TMPDIR:-/tmp}/plex-api-smoke}"
CONFIG_DIR="${SCRATCH}/plex-config"
DB_DIR="${CONFIG_DIR}/Library/Application Support/Plex Media Server/Plug-in Support/Databases"

IMAGE="${IMAGE:-plex-api-layer:dev}"
CONTAINER="${CONTAINER:-plex-api-smoke}"
PORT="${PORT:-32500}"
TOKEN="${TOKEN:-test}"
BASE="http://127.0.0.1:${PORT}"
AUTH=(-H "Authorization: Bearer ${TOKEN}")

KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

RED=$'\033[31m'; GRN=$'\033[32m'; YEL=$'\033[33m'; OFF=$'\033[0m'
info() { printf '%s==>%s %s\n' "$GRN" "$OFF" "$*"; }
warn() { printf '%s==>%s %s\n' "$YEL" "$OFF" "$*"; }
die()  { printf '%sERROR:%s %s\n' "$RED" "$OFF" "$*" >&2; exit 1; }

# ---------------------------------------------------------------- preflight
command -v docker >/dev/null || die "docker not found"
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon"

if [[ ! -f "${REPO_ROOT}/cmd/plex-api/main.go" ]]; then
    die "cmd/plex-api/main.go does not exist yet -- the Go service has not been
       written. This script builds ./cmd/plex-api inside Docker and cannot
       succeed until it is there. Re-run once the Go code lands."
fi
[[ -f "${REPO_ROOT}/go.mod" ]] || die "go.mod missing at ${REPO_ROOT}"

for f in com.plexapp.plugins.library.db com.plexapp.plugins.library.blobs.db; do
    [[ -f "${SRC_DB_DIR}/${f}" ]] || die "source database missing: ${SRC_DB_DIR}/${f}"
done

# ---------------------------------------------------------------- cleanup
cleanup() {
    local rc=$?
    if (( KEEP )); then
        warn "--keep: leaving container '${CONTAINER}' and ${CONFIG_DIR} in place"
        return 0
    fi
    info "tearing down"
    docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
    # The container chowns /config to PUID/PGID, which is us (1000), so this
    # needs no privileges.
    rm -rf "${CONFIG_DIR}" 2>/dev/null || warn "could not remove ${CONFIG_DIR}"
    return $rc
}
trap cleanup EXIT

docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true

# ---------------------------------------------------------------- scratch DB
info "preparing scratch config at ${CONFIG_DIR}"
rm -rf "${CONFIG_DIR}"
mkdir -p "${DB_DIR}"
for f in com.plexapp.plugins.library.db com.plexapp.plugins.library.blobs.db; do
    info "  copying ${f} ($(du -h "${SRC_DB_DIR}/${f}" | cut -f1))"
    cp --reflink=auto "${SRC_DB_DIR}/${f}" "${DB_DIR}/${f}"
done
chmod -R u+rwX "${CONFIG_DIR}"

# ---------------------------------------------------------------- build
info "building ${IMAGE} (deploy/Dockerfile.layer)"
docker build -f "${REPO_ROOT}/deploy/Dockerfile.layer" -t "${IMAGE}" "${REPO_ROOT}"

# ---------------------------------------------------------------- run
info "starting container ${CONTAINER}"
docker run -d --name "${CONTAINER}" \
    -e PUID=1000 -e PGID=1000 -e TZ=UTC -e VERSION=docker \
    -e PLEX_API_BIND="0.0.0.0:32500" \
    -e PLEX_API_TOKEN="${TOKEN}" \
    -e PLEX_API_WRITE=true \
    -e PLEX_API_WRITE_WHILE_RUNNING=true \
    -v "${CONFIG_DIR}:/config" \
    -p "127.0.0.1:${PORT}:32500" \
    "${IMAGE}" >/dev/null

# ---------------------------------------------------------------- wait
# PMS itself may take a long time to come up (or never, unclaimed); the API
# must not wait on it. 120s is generous headroom for the lsio init chain,
# which chowns a 3.4 GB /config on first start.
info "waiting for ${BASE}/health"
deadline=$(( SECONDS + 180 ))
ready=0
while (( SECONDS < deadline )); do
    if ! docker ps --format '{{.Names}}' | grep -qx "${CONTAINER}"; then
        docker logs "${CONTAINER}" 2>&1 | tail -40
        die "container exited during startup"
    fi
    if curl -fsS --max-time 5 "${BASE}/health" >/dev/null 2>&1; then
        ready=1; break
    fi
    sleep 2
done
if (( ! ready )); then
    docker logs "${CONTAINER}" 2>&1 | tail -60
    die "/health did not become ready within 180s"
fi
info "API ready after ~$(( SECONDS ))s"

pms_state=$(docker exec "${CONTAINER}" sh -c 'nc -z localhost 32400 && echo up || echo down' 2>/dev/null || echo unknown)
info "Plex Media Server is currently: ${pms_state} (the tests below do not depend on this)"

# ---------------------------------------------------------------- checks
FAILED=0
show() { # name, curl args...
    local name="$1"; shift
    printf '\n%s--- %s ---%s\n' "$GRN" "$name" "$OFF"
    local body code
    body=$(curl -sS -w $'\n%{http_code}' --max-time 30 "$@" || true)
    code="${body##*$'\n'}"
    body="${body%$'\n'*}"
    if [[ "$code" != 200 ]]; then
        printf '%sHTTP %s%s\n' "$RED" "${code:-<none>}" "$OFF"
        FAILED=1
    else
        printf 'HTTP %s\n' "$code"
    fi
    if command -v jq >/dev/null && printf '%s' "$body" | jq . >/dev/null 2>&1; then
        printf '%s' "$body" | jq . | head -40
    else
        printf '%s\n' "$body" | head -40
    fi
    LAST_BODY="$body"
}

show "GET /health (no auth)" "${BASE}/health"
show "GET /v1/search?q=star" "${AUTH[@]}" "${BASE}/v1/search?q=star"
SEARCH_BODY="$LAST_BODY"
show "GET /v1/tables/metadata_items?limit=2" "${AUTH[@]}" "${BASE}/v1/tables/metadata_items?limit=2"

# Pull the first plausible id out of the search response, whatever the exact
# envelope shape turns out to be.
ITEM_ID=""
if command -v jq >/dev/null; then
    ITEM_ID=$(printf '%s' "$SEARCH_BODY" | jq -r '
        [ .. | objects | (.id // .metadata_item_id // .rating_key // empty) ]
        | map(select(. != null) | tostring) | .[0] // empty' 2>/dev/null || true)
fi
if [[ -n "$ITEM_ID" ]]; then
    show "GET /v1/items/${ITEM_ID}" "${AUTH[@]}" "${BASE}/v1/items/${ITEM_ID}"
else
    warn "no item id found in the /v1/search response; skipping /v1/items/<id>"
fi

printf '\n'
if (( FAILED )); then
    warn "container logs (tail):"
    docker logs "${CONTAINER}" 2>&1 | tail -40
    die "one or more endpoints failed"
fi
info "smoke test PASSED"

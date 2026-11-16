#!/usr/bin/env bash
# Bandingkan GET /api/v1/state antara binary v1 (ref git, default `develop` sebelum v2) dan v2 (worktree ini) di atas
# seed demo yang sama. Keduanya memakai DB baru di container Postgres tes (scripts/test-db.sh), DEMO_MODE=true, dan
# v2 dengan SNAPSHOT_SCOPE=full. Timestamp dinormalkan (dikurangi seedBase masing-masing, serverNow dibuang),
# lalu JSON dibandingkan apa adanya: nama field, urutan field, urutan array, dan nilai.
#
#   scripts/compare-v1.sh [ref-v1]        # keluar 0 kalau identik, 1 kalau ada beda (diff dicetak)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Default: the commit this branch started from (v1 core).
REF="${1:-$(git -C "$ROOT" merge-base HEAD develop)}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sewain-compare.XXXXXX")"
GO_IMAGE="${GO_IMAGE:-golang:1.27.3}"
CACHE_VOLUME="${GO_CACHE_VOLUME:-sewain-go-cache}"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

DB_URL="$("$ROOT/scripts/test-db.sh" up)"
DB_NAME="$(docker ps --format '{{.Names}}' | grep -m1 "^sewain-test-$(basename "$ROOT" | tr -c 'a-zA-Z0-9\n' '-' | cut -c1-40)$")"
base="${DB_URL%/*}"
for db in cmp_v1 cmp_v2; do
  docker exec "$DB_NAME" psql -U sewain -d postgres -qc "DROP DATABASE IF EXISTS $db WITH (FORCE)" -c "CREATE DATABASE $db" >/dev/null
done

echo "▶ build v1 ($REF)"
mkdir -p "$WORK/v1"
git -C "$ROOT" archive "$REF" | tar -x -C "$WORK/v1"
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 \
  -e GOFLAGS=-buildvcs=false -v "$CACHE_VOLUME":/cache -v "$WORK/v1":/src -w /src "$GO_IMAGE" go build -o /src/api ./cmd/api
echo "▶ build v2 (worktree)"
(cd "$ROOT" && CGO_ENABLED=0 scripts/go.sh build -o bin/api-compare ./cmd/api)

start() { # name binary port db extra-env...
  local name=$1 bin=$2 port=$3 db=$4; shift 4
  env -i PATH="$PATH" PORT="$port" DATABASE_URL="$base/$db?sslmode=disable" DEMO_MODE=true APP_ENV=demo \
    UPLOAD_DIR="$WORK/uploads-$name" "$@" "$bin" >"$WORK/$name.log" 2>&1 &
  PIDS+=($!)
  for _ in $(seq 1 60); do
    curl -fs "http://127.0.0.1:$port/api/health" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "$name tidak start:" >&2; cat "$WORK/$name.log" >&2; return 1
}
P1="${V1_PORT:-18731}"; P2="${V2_PORT:-18732}"
start v1 "$WORK/v1/api" "$P1" cmp_v1
start v2 "$ROOT/bin/api-compare" "$P2" cmp_v2 SNAPSHOT_SCOPE=full

curl -fs "http://127.0.0.1:$P1/api/v1/state" >"$WORK/v1.json"
curl -fs "http://127.0.0.1:$P2/api/v1/state" >"$WORK/v2.json"

normalize() {
  python3 -I - "$1" <<'PY'
import json, sys
TIME = {"createdAt","expiresAt","acceptedAt","declinedAt","cancelledAt","activeAt","returnedAt","completedAt","resolvedAt",
        "renterAgreedAt","providerAgreedAt","paidAt","submittedAt","at","serverTimestamp","verifiedAt","reviewedAt","joinedAt",
        "updatedAt","l1Deadline","providerReturnedAt","renterConfirmedAt","autoCompleteAt"}
doc = json.load(open(sys.argv[1]))
base = doc["seedBase"]
def walk(v):
    if isinstance(v, dict):
        return {k: (x - base if k in TIME and isinstance(x, (int, float)) else walk(x)) for k, x in v.items()}
    if isinstance(v, list):
        return [walk(x) for x in v]
    return v
doc.pop("serverNow", None)
doc["seedBase"] = 0
# chatReads values are timestamps too
doc["chatReads"] = {k: x - base for k, x in doc.get("chatReads", {}).items()}
print(json.dumps(walk(doc), indent=1, ensure_ascii=False))
PY
}
normalize "$WORK/v1.json" >"$WORK/v1.norm.json"
normalize "$WORK/v2.json" >"$WORK/v2.norm.json"
if ! diff -u "$WORK/v1.norm.json" "$WORK/v2.norm.json" >"$WORK/diff.txt"; then
  echo "✗ beda (lihat $WORK):"
  head -100 "$WORK/diff.txt"
  exit 1
fi
echo "✓ /api/v1/state identik (v1 $REF vs v2, scope full, $(wc -c <"$WORK/v2.json") byte)"

# Search: same hits (ids, order) and hidden count for the queries of sewain-web/e2e/api.mjs. Distances are rounded
# to 100 m in v2 (FR-S6), so only ids are compared.
for qs in "lat=-7.7713&lng=110.3776&radiusKm=3" "lat=-7.7596&lng=110.3727&radiusKm=3" \
  "lat=-7.7713&lng=110.3776&radiusKm=5&kategori=kamera&jaminan=cukup-identitas" "lat=-7.7713&lng=110.3776&radiusKm=5&penyedia=toko&min=10000&max=50000"; do
  # (v1 cannot answer `serah=…`: its jsonb `?` operator clashes with the query placeholders. Fixed in v2.)
  # v1 leaves ties (listings at the same point) in arbitrary order; v2 breaks them by id.
  a="$(curl -s "http://127.0.0.1:$P1/api/v1/listings/search?$qs" | jq -c '[[.hits | sort_by([(.distanceKm*1e6|round), .id])[] | .id], .hiddenByProviderReach]')"
  b="$(curl -s "http://127.0.0.1:$P2/api/v1/listings/search?$qs" | jq -c '[[.hits[].id], .hiddenByProviderReach]')"
  if [ "$a" != "$b" ]; then
    echo "✗ search beda untuk $qs"; echo "  v1: $a"; echo "  v2: $b"; exit 1
  fi
done
echo "✓ pencarian identik (4 query e2e)"

# Upgrade: stop v1 and start v2 on the database v1 created (v1 JSONB schema) → migration 00001 + re-seed.
kill "${PIDS[0]}"; wait "${PIDS[0]}" 2>/dev/null || true; PIDS[0]=""
P3="${V2_UPGRADE_PORT:-18733}"
start v2-upgrade "$ROOT/bin/api-compare" "$P3" cmp_v1 SNAPSHOT_SCOPE=full
curl -fs "http://127.0.0.1:$P3/api/v1/state" >"$WORK/v2up.json"
normalize "$WORK/v2up.json" >"$WORK/v2up.norm.json"
if ! diff -q <(grep -v '"version"' "$WORK/v1.norm.json") <(grep -v '"version"' "$WORK/v2up.norm.json") >/dev/null; then
  echo "✗ v2 di atas DB v1: snapshot beda (lihat $WORK)"
  exit 1
fi
echo "✓ v2 start di DB berskema v1: tabel v1 di-drop, skema v2 dibuat, seed dimuat ulang"
rm -rf "$WORK"

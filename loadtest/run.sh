#!/usr/bin/env bash
# Sewain v2 load test runner (ROADMAP F5.5). See loadtest/README.md.
#
#   loadtest/run.sh prepare              # account pool → $OUT/pool.json
#   loadtest/run.sh main                 # browse 70% + rent 25% + upload 5%, 0 → 100 → 300 VUs, hold 3 min
#   loadtest/run.sh campus-nat           # 300 VUs from one IP, expects zero 429
#   loadtest/run.sh reset                # demo-login u-admin → resetClock + resetDemo
#   loadtest/run.sh stack-up|stack-down  # throwaway sewain-lt-* stack for a local build (API_IMAGE)
#
# Env: OUT (results dir), BASE_URL (default http://127.0.0.1 = local Caddy), HOST_HEADER, API_CONTAINER,
# DB_CONTAINER, SCALE (1 = 300 VUs), HOLD (default 3m), NAME (result prefix), DEMO_HEALTH_URL (guarded site),
# GUARD=off to skip the guard (only for an isolated stack without a demo next to it).
#
# While k6 runs a guard measures DEMO_HEALTH_URL every 5 s. Three slow (> 1 s) or failed probes in a row stop the
# test at once (docker kill) and write $OUT/GUARD_TRIPPED.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K6_IMAGE="${K6_IMAGE:-grafana/k6:2.3.0}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.11.1}"
OUT="${OUT:-$HERE/out}"
BASE_URL="${BASE_URL:-http://127.0.0.1}"
HOST_HEADER="${HOST_HEADER:-sewain-staging.hallojanu.me}"
API_CONTAINER="${API_CONTAINER:-sewain-staging-api}"
DB_CONTAINER="${DB_CONTAINER:-sewain-staging-db}"
DEMO_HEALTH_URL="${DEMO_HEALTH_URL:-https://sewain.hallojanu.me/api/health}"
K6_NAME="${K6_NAME:-sewain-lt-k6}"
mkdir -p "$OUT"

k6() {
  docker run --rm --name "$K6_NAME" --network host --user "$(id -u):$(id -g)" \
    -v "$HERE":/lt:ro -v "$OUT":/out -w /lt \
    -e BASE_URL="$BASE_URL" -e HOST_HEADER="$HOST_HEADER" -e OUT_DIR=/out -e POOL=/out/pool.json \
    -e SCALE="${SCALE:-1}" -e HOLD="${HOLD:-3m}" -e NAME="${NAME:-}" -e FIXED_IP="${FIXED_IP:-}" \
    -e USERS="${USERS:-320}" -e PROVIDERS="${PROVIDERS:-40}" -e K6_NO_USAGE_REPORT=true \
    "$K6_IMAGE" "$@"
}

metrics() { # Prometheus text from the API's internal :9090 (distroless image: borrow its network namespace)
  docker run --rm --network "container:$API_CONTAINER" "$CURL_IMAGE" -s --max-time 5 http://127.0.0.1:9090/metrics || true
}

guard() {
  local bad=0 t
  echo "ts,seconds,ok" >"$OUT/$1-guard.csv"
  while true; do
    if t="$(curl -s -o /dev/null -w '%{time_total}' --max-time 5 -f "$DEMO_HEALTH_URL")"; then
      echo "$(date +%s),$t,1" >>"$OUT/$1-guard.csv"
      if awk "BEGIN{exit !($t > 1.0)}"; then bad=$((bad + 1)); else bad=0; fi
    else
      echo "$(date +%s),,0" >>"$OUT/$1-guard.csv"
      bad=$((bad + 1))
    fi
    if [ "$bad" -ge 3 ]; then
      echo "$(date -Is) demo health slow/failed 3x in a row, stopping $K6_NAME" | tee "$OUT/GUARD_TRIPPED" >&2
      docker kill "$K6_NAME" >/dev/null 2>&1 || true
      return 0
    fi
    sleep 5
  done
}

stats() {
  echo "ts,container,cpu_pct,mem_used,mem_limit,mem_pct,net_io,block_io,pids" >"$OUT/$1-stats.csv"
  while true; do
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}},{{.NetIO}},{{.BlockIO}},{{.PIDs}}' \
      "$API_CONTAINER" "$DB_CONTAINER" 2>/dev/null | sed "s/ \/ /,/; s/^/$(date +%s),/" >>"$OUT/$1-stats.csv" || true
    sleep 3
  done
}

pgstats() { # pg_stat_statements is not installed; sample active queries/locks instead (read-only)
  echo "ts,active,idle_in_tx,waiting_lock,total" >"$OUT/$1-pg.csv"
  while true; do
    docker exec "$DB_CONTAINER" psql -U sewain -d sewain -AtF, -c \
      "select extract(epoch from now())::int, count(*) filter (where state='active'), count(*) filter (where state like 'idle in transaction%'), count(*) filter (where wait_event_type='Lock'), count(*) from pg_stat_activity where datname='sewain' and pid<>pg_backend_pid()" \
      >>"$OUT/$1-pg.csv" 2>/dev/null || true
    sleep 5
  done
}

run_scenario() {
  local script="$1" name="${NAME:-$2}"
  [ -f "$OUT/pool.json" ] || { echo "no $OUT/pool.json: run '$0 prepare' first" >&2; exit 1; }
  rm -f "$OUT/GUARD_TRIPPED"
  metrics >"$OUT/$name-metrics-before.txt"
  local pids=()
  stats "$name" & pids+=($!)
  pgstats "$name" & pids+=($!)
  if [ "${GUARD:-on}" != off ]; then guard "$name" & pids+=($!); fi
  trap 'kill "${pids[@]}" 2>/dev/null || true; docker kill "$K6_NAME" >/dev/null 2>&1 || true' EXIT INT TERM
  set +e
  NAME="$name" k6 run --quiet --out "csv=/out/$name-k6.csv.gz" "/lt/$script"
  local rc=$?
  set -e
  kill "${pids[@]}" 2>/dev/null || true
  trap - EXIT INT TERM
  metrics >"$OUT/$name-metrics-after.txt"
  [ -f "$OUT/GUARD_TRIPPED" ] && { echo "GUARD TRIPPED: see $OUT/GUARD_TRIPPED" >&2; exit 3; }
  echo "k6 exit $rc (99 = a threshold failed). Results: $OUT/$name-*"
}

reset() {
  local tok
  tok="$(curl -sf -H "Host: $HOST_HEADER" -H 'Content-Type: application/json' -d '{"userId":"u-admin"}' \
    "$BASE_URL/api/v1/auth/demo-login" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  [ -n "$tok" ] || { echo "demo-login u-admin failed" >&2; exit 1; }
  for a in resetClock resetDemo; do
    curl -sf -H "Host: $HOST_HEADER" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
      -d '{"args":[]}' "$BASE_URL/api/v1/actions/$a" && echo
  done
}

# Throwaway stack (stack/docker-compose.yml) for a local build: API_IMAGE (default sewain-lt-api:local).
stack() {
  local envf="$OUT/lt.env"
  case "$1" in
  up)
    umask 077
    [ -f "$envf" ] || {
      echo "DB_PASSWORD=$(openssl rand -hex 16)"
      echo "OTP_PEPPER=$(openssl rand -base64 32)"
      echo "NIK_PEPPER=$(openssl rand -base64 32)"
      echo "KYC_KEYS=v1:$(openssl rand -base64 32)"
      echo "MEDIA_SIGNING_KEY=$(openssl rand -hex 32)"
    } >"$envf"
    docker compose -p sewain-lt --env-file "$envf" -f "$HERE/stack/docker-compose.yml" up -d --wait --force-recreate
    ;;
  down)
    docker compose -p sewain-lt --env-file "$envf" -f "$HERE/stack/docker-compose.yml" down -v --remove-orphans
    rm -f "$envf"
    ;;
  esac
}

case "${1:-}" in
prepare) k6 run --quiet /lt/prepare.js ;;
main) run_scenario main.js main ;;
campus-nat) run_scenario campus-nat.js campus-nat ;;
reset) reset ;;
metrics) metrics ;;
stack-up) stack up ;;
stack-down) stack down ;;
*) sed -n "2,17p" "$0"; exit 2 ;;
esac

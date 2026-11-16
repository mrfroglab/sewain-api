#!/usr/bin/env bash
# Postgres 16 + PostGIS sekali pakai untuk tes integrasi. Satu container per worktree (nama dan port unik),
# data di tmpfs (cepat, hilang saat dihentikan). Tidak pernah menyentuh container stack (sewain-db, sewain-staging-db).
#
#   export TEST_DATABASE_URL=$(scripts/test-db.sh up)   # start (atau pakai yang sudah jalan), cetak URL
#   scripts/test-db.sh url                              # cetak URL container yang sedang jalan
#   scripts/test-db.sh reset                            # kosongkan database (drop + create)
#   scripts/test-db.sh down                             # hapus container
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
slug="$(basename "$ROOT" | tr -c 'a-zA-Z0-9\n' '-' | cut -c1-40)"
NAME="${TEST_DB_NAME:-sewain-test-$slug}"
IMAGE="${TEST_DB_IMAGE:-postgis/postgis:16-3.4-alpine}"

url() {
  local port
  port="$(docker port "$NAME" 5432/tcp 2>/dev/null | head -1 | sed 's/.*://')"
  [ -n "$port" ] || { echo "container $NAME tidak jalan (scripts/test-db.sh up)" >&2; return 1; }
  echo "postgres://sewain:test@127.0.0.1:$port/sewain?sslmode=disable"
}

wait_ready() {
  for _ in $(seq 1 60); do
    docker exec "$NAME" pg_isready -U sewain -d sewain >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "Postgres tes tidak siap: docker logs $NAME" >&2
  return 1
}

case "${1:-up}" in
up)
  if [ -z "$(docker ps -q -f "name=^${NAME}$")" ]; then
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    docker run -d --name "$NAME" \
      -e POSTGRES_USER=sewain -e POSTGRES_PASSWORD=test -e POSTGRES_DB=sewain \
      -p 127.0.0.1::5432 --tmpfs /var/lib/postgresql/data:rw --memory 512m \
      --label sewain.test=1 "$IMAGE" >/dev/null
  fi
  wait_ready
  # PostGIS image menjalankan init lalu restart sekali; tunggu lagi supaya koneksi pertama tidak putus.
  sleep 2; wait_ready
  url ;;
url) url ;;
reset)
  docker exec "$NAME" psql -U sewain -d postgres -qc "DROP DATABASE IF EXISTS sewain WITH (FORCE)" -c "CREATE DATABASE sewain" >/dev/null
  url ;;
down) docker rm -f "$NAME" >/dev/null 2>&1 || true ;;
*) echo "pakai: $0 up|url|reset|down" >&2; exit 2 ;;
esac

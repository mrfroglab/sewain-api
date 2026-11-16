#!/usr/bin/env bash
# Jalankan toolchain Go lewat Docker (server ini tidak memasang Go). Argumen diteruskan ke `go`.
#
#   scripts/go.sh test ./...
#   scripts/go.sh run ./cmd/api
#   TEST_DATABASE_URL=$(scripts/test-db.sh up) scripts/go.sh test ./...
#
# Container memakai jaringan host (bisa menjangkau Postgres tes di 127.0.0.1), berjalan sebagai user kita
# (berkas hasil tidak dimiliki root), dan dibatasi memori/CPU supaya beberapa worktree bisa build bersamaan.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_IMAGE="${GO_IMAGE:-golang:1.27.4}"
CACHE_VOLUME="${GO_CACHE_VOLUME:-sewain-go-cache}"

# Volume cache bersama (modul + build) dibuat sekali dan dimiliki user kita.
if ! docker volume inspect "$CACHE_VOLUME" >/dev/null 2>&1; then
  docker volume create "$CACHE_VOLUME" >/dev/null
  docker run --rm -v "$CACHE_VOLUME":/cache busybox:1.37 \
    sh -c "mkdir -p /cache/build /cache/mod && chown -R $(id -u):$(id -g) /cache"
fi

tty=()
[ -t 0 ] && [ -t 1 ] && tty=(-t)

envs=(-e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e "GOFLAGS=${GOFLAGS:--buildvcs=false}")
for v in CGO_ENABLED GOOS GOARCH DATABASE_URL TEST_DATABASE_URL APP_ENV DEMO_MODE PORT UPLOAD_DIR; do
  [ -n "${!v:-}" ] && envs+=(-e "$v")
done

exec docker run --rm -i "${tty[@]}" \
  --network host \
  --user "$(id -u):$(id -g)" \
  --memory "${GO_MEMORY:-2g}" --cpus "${GO_CPUS:-2}" \
  "${envs[@]}" \
  -v "$CACHE_VOLUME":/cache \
  -v "$ROOT":"$ROOT" -w "$PWD" \
  "$GO_IMAGE" go "$@"

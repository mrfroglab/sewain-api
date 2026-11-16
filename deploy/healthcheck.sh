#!/usr/bin/env bash
# Sewain: watchdog tiap 5 menit (timer systemd user). ROADMAP F7.4.
# Cek dari LUAR (lewat DNS Cloudflare, jadi tunnel/DNS mati ikut ketahuan), kesehatan container, dan umur backup.
# Notifikasi hanya saat status BERUBAH (2 cek gagal berturut-turut ≈ 10 menit, supaya tidak alarm karena kedipan).
#
#   healthcheck.sh         jalankan cek
#   healthcheck.sh test    kirim notifikasi uji
#
# Dipasang ke /opt/stacks/apps/<stack>/healthcheck.sh. Pengaturan di $STACK/monitor.env (opsional, chmod 600):
#   SITE_URL=https://sewain.hallojanu.me
#   MONITOR_NOTIFY_URL=https://ntfy.sh/<topik-rahasia>   (kosong = hanya tulis ke jurnal)
set -uo pipefail

STACK=${STACK:-/opt/stacks/apps/sewain}
NAME=${NAME:-$(basename "$STACK")}
ENV_FILE=${ENV_FILE:-$STACK/monitor.env}
STATE=${STATE:-$STACK/.monitor-state}
BACKUP_STATUS=${BACKUP_STATUS:-/opt/stacks/backup/$NAME/last-status.json}
SITE_URL=https://sewain.hallojanu.me
MONITOR_NOTIFY_URL=
# shellcheck disable=SC1090
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
FAILS_BEFORE_ALERT=2

notify() { # notify <judul> <isi> <prioritas> <tag>
  echo "[$(date -Is)] NOTIFY: $1 — $2"
  [ -n "$MONITOR_NOTIFY_URL" ] || return 0
  curl -fsS -m 15 -H "Title: $1" -H "Priority: ${3:-high}" -H "Tags: ${4:-warning}" \
    -d "$2" "$MONITOR_NOTIFY_URL" >/dev/null || echo "peringatan: notifikasi gagal"
}

if [ "${1:-}" = test ]; then notify "Sewain monitor ($NAME)" "Notifikasi uji: monitor aktif untuk $SITE_URL" default white_check_mark; exit 0; fi

problems=()
# Resolver lokal (AdGuard) bisa menyimpan cache lama, jadi resolve lewat DoH Cloudflare.
probe() { curl -s -o /dev/null -w '%{http_code}' -m 20 --doh-url https://1.1.1.1/dns-query "$1" || true; }
for path in /api/health /; do
  code=$(probe "$SITE_URL$path")
  [ "$code" = 200 ] || problems+=("$SITE_URL$path → HTTP ${code:-000}")
done
for c in "$NAME-db" "$NAME-api" "$NAME-web"; do
  s=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$c" 2>/dev/null || echo missing)
  [ "$s" = healthy ] || problems+=("container $c: $s")
done
if [ -f "$BACKUP_STATUS" ]; then
  read -r bstate bage < <(python3 - "$BACKUP_STATUS" <<'PY'
import json, sys, datetime
d = json.load(open(sys.argv[1]))
age = (datetime.datetime.now().astimezone() - datetime.datetime.fromisoformat(d["at"])).total_seconds() / 3600
print(d.get("state", "?"), int(age))
PY
)
  [ "$bstate" = failed ] && problems+=("backup terakhir GAGAL")
  [ "${bage:-0}" -gt 30 ] && problems+=("backup terakhir ${bage} jam lalu")
fi

prev_state=ok; prev_fails=0
[ -f "$STATE" ] && read -r prev_state prev_fails < "$STATE"

if [ ${#problems[@]} -eq 0 ]; then
  [ "$prev_state" = down ] && notify "✅ Sewain ($NAME) pulih" "Semua cek OK lagi: $SITE_URL" default white_check_mark
  echo "ok 0" > "$STATE"
  echo "[$(date -Is)] OK"
  exit 0
fi

fails=$((prev_fails + 1))
msg=$(printf '%s\n' "${problems[@]}")
echo "[$(date -Is)] MASALAH ($fails): $msg"
if [ "$prev_state" != down ] && [ "$fails" -ge "$FAILS_BEFORE_ALERT" ]; then
  notify "🔴 Sewain ($NAME) bermasalah" "$msg" urgent rotating_light
  echo "down $fails" > "$STATE"
else
  echo "$prev_state $fails" > "$STATE"
fi
exit 0

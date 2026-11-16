#!/usr/bin/env bash
# Sewain: backup harian Postgres + folder uploads (foto bukti). ROADMAP F0.1 / F5.6.
#
#   backup.sh              jalankan backup (dipanggil timer systemd user tiap hari)
#   backup.sh verify       pulihkan dump terbaru ke database sementara lalu hitung baris (uji restore)
#   backup.sh notify-fail  kirim notifikasi kegagalan (dipanggil systemd lewat OnFailure=)
#
# Dipasang ke /opt/stacks/apps/<stack>/backup.sh. Pengaturan opsional ada di $ENV_FILE (lihat backup.env.example).
# Rahasia tidak pernah ditulis di sini.
set -Eeuo pipefail

STACK=${STACK:-/opt/stacks/apps/sewain}
NAME=${NAME:-$(basename "$STACK")}
DEST=${DEST:-/opt/stacks/backup/$NAME}
ENV_FILE=${ENV_FILE:-$STACK/backup.env}
DB_CONTAINER=${DB_CONTAINER:-$NAME-db}
DB_USER=${DB_USER:-sewain}
DB_NAME=${DB_NAME:-sewain}
# shellcheck disable=SC1090
[ -f "$ENV_FILE" ] && . "$ENV_FILE"

REMOTE=${BACKUP_REMOTE:-}                # contoh: gdrive-crypt:sewain  (kosong = hanya salinan lokal)
NOTIFY_URL=${BACKUP_NOTIFY_URL:-}        # ntfy / Discord / Slack webhook (kosong = hanya tulis ke jurnal)
HEARTBEAT_URL=${BACKUP_HEARTBEAT_URL:-}  # ping setelah backup LENGKAP berhasil (mis. healthchecks.io)
KEEP_DAILY=${KEEP_DAILY:-7}
KEEP_WEEKLY=${KEEP_WEEKLY:-4}
STATUS="$DEST/last-status.json"

log() { echo "[$(date -Is)] $*"; }

notify() { # notify <judul> <isi>
  local title=$1 body=$2
  log "NOTIFY: $title — $body"
  [ -n "$NOTIFY_URL" ] || return 0
  case "$NOTIFY_URL" in
    *discord.com/api/webhooks/*)
      curl -fsS -m 15 -H 'Content-Type: application/json' \
        -d "$(python3 -c 'import json,sys;print(json.dumps({"content":"**%s**\n%s"%(sys.argv[1],sys.argv[2])}))' "$title" "$body")" "$NOTIFY_URL" >/dev/null ;;
    *hooks.slack.com/*)
      curl -fsS -m 15 -H 'Content-Type: application/json' \
        -d "$(python3 -c 'import json,sys;print(json.dumps({"text":"*%s*\n%s"%(sys.argv[1],sys.argv[2])}))' "$title" "$body")" "$NOTIFY_URL" >/dev/null ;;
    *) curl -fsS -m 15 -H "Title: $title" -H 'Priority: high' -H 'Tags: warning' -d "$body" "$NOTIFY_URL" >/dev/null ;;
  esac || log "peringatan: notifikasi gagal terkirim"
}

write_status() { # write_status <ok|failed|offsite-failed> <pesan>
  mkdir -p "$DEST"
  python3 - "$1" "$2" >"$STATUS" <<'PY'
import json, sys, datetime
print(json.dumps({"state": sys.argv[1], "message": sys.argv[2], "at": datetime.datetime.now().astimezone().isoformat()}))
PY
}

case "${1:-run}" in
notify-fail)
  msg=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("message",""))' "$STATUS" 2>/dev/null || true)
  notify "Backup $NAME GAGAL" "${msg:-backup berhenti dengan error, lihat: journalctl --user -u $NAME-backup.service}"
  exit 0 ;;

verify)
  latest=$(find "$DEST/daily" -mindepth 1 -maxdepth 1 -type d | sort | tail -1)
  [ -n "$latest" ] || { echo "belum ada backup di $DEST/daily" >&2; exit 1; }
  (cd "$latest" && sha256sum -c --quiet SHA256SUMS)
  scratch=restore_check_$$
  docker exec "$DB_CONTAINER" createdb -U "$DB_USER" "$scratch"
  trap 'docker exec "$DB_CONTAINER" dropdb -U "$DB_USER" --if-exists "$scratch" >/dev/null' EXIT
  docker exec -i "$DB_CONTAINER" pg_restore -U "$DB_USER" -d "$scratch" --no-owner <"$latest/db.dump"
  # Hitung baris tabel utama (nama tabel sama di skema v1 dan v2).
  docker exec "$DB_CONTAINER" psql -U "$DB_USER" -d "$scratch" -tAc \
    "select 'users='||(select count(*) from users)||' listings='||(select count(*) from listings)||' bookings='||(select count(*) from bookings)"
  tar -tzf "$latest/uploads.tar.gz" >/dev/null
  echo "restore OK dari $latest"; exit 0 ;;
esac

# --- backup ---------------------------------------------------------------
mkdir -p "$DEST/daily" "$DEST/weekly"
chmod 700 "$DEST"
exec 9>"$DEST/.backup.lock"
flock -n 9 || { log "backup lain masih berjalan, lewati"; exit 0; }
trap 'rc=$?; [ -n "${DIR:-}" ] && rm -rf "$DIR"; write_status failed "baris $LINENO: ${BASH_COMMAND} (exit $rc)"; exit $rc' ERR

STAMP=$(date +%Y-%m-%d_%H%M)
DIR="$DEST/daily/$STAMP"
mkdir -p "$DIR"

log "dump database $DB_NAME ($DB_CONTAINER)"
docker exec "$DB_CONTAINER" pg_dump -U "$DB_USER" -d "$DB_NAME" -Fc --no-owner >"$DIR/db.dump"
docker exec -i "$DB_CONTAINER" pg_restore --list <"$DIR/db.dump" >/dev/null   # arsip harus terbaca
log "db.dump $(du -h "$DIR/db.dump" | cut -f1)"

log "arsipkan uploads"
tar -C "$STACK/data" -czf "$DIR/uploads.tar.gz" uploads
(cd "$DIR" && sha256sum db.dump uploads.tar.gz >SHA256SUMS)
chmod -R go-rwx "$DIR"

[ "$(date +%u)" = 7 ] && cp -a "$DIR" "$DEST/weekly/$STAMP"

prune() { find "$1" -mindepth 1 -maxdepth 1 -type d | sort | head -n -"$2" | xargs -r rm -rf; }
log "rotasi (simpan $KEEP_DAILY harian, $KEEP_WEEKLY mingguan)"
prune "$DEST/daily" "$KEEP_DAILY"
prune "$DEST/weekly" "$KEEP_WEEKLY"
DIR=""

if [ -n "$REMOTE" ]; then
  log "sinkron ke $REMOTE"
  if ! rclone sync "$DEST" "$REMOTE" --transfers 2 --exclude 'last-status.json' --exclude '.backup.lock' --quiet; then
    write_status offsite-failed "salinan lokal OK ($STAMP) tapi sinkron ke $REMOTE gagal (cek: rclone config reconnect ${REMOTE%%:*}:)"
    notify "Backup $NAME: salinan luar server GAGAL" "Backup lokal $STAMP aman, tapi rclone ke $REMOTE gagal. Biasanya token kedaluwarsa: rclone config reconnect ${REMOTE%%:*}:"
    exit 3
  fi
else
  log "BACKUP_REMOTE kosong: hanya salinan lokal (belum ada salinan di luar server)"
fi

write_status ok "backup $STAMP selesai$([ -n "$REMOTE" ] && echo ", tersalin ke $REMOTE")"
log "selesai: $DEST/daily/$STAMP"

if [ -n "$HEARTBEAT_URL" ]; then
  curl -fsS -m 10 --retry 2 -o /dev/null "$HEARTBEAT_URL" && log "heartbeat terkirim" || log "peringatan: heartbeat gagal terkirim"
fi

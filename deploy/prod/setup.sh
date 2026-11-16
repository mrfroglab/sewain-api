#!/usr/bin/env bash
# Pasang stack PRODUKSI Sewain di server ini (ROADMAP F7.1). Idempoten: rahasia yang sudah ada tidak ditimpa.
#
#   deploy/prod/setup.sh sewain.me            # host publik final (domain dibeli dulu, ROADMAP §9 #1)
#
# Yang dilakukan:
#   1. /opt/stacks/apps/sewain-prod: compose, folder data, .env (chmod 600) dengan rahasia BARU yang dibuat di sini
#      (bukan salinan staging): DB_PASSWORD, OTP_PEPPER, NIK_PEPPER, KYC_KEYS, MEDIA_SIGNING_KEY, kunci VAPID.
#   2. Baris kosong untuk kredensial pihak ketiga (Zenziva, email, R2) yang diisi pemilik akun.
#   3. backup.sh + healthcheck.sh + timer systemd user (sewain-prod-backup 02:30, sewain-prod-monitor 5 menit).
#   4. Blok Caddy "# BEGIN sewain-prod" (validasi dulu, kembalikan cadangan kalau gagal).
# Yang TIDAK dilakukan: hostname/DNS Cloudflare (lewat opencode), menyalakan api/web (butuh image :prod dari tag rilis
# dan kredensial OTP), dan memberi peran admin (`docker exec sewain-prod-api /app/api admin grant <hp>`).
set -euo pipefail

HOST=${1:?pakai: $0 <host-publik>  (mis. sewain.me)}
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # sewain-api/deploy
STACK=/opt/stacks/apps/sewain-prod
CADDYFILE=/opt/stacks/reverse-proxy/Caddyfile
API_IMAGE=ghcr.io/mrfroglab/sewain-api:${API_TAG:-develop}   # dipakai hanya untuk `api vapid gen`

mkdir -p "$STACK/data/postgres" "$STACK/data/uploads"
sudo -n chown 65532:65532 "$STACK/data/uploads" 2>/dev/null || chmod 777 "$STACK/data/uploads"
cp "$SRC/prod/docker-compose.yml" "$STACK/docker-compose.yml"
cp "$SRC/backup.sh" "$STACK/backup.sh" && chmod 750 "$STACK/backup.sh"
cp "$SRC/healthcheck.sh" "$STACK/healthcheck.sh" && chmod 750 "$STACK/healthcheck.sh"

umask 077
ENV="$STACK/.env"; touch "$ENV"; chmod 600 "$ENV"
add() { grep -q "^$1=" "$ENV" || printf '%s=%s\n' "$1" "$2" >> "$ENV"; }
rand() { head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 40; }
subnet=$(docker network inspect gateway-net -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}')

add DB_PASSWORD "$(rand)"
add OTP_PEPPER "$(openssl rand -base64 32)"
add NIK_PEPPER "$(openssl rand -base64 32)"
add KYC_KEYS "v1:$(openssl rand -base64 32)"
add MEDIA_SIGNING_KEY "$(openssl rand -hex 32)"
if ! grep -q '^VAPID_PRIVATE_KEY=' "$ENV"; then
  docker run --rm "$API_IMAGE" vapid gen | grep -E '^VAPID_(PUBLIC|PRIVATE)_KEY=[A-Za-z0-9_-]+$' >> "$ENV"
fi
add VAPID_SUBJECT "https://$HOST"
add PUBLIC_URL "https://$HOST"
add CORS_ORIGINS "https://$HOST"
add TRUSTED_PROXY_CIDRS "$subnet"
add PAYMENT_MODE offline
add MEDIA_BACKEND local
add OTP_PROVIDER zenziva
add SESSION_COOKIE_SECURE true
add TERMS_VERSION ""
add PRIVACY_VERSION ""
add MAIL_PROVIDER none
for v in ZENZIVA_WA_URL ZENZIVA_SMS_URL ZENZIVA_USERKEY ZENZIVA_PASSKEY MAIL_FROM RESEND_API_KEY \
         S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY_ID S3_SECRET_ACCESS_KEY S3_RETENTION_ACCESS_KEY_ID S3_RETENTION_SECRET_ACCESS_KEY; do
  add "$v" ""
done
printf 'SITE_URL=https://%s\nMONITOR_NOTIFY_URL=\n' "$HOST" > "$STACK/monitor.env.new"
[ -f "$STACK/monitor.env" ] || mv "$STACK/monitor.env.new" "$STACK/monitor.env"; rm -f "$STACK/monitor.env.new"
[ -f "$STACK/backup.env" ] || cp "$SRC/backup.env.example" "$STACK/backup.env"
chmod 600 "$STACK/monitor.env" "$STACK/backup.env"

# --- timer systemd user: backup + watchdog, dengan STACK produksi ---
UNIT=~/.config/systemd/user
mkdir -p "$UNIT"
cat > "$UNIT/sewain-prod-backup.service" <<EOF
[Unit]
Description=Sewain PRODUCTION daily backup (Postgres + uploads)
After=docker.service network-online.target
OnFailure=sewain-prod-backup-notify.service

[Service]
Type=oneshot
Environment=STACK=$STACK
ExecStart=$STACK/backup.sh
EOF
cat > "$UNIT/sewain-prod-backup-notify.service" <<EOF
[Unit]
Description=Notify that the Sewain PRODUCTION backup failed

[Service]
Type=oneshot
Environment=STACK=$STACK
ExecStart=$STACK/backup.sh notify-fail
EOF
cat > "$UNIT/sewain-prod-backup.timer" <<'EOF'
[Unit]
Description=Run Sewain PRODUCTION backup daily at 02:30

[Timer]
OnCalendar=*-*-* 02:30:00
Persistent=true
RandomizedDelaySec=5m

[Install]
WantedBy=timers.target
EOF
cat > "$UNIT/sewain-prod-monitor.service" <<EOF
[Unit]
Description=Sewain PRODUCTION uptime/health watchdog

[Service]
Type=oneshot
Environment=STACK=$STACK
ExecStart=$STACK/healthcheck.sh
EOF
cat > "$UNIT/sewain-prod-monitor.timer" <<'EOF'
[Unit]
Description=Run Sewain PRODUCTION watchdog every 5 minutes

[Timer]
OnBootSec=3min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
EOF
systemctl --user daemon-reload
systemctl --user enable sewain-prod-backup.timer sewain-prod-monitor.timer >/dev/null

# --- Caddy ---
cp "$CADDYFILE" "$CADDYFILE.bak-sewain-prod-$(date +%Y%m%d%H%M%S)"
SITE_HOST="$HOST" python3 - "$CADDYFILE" <<'PY'
import os, re, sys
path, host = sys.argv[1], os.environ["SITE_HOST"]
block = f"""# BEGIN sewain-prod (stack /opt/stacks/apps/sewain-prod, tag rilis)
http://{host} {{
	encode zstd gzip
	header {{
		X-Content-Type-Options nosniff
		Referrer-Policy strict-origin-when-cross-origin
		Permissions-Policy "camera=(self), geolocation=(self), microphone=()"
		Strict-Transport-Security "max-age=31536000"
		-Server
	}}
	handle /api/* {{
		reverse_proxy sewain-prod-api:8080 {{
			header_up X-Forwarded-For {{http.request.header.CF-Connecting-IP}}
		}}
	}}
	handle {{
		reverse_proxy sewain-prod-web:3000 {{
			header_up X-Forwarded-For {{http.request.header.CF-Connecting-IP}}
		}}
	}}
}}
# END sewain-prod"""
src = open(path).read()
pat = re.compile(r"# BEGIN sewain-prod .*?# END sewain-prod", re.S)
src = pat.sub(block, src) if pat.search(src) else src.rstrip() + "\n\n" + block + "\n"
with open(path, "r+") as f:
    f.seek(0); f.write(src); f.truncate()
PY
if ! docker exec caddy-proxy caddy validate --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
  echo "Caddyfile tidak valid, mengembalikan cadangan." >&2
  cp "$(ls -t "$CADDYFILE".bak-sewain-prod-* | head -1)" "$CADDYFILE.tmp" && cat "$CADDYFILE.tmp" > "$CADDYFILE" && rm "$CADDYFILE.tmp"
  exit 1
fi
docker exec caddy-proxy caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1

cd "$STACK" && docker compose up -d db >/dev/null
cat <<EOF

Stack produksi siap di $STACK (db jalan; api/web belum).
Berikutnya:
  1. Hostname: opencode run --auto "Tambahkan public hostname $HOST ... → http://localhost:80, buat record DNS-nya"
  2. Isi kredensial di $STACK/.env: ZENZIVA_* (OTP), TERMS_VERSION/PRIVACY_VERSION (teks final), opsional MAIL_*/S3_*.
  3. SIMPAN SALINAN NIK_PEPPER dan KYC_KEYS di password manager (NIK_PEPPER tidak bisa diganti).
  4. Rilis: tag vX.Y.Z di sewain-api dan sewain-web → image :prod → deploy (job CI deploy-prod, butuh persetujuan).
  5. Admin: docker exec sewain-prod-api /app/api admin grant <nomor-hp>
  6. backup.env (BACKUP_REMOTE, BACKUP_NOTIFY_URL) dan monitor.env (MONITOR_NOTIFY_URL), lalu cek go-live di deploy/OPS.md §7.
EOF

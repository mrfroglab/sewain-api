# sewain-api

Backend **Sewain** (PRD §13), marketplace sewa barang P2P untuk pilot kampus UGM. Go + Postgres/PostGIS. Server
ini sumber data tunggal untuk web: penyewa, penyedia, dan admin bisa memakai perangkat berbeda.

- Demo (v1, dibekukan di `main`): https://sewain.hallojanu.me/api/health
- Staging (v2, branch `develop`): https://sewain-staging.hallojanu.me/api/health
- Stack: Go 1.27 (`net/http`, `pgx/v5`, `coder/websocket`), Postgres 16 + PostGIS 3.4, goose (migrasi), sqlc
  (query bertipe), Prometheus client, image distroless `nonroot`.
- Rencana: `ROADMAP.md` §2–§5 (repo induk). Runbook operasional: [`deploy/OPS.md`](deploy/OPS.md). Catatan
  pemasangan per modul: [`docs/integration/`](docs/integration/), keputusan gelombang 2:
  [`docs/integration/integrate-v2.md`](docs/integration/integrate-v2.md). Review keamanan:
  [`docs/security/review-wave1.md`](docs/security/review-wave1.md).

## Arsitektur v2

```
browser ─► Cloudflare Tunnel ─► Caddy ─► api :8080 ─┬─► Postgres + PostGIS (satu transaksi per aksi)
                                                    ├─► media.Storage (disk lokal / R2, write-once)
             WebSocket /api/v1/events ◄── Hub ◄─────┤◄── LISTEN/NOTIFY (fan-out antar instance)
                                                    └─► outbox ─► dispatcher ─► web push / email
                              :9090 /metrics (internal, tidak lewat Caddy)
```

| Paket | Isi |
|---|---|
| `cmd/api` | Proses: serve, `migrate`, `admin grant/revoke`, `vapid gen`, `healthcheck`. Shutdown rapi. |
| `internal/app` | Wiring semua modul + runner latar (job waktu, retensi, dispatcher outbox, listener). `apptest` = harness tes E2E. |
| `internal/config` | Semua env, validasi saat start (produksi menolak konfigurasi demo, semua galat dilaporkan sekaligus). |
| `internal/httpapi` | Rute HTTP + rantai middleware platform, snapshot dengan URL foto bertanda tangan per viewer. |
| `internal/service` | Aksi transaksional (aturan dari `internal/domain`), snapshot per viewer, KYC mirror, retensi, ekspor/hapus akun, metrik admin, impor CSV. |
| `internal/domain` | Aturan bisnis murni (tanpa I/O), tipe JSON kontrak web, skor kepercayaan. |
| `internal/db` | Pool pgx, migrasi goose (advisory lock, out-of-order), query sqlc (`dbq`). |
| `internal/auth`, `internal/kyc`, `internal/crypto` | OTP, sesi (cookie/bearer, CSRF), consent, admin; KYC terenkripsi; AES-GCM + HMAC. |
| `internal/media`, `internal/geo` | Storage, validasi unggahan, strip GPS, URL bertanda tangan, watermark, rencana retensi; lokasi publik. |
| `internal/realtime`, `internal/notify` | Hub WebSocket per user + Publisher/Listener; outbox, web push VAPID, email. |
| `internal/platform/*` | Middleware (request ID, IP klien, log JSON tanpa PII, recover, header keamanan, CORS, batas body, timeout), rate limit, metrics, health, server. |

### Alur penting

- **Aksi** (`POST /api/v1/actions/{name}`): satu transaksi Postgres, aktor = argumen pertama dan harus pemilik
  sesi. Pelanggaran aturan = `200 {ok:false,error}`. `Idempotency-Key` (header atau field body `idempotencyKey`,
  header menang): kunci sama → respons asli; kunci sama dengan argumen lain → `409`.
- **Snapshot** (`GET /api/v1/state`): bentuk `DataState` web + `scope`, `viewerId`, `listingBusy`,
  `users[].publicStats`, `reviews[].visibleAt`. `SNAPSHOT_SCOPE=scoped` (wajib di produksi) menyaring per viewer
  (ROADMAP §4): tamu = data publik; user = + datanya sendiri dan booking tempat ia jadi pihak; admin = semua.
  `SNAPSHOT_USERS=referenced` membatasi `users` ke profil yang dirujuk snapshot itu (admin tetap semua).
  Lokasi listing/papan milik orang lain = `geo.FuzzPoint` (≤ 400 m). `src` foto bukti = URL bertanda tangan untuk
  viewer itu (pihak: salinan ber-watermark, admin: asli); foto yang tidak boleh dilihat tidak dikirim.
- **Realtime**: setiap commit menulis `change_log` (versi) lalu event `{type:"changed",version}` hanya ke pihak
  terkait + admin (atau semua kalau data publik berubah), lintas instance lewat `LISTEN/NOTIFY`.
- **Notifikasi**: setiap notifikasi in-app mengantre ke `notification_outbox` di transaksi yang sama; dispatcher
  mengirim (retry eksponensial, dead-letter). Notifikasi "ke admin" dikirim ke semua akun ber-role admin.
- **KYC produksi**: `POST /api/v1/kyc/submissions` (terenkripsi) → `verification_requests` (`manual`, user
  `menunggu`) → admin memutuskan lewat `reviewVerification` atau `/api/v1/admin/kyc/{id}/decision` (keduanya
  `kyc.Decide`) → user `terverifikasi` + `nik_masked`. Simulasi `submitVerification` hanya di `DEMO_MODE`.
- **Pembayaran**: `PAYMENT_MODE=offline` (default di luar demo): penyewa `payBooking(..., "langsung")`, penyedia
  `confirmDirectPayment`. `simulated` (default demo): perilaku demo. `gateway`: Fase 8, ditolak saat start.
- **Pencarian**: radius hanya 1/3/5/10/25 km (dibulatkan ke atas), keanggotaan dan jarak dihitung dari titik
  publik, jadi pencarian berulang tidak bisa mentrilaterasi titik asli (review C1).

## Endpoint

| Metode + path | Akses | Isi |
|---|---|---|
| `GET /api/health` | publik | Liveness: `{ok, version, commit, demoMode, clients}` (tanpa DB) |
| `GET /api/ready` | publik | DB + semua migrasi terpasang, 503 kalau belum |
| `GET /api/v1/config` | publik | `{ok, appEnv, demoMode, paymentMode, auth:{otpProvider}, push:{vapidPublicKey}\|null, legal:{termsVersion, privacyVersion}}`, `max-age=60` |
| `GET /api/v1/state` | tamu/user | Snapshot per viewer, `ETag` / `If-None-Match` |
| `POST /api/v1/actions/{name}` | user | Aksi web (≥ 40, lihat `internal/service/dispatch.go`), termasuk `confirmDirectPayment` |
| `GET /api/v1/listings/search` | publik | `lat,lng,radiusKm,kategori,min,max,serah,jaminan,penyedia,q,mulai,selesai` → `{hits:[{id,distanceKm}], hiddenByProviderReach, radiusKm}` |
| `GET /api/v1/events` | tamu/user | WebSocket. Cookie, atau `?token=` hanya kalau `SESSION_BEARER_LOGIN` (demo) |
| `POST /api/v1/auth/otp/request`, `POST /api/v1/auth/otp/verify` | publik | OTP; verify membuat akun (+ consent) dan sesi (cookie, atau token kalau bearer diizinkan) |
| `POST /api/v1/auth/demo-login` | publik, `DEMO_MODE` | Sesi bearer untuk persona demo (sesi asli `auth_sessions`) |
| `POST /api/v1/auth/logout`, `GET/DELETE /api/v1/auth/sessions[/{id}]`, `GET /api/v1/auth/me` | user | Sesi |
| `POST /api/v1/photos` | user | Unggah foto (multipart `file, stage, bookingId?, angle?, source`); bukti hanya untuk pihak booking |
| `POST /api/v1/photos/demo` | user | Daftarkan foto contoh `/img/*` |
| `GET\|HEAD /api/v1/photos/{id}` | tanda tangan / sesi berhak | Foto (referensi publik); `X-Content-SHA256`, `X-Original-SHA256` |
| `POST /api/v1/kyc/submissions`, `GET /api/v1/kyc/status` | user | Unggah KTP + selfie, status |
| `GET /api/v1/admin/kyc[/{id}]`, `POST /api/v1/admin/kyc/{id}/decision`, `GET /api/v1/admin/kyc/{id}/files/{ktp\|selfie}`, `POST /api/v1/admin/kyc/blacklist` | admin | Antrean KYC (akses berkas diaudit) |
| `GET /api/v1/push/vapid-key`, `POST/DELETE /api/v1/push/subscriptions` | publik / user | Web push |
| `GET/PUT /api/v1/me/notification-prefs` | user | Preferensi notifikasi + jam tenang |
| `GET /api/v1/me/export` | user | Semua data milik user (JSON, unduhan) |
| `DELETE /api/v1/me` | user | Hapus akun = anonimisasi (409 kalau ada sewa berjalan/sengketa terbuka) |
| `GET /api/v1/admin/metrics` | admin | Booking per status, nilai sewa + komisi "seandainya", sengketa per kategori, funnel request (% direspons < 24 jam), sewa selesai per minggu |
| `POST /api/v1/admin/listings/import[?dryRun=1]` | admin | Impor listing CSV (lihat di bawah) |

Galat selalu `{"ok":false,"error":"<Bahasa Indonesia>"}`. Mutasi lewat cookie wajib `Origin` yang diizinkan (CSRF).
Rate limit per rute: `auth` 10/menit (OTP, demo-login), `upload` 20/menit, `write` 60/menit, `public` 120/menit/IP.

### Impor listing CSV (Early Lender, PRD §18.1)

Baris pertama = judul kolom (urutan bebas, `,` atau `;`). Daftar `|`. Wajib: `owner_phone, title, category,
price_per_day, item_value, lat, lng, handover, collateral, photos` (+ `owner_name` kalau pemilik belum punya akun).
Opsional: `early_lender, description, condition_notes, price_per_week, area_id, radius_km, stock, meeting_point,
late_fee_per_day, cancellation`. Contoh:

```csv
owner_phone,owner_name,early_lender,title,category,price_per_day,item_value,lat,lng,area_id,handover,collateral,photos
0857 1234 5678,Naila Putri,ya,Kebaya brokat hijau,pakaian-acara,40000,900000,-7.7652,110.3752,sendowo,cod,deposit:25%,/img/a.webp|/img/b.webp|/img/c.webp
```

`collateral`: `identitas:KTM/SIM`, `deposit:50000`, `deposit:25%`, `barang:<deskripsi>`. Semua baris divalidasi
dengan aturan yang sama seperti form web; satu baris salah = tidak ada yang disimpan (`422` + galat per baris).
Pemilik baru dibuat dengan verifikasi `belum`, dan listingnya `jeda` sampai pemilik terverifikasi lalu
mengaktifkannya. Deskripsi kolom lengkap: `service.ImportColumns` (juga dikirim di respons galat kolom).

## CLI

```sh
api                              # serve (migrasi otomatis kalau MIGRATE_ON_START=true)
api migrate [up|status]
api admin grant 0812xxxxxxx      # user harus sudah pernah masuk lewat OTP; diaudit (aktor "cli")
api admin revoke 0812xxxxxxx
api vapid gen                    # cetak VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY baru
api healthcheck                  # dipakai HEALTHCHECK Docker
```

Di container: `docker exec <container-api> /app/api admin grant 0812…`.

## Environment

Contoh lengkap: [`deploy/.env.example`](deploy/.env.example). Kolom "Prod" = wajib diisi saat `APP_ENV=production`.

| Nama | Prod | Contoh | Keterangan |
|---|---|---|---|
| `DATABASE_URL` | ya | `postgres://sewain:…@sewain-db:5432/sewain?sslmode=disable` | |
| `APP_ENV` | ya | `production` | `development` \| `demo` \| `staging` \| `production`; nilai lain ditolak |
| `DEMO_MODE` | – | `false` | Login demo, eKYC/bayar simulasi, jam demo. **Ditolak** di produksi |
| `PORT` | – | `8080` | |
| `MIGRATE_ON_START` | – | `true` | |
| `SNAPSHOT_SCOPE` | – | `scoped` | `full` hanya dengan `DEMO_MODE`, ditolak di produksi |
| `SNAPSHOT_USERS` | – | `all` | `referenced` = snapshot non-admin hanya memuat profil user yang dirujuk isinya (pemilik listing, pihak booking, ulasan, papan, ...). Biaya snapshot tidak lagi naik dengan jumlah akun. Lihat [`docs/loadtest/2026-11-report.md`](docs/loadtest/2026-11-report.md) |
| `DATABASE_URL` `?pool_max_conns=N` | – | `max(4, jumlah CPU)` | Ukuran pool pgx (parameter URL bawaan pgx). Saran: 16 di server ini, 8 di VPS 2 vCPU |
| `PAYMENT_MODE` | – | `offline` | `simulated` (default demo, ditolak di produksi) \| `offline` \| `gateway` (belum ada) |
| `CORS_ORIGINS` | ya | `https://sewain.me` | Origin web, pisah koma. `*` ditolak di produksi |
| `ALLOWED_ORIGINS` | – | `https://sewain.me` | Cek CSRF cookie. Default = `CORS_ORIGINS` tanpa `*` |
| `TRUSTED_PROXY_CIDRS` | ya | `172.16.0.0/12` | Proxy yang header IP-nya dipercaya (subnet `gateway-net`) |
| `REQUEST_TIMEOUT` | – | `15s` | Batas per request (WebSocket dikecualikan) |
| `LOG_LEVEL` | – | `info` | |
| `METRICS_ADDR` | – | `127.0.0.1:9090` | `:9090` untuk scrape dari jaringan internal, `off` = mati. Jangan dipublish |
| `RETENTION_INTERVAL` | – | `1h` | Jeda runner retensi |
| `WEB_ORIGIN` | – | `http://sewain-web:3000` | Sumber foto contoh `/img/*` |
| `OTP_PROVIDER` | ya | `zenziva` | `demo` (hanya DEMO_MODE) \| `echo` (staging) \| `zenziva`. demo/echo ditolak di produksi |
| `OTP_PEPPER` | ya (+staging) | `openssl rand -base64 32` | Rahasia HMAC kode OTP |
| `OTP_MAX_PER_PHONE_HOUR` | – | `5` | Kode OTP per nomor per jam |
| `OTP_MAX_PER_IP_HOUR` | – | `200` | Kode OTP per IP (IPv6 per /64) per jam. Tinggi karena NAT kampus |
| `OTP_MAX_SENDS_PER_DAY` | – | `2000` | Plafon global kode terkirim per hari WIB (anti SMS/WA pumping). Tercapai → OTP ditolak sampai 00.00 WIB, log `WARN`, satu notifikasi ke admin |
| `RATE_IP_PER_MIN`, `RATE_IP_BURST` | – | `6000`, `3000` | Rem murah per IP **sebelum** auth (token/cookie palsu tidak memicu lookup sesi). Jauh di atas lalu lintas NAT kampus |
| `RATE_PUBLIC_PER_MIN`, `RATE_PUBLIC_BURST` | – | `1200`, `300` | Batas global semua rute, per user (tamu per IP) |
| `RATE_WRITE_PER_MIN`, `RATE_WRITE_BURST` | – | `120`, `40` | Aksi dan mutasi lain, per user (tamu per IP) |
| `RATE_UPLOAD_PER_MIN`, `RATE_UPLOAD_BURST` | – | `30`, `10` | Unggah foto/KYC, per user |
| `RATE_AUTH_PER_MIN`, `RATE_AUTH_BURST` | – | `60`, `20` | OTP request/verify, per IP |
| `RATE_DEMO_PER_MIN`, `RATE_DEMO_BURST` | – | `600`, `100` | `demo-login` (DEMO_MODE), per IP |
| `SESSION_COOKIE_SECURE` | – | `true` | Default `true` di luar development/demo |
| `SESSION_BEARER_LOGIN` | – | `false` | Token di body + `?token=` WS (demo per tab). Ditolak di produksi |
| `TERMS_VERSION`, `PRIVACY_VERSION` | ya | `2026-12-v1` | Versi S&K/privasi yang dicatat di `consents` |
| `ZENZIVA_WA_URL`, `ZENZIVA_SMS_URL`, `ZENZIVA_USERKEY`, `ZENZIVA_PASSKEY`, `ZENZIVA_*` | ya (zenziva) | | Lihat `docs/integration/auth.md` §1 |
| `KYC_KEYS` | ya (+staging) | `v1:<base64 32 byte>` | Kunci AES-GCM KYC, yang terakhir = aktif |
| `NIK_PEPPER` | ya (+staging) | `openssl rand -base64 32` | HMAC NIK. **Tidak boleh berubah** |
| `MEDIA_BACKEND` | – | `local` | `local` \| `s3` (R2) |
| `UPLOAD_DIR` | – | `/data/uploads` | Root storage lokal |
| `S3_ENDPOINT`/`R2_ACCOUNT_ID`, `S3_BUCKET`, `S3_REGION`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `S3_USE_SSL`, `S3_PREFIX` | ya (s3) | | Alias `R2_*` |
| `S3_RETENTION_ACCESS_KEY_ID`, `S3_RETENTION_SECRET_ACCESS_KEY` | ya (s3) | | Kredensial yang boleh hapus (job retensi) |
| `MEDIA_SIGNING_KEY` | ya (+staging) | `openssl rand -hex 32` | HMAC URL foto |
| `MEDIA_SIGNING_KEY_PREVIOUS` | – | | Kunci lama selama rotasi (≥ 75 menit) |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT` | untuk push | `api vapid gen`, `mailto:ops@sewain.me` | Kosong = push mati |
| `MAIL_PROVIDER` | – | `none` | `none` \| `smtp` \| `resend` \| `log` |
| `MAIL_FROM`, `PUBLIC_URL` | kalau email | `Sewain <noreply@sewain.me>`, `https://sewain.me` | |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_SECURITY` | kalau smtp | | |
| `RESEND_API_KEY` | kalau resend | | |
| `NOTIFY_PREFS_PATH` | – | `/profil/notifikasi` | Tautan preferensi di email |

`CLIENT_IP_HEADER` tidak dipakai lagi: IP klien selalu dari `TRUSTED_PROXY_CIDRS`.

Batas `RATE_*` dan `OTP_MAX_*`: kosong atau `0` = default; nilai lain yang bukan bilangan bulat positif membuat API
gagal start. Default dipilih untuk **NAT kampus** (ratusan mahasiswa di balik satu IP Wi-Fi UGM): batas global
dihitung per user yang sudah masuk, bukan per IP. `RATE_*` dihitung di memori **per instance** (N instance = N kali
lipat); `OTP_MAX_*` dihitung di database. Alasan lengkap: [`docs/integration/ops.md`](docs/integration/ops.md) §2.

### Kunci

```sh
openssl rand -base64 32                    # OTP_PEPPER, NIK_PEPPER, tiap kunci KYC_KEYS (v1:<nilai>)
openssl rand -hex 32                       # MEDIA_SIGNING_KEY
docker run --rm ghcr.io/mrfroglab/sewain-api:develop vapid gen   # atau: scripts/go.sh run ./cmd/api vapid gen
```

Simpan setiap kunci di dua tempat (`.env` server + password manager). Rotasi: [`deploy/OPS.md`](deploy/OPS.md).
Di `development`/`demo` kunci yang kosong diganti kunci acak sementara (data terenkripsi dan URL foto tidak berlaku
lagi setelah restart).

## Menjalankan lokal

Go tidak wajib terpasang: `scripts/go.sh` menjalankan Go 1.27.4 di Docker.

```sh
make db-up                                  # Postgres+PostGIS dev (docker-compose.dev.yml)
cp .env.example .env                        # DEMO_MODE=true, APP_ENV=development
make run                                    # http://localhost:8080/api/health

export TEST_DATABASE_URL=$(scripts/test-db.sh up)   # Postgres sekali pakai per worktree
scripts/go.sh vet ./... && scripts/go.sh test ./...  # unit + integrasi + E2E HTTP (internal/app)
scripts/test-db.sh down
make generate                               # sqlc (image dipin) setelah mengubah internal/db/queries
```

Lint seperti CI:

```sh
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
  -e GOLANGCI_LINT_CACHE=/cache/lint -e GOFLAGS=-buildvcs=false -v sewain-go-cache:/cache \
  -v "$PWD":/src -w /src golangci/golangci-lint:v2.15.0 golangci-lint run ./...
```

## Migrasi

goose, satu folder `internal/db/migrations` (di-embed), `allow out-of-order`, advisory lock. Rentang: inti
`00001–00099`, auth `00100–00119`, media `00120–00139`, notify `00140–00159`, ops `00160–00179`, pembayaran
`00200–00249`. Migrasi `00001` mengganti skema JSONB v1 kalau masih ada (DB staging lama).

## Deploy

CI (`.github/workflows/deploy.yml`): `main` → image `:latest` → stack demo, `develop` → `:develop` → staging,
setiap build juga diberi tag `sha-<commit>` untuk rollback. Langkah, rollback, restore, rotasi kunci, insiden data:
[`deploy/OPS.md`](deploy/OPS.md).

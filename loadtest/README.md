# Uji beban Sewain v2 (k6)

Skrip uji beban ROADMAP F5.5. Hasil dan analisis: [`docs/loadtest/2026-11-report.md`](../docs/loadtest/2026-11-report.md).

| Berkas | Isi |
|---|---|
| `prepare.js` | Membuat pool akun lewat DEMO_MODE: `registerUser` → `demo-login` → `submitVerification` (eKYC demo), plus penyedia dengan satu listing masing-masing. Hasil: `$OUT/pool.json` |
| `main.js` | Campuran: **browse** 70% (tamu + user: snapshot dengan `If-None-Match`, pencarian bervariasi, config, WebSocket singkat), **rent** 25% (`createBooking` → `acceptBooking` sebagai penyedia → `payBooking` → `sendMessage`, semua dengan `Idempotency-Key`), **upload** 5% (foto bukti 125 KB ke booking sendiri). Tahapan 0 → 100 → 300 VU, tahan 3 menit, turun |
| `campus-nat.js` | 300 VU dari **satu IP** (NAT Wi-Fi kampus), pola wajar, 10% tamu. Lulus kalau 429 = 0 |
| `lib/common.js`, `lib/summary.js` | Header (Host, `CF-Connecting-IP` acak dari 100.64.0.0/10 per VU), helper aksi, tabel ringkasan per endpoint |
| `run.sh` | Runner: penjaga demo, `docker stats` + `pg_stat_activity` tiap 5 detik, metrik Prometheus sebelum/sesudah, k6 |
| `analyze.py` | Deret waktu (VU, req/s, p95 per grup, gagal, 429), puncak container, delta pool pgx/job (stdlib saja) |
| `stack/docker-compose.yml` | Stack sementara `sewain-lt-*` (Postgres + API image lokal, batas memori sama dengan staging, port hanya 127.0.0.1) |
| `fixtures/bukti.jpg` | Foto bukti 640×480, 125 KB (noise sintetis, tanpa EXIF) |

Alat: `grafana/k6:2.3.0` dan `curlimages/curl:8.11.1` (image Docker, versi dipin), `--network host`.

## Aturan aman (server yang sama menjalankan demo final)

- Tembak staging **lewat Caddy lokal**: `BASE_URL=http://127.0.0.1` + header `Host: sewain-staging.hallojanu.me`,
  bukan lewat Cloudflare. Caddy ada di `gateway-net`, yang dipercaya API (`TRUSTED_PROXY_CIDRS`), jadi
  `CF-Connecting-IP` dipakai sebagai IP klien.
- `run.sh` menjalankan **penjaga demo**: `curl https://sewain.hallojanu.me/api/health` tiap 5 detik. Kalau 3 kali
  berturut-turut > 1 detik atau gagal, container k6 (`sewain-lt-k6`) langsung di-`docker kill` dan
  `$OUT/GUARD_TRIPPED` ditulis (exit 3).
- Tiap skenario ≤ 5 menit. Maksimal 3 skenario berat berturut-turut, lalu jeda 2 menit.
- Staging dipakai bersama. `resetDemo` atau deploy ulang di tengah uji menghapus akun pool. k6 berhenti sendiri
  setelah 5 respons 401/403 pada sesi pool, lalu jalankan `prepare` lagi.

## Menjalankan ulang

```sh
export OUT=$(mktemp -d)        # hasil: pool.json, *-summary.txt/json, *-k6.csv.gz, *-stats.csv, *-pg.csv, *-guard.csv
loadtest/run.sh prepare        # USERS=320 PROVIDERS=40 (default)
loadtest/run.sh main           # SCALE=1 → 300 VU puncak, HOLD=3m
# jeda ≥ 2 menit kalau sudah 3 skenario berat berturut-turut
loadtest/run.sh campus-nat
python3 -I loadtest/analyze.py "$OUT" main 20
python3 -I loadtest/analyze.py "$OUT" campus-nat 30
loadtest/run.sh reset          # demo-login u-admin → resetClock + resetDemo (menghapus akun pool)
rm -rf "$OUT"
```

Variabel: `SCALE` (1 = 300 VU; 3 = 900 VU untuk uji stres), `HOLD`, `NAME` (prefiks berkas hasil), `USERS`/`PROVIDERS`
(ukuran pool; harus ≥ jumlah VU puncak supaya tiap VU punya akun sendiri), `API_CONTAINER`/`DB_CONTAINER`
(container yang diukur), `GUARD=off` (jangan dipakai di server ini).

### Stack sementara untuk build lokal

Staging menjalankan `develop`. Untuk mengukur build branch sendiri:

```sh
docker build -t sewain-lt-api:local .
export OUT=$(mktemp -d) BASE_URL=http://127.0.0.1:18480 API_CONTAINER=sewain-lt-api DB_CONTAINER=sewain-lt-db
API_IMAGE=sewain-lt-api:local loadtest/run.sh stack-up       # rahasia acak di $OUT/lt.env (izin 600)
# opsional: DB_URL_PARAMS='&pool_max_conns=16' untuk ukuran pool pgx
loadtest/run.sh prepare && loadtest/run.sh main
loadtest/run.sh stack-down && docker image rm sewain-lt-api:local
```

Data Postgres stack ini ada di tmpfs, sehingga memori container DB ikut menghitung isi database. Angka memori DB di
stack ini lebih tinggi daripada staging, yang memakai volume.

### Benchmark snapshot

```sh
TEST_DATABASE_URL=$(scripts/test-db.sh up) scripts/go.sh test ./internal/service -run '^$' -bench Snapshot -benchmem
```

Mengukur build snapshot dan `json.Marshal` dengan 3.000 user (skala pilot PRD §15.2).

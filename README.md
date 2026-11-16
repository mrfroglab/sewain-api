# sewain-api

Backend **Sewain** (PRD §13): Go + Postgres/PostGIS. Server ini sumber data tunggal untuk web, sehingga penyewa,
penyedia, dan admin bisa memakai perangkat berbeda.

- Live (demo, v1): https://sewain.hallojanu.me/api/health · staging (v2): https://sewain-staging.hallojanu.me/api/health
- Stack: Go 1.27 (`net/http`, `pgx/v5`, `coder/websocket`), Postgres 16 + PostGIS 3.4, goose (migrasi), sqlc (query
  bertipe), image distroless
- Rencana: [`ROADMAP.md`](../ROADMAP.md) §2–§5. Titik integrasi untuk modul auth/media/notify:
  [`docs/integration/core-v2.md`](docs/integration/core-v2.md)

## Arsitektur v2

```
[sewain-web] ──HTTPS/JSON──► httpapi ──► service ──► Postgres + PostGIS
     ▲                         │  (satu transaksi per aksi, FOR UPDATE, change_log → version)
     └── WebSocket /events ◄───┘──► PhotoStore (disk lokal; R2 lewat modul media)
```

| Paket | Isi |
|---|---|
| `internal/domain` | Aturan bisnis murni tanpa state: harga, komisi, saran jaminan, hash kesepakatan, validasi listing, kata terlarang, grid lokasi publik, tipe JSON (cermin `sewain-web/src/lib/types.ts`) |
| `internal/service` | Semua aksi sebagai transaksi Postgres: kunci baris (`SELECT … FOR UPDATE`) pada booking/listing/sengketa yang diubah, retry otomatis untuk serialization failure/deadlock (maks 3), notifikasi in-app + pesan sistem + audit di transaksi yang sama. Juga snapshot per viewer, job berkala, idempotency, pencarian, seed/reset demo |
| `internal/db` | Pool pgx, `WithTx`, advisory lock, migrasi goose (embed, jalan saat start dengan advisory lock, out-of-order diizinkan), query sqlc di `internal/db/dbq` |
| `internal/httpapi` | Handler HTTP (kontrak v1), `ViewerResolver` (token demo sekarang), `PhotoStore` (disk lokal sekarang), hub WebSocket |
| `internal/seed` | `seed.json` dari web, digeser ke "sekarang" |

Prinsip penting:

- **Satu aksi = satu transaksi.** Aturan bentrok jadwal (FR-R4, stok akun Toko) dihitung setelah baris listing dikunci,
  jadi `createBooking`/`acceptBooking` paralel tidak pernah melebihi stok. `acceptBooking` sekarang juga memeriksa stok.
- **Versi** = `max(change_log.version)`, dibaca di transaksi snapshot yang sama, sehingga versi yang dilaporkan selalu
  sudah terlihat datanya. Setiap commit yang mengubah data menulis satu baris `change_log` (audiens: user terkait, admin,
  publik) lalu memanggil listener (`service.Change`).
- **Urutan koleksi** di snapshot sama dengan v1 (notifikasi/audit terbaru di depan, pesan dari terlama, …) lewat kolom
  `ord` dari satu sequence global.
- **Tabel bukti append-only:** trigger menolak UPDATE/DELETE/TRUNCATE pada `photos` dan `audit_log` kecuali transaksi
  menyetel `SET LOCAL sewain.retention = 'on'` (job retensi, reset demo). Pesan chat tidak bisa dihapus selama booking
  berjalan/bersengketa (FR-C3).
- **Job berkala** tiap 15 detik dengan advisory lock (aman banyak instance): kadaluarsa 24 jam, flag telat, selesai
  otomatis 48 jam setelah jaminan dikembalikan, sengketa L1 → L2 setelah 48 jam, rilis ulasan 7 hari (FR-T2). Jam
  bisa disuntik (`service.Options.Now`).
- **Analitik:** view `analytics_bookings_by_status`, `analytics_rent_value` (termasuk komisi "seandainya aktif"),
  `analytics_disputes_by_case`, `analytics_request_funnel`, `analytics_weekly_completed` (North Star).

### Snapshot per viewer (`SNAPSHOT_SCOPE`)

| Data | Tamu | User | Admin / `full` |
|---|---|---|---|
| Listing | Aktif, lokasi dibulatkan ke grid ± 250 m | + milik sendiri (semua status, titik presisi) + listing dari booking sendiri | Semua |
| Profil user | Publik: nama depan/nickname, tipe akun, toko, area, kampus, status verifikasi, rating, tanggal gabung | + profil sendiri lengkap | Semua |
| HP, email, NIK, strike | Tidak | Milik sendiri | Ya |
| Booking, pesan, foto bukti, sengketa | Tidak | Booking tempat ia jadi pihak (+ foto yang ia unggah) | Semua |
| Ulasan | Yang sudah dirilis (FR-T2) | + tulisan sendiri | Semua |
| Notifikasi, favorit, chatReads, laporan, strike, verifikasi | Tidak | Milik sendiri | Semua |
| Papan permintaan | Semua post, tanpa tawaran | + tawaran di post sendiri / tawaran sendiri | Semua |
| Komisi, aturan jaminan, kata terlarang | Ya | Ya | Ya |
| Audit log | Tidak | Tidak | Ya |

`scoped` adalah default dan satu-satunya yang boleh di produksi. `full` (persis v1) hanya diterima bersama
`DEMO_MODE=true`. Respons membawa `ETag`; `If-None-Match` yang cocok → `304`.

> Web saat ini mengambil `/api/v1/state` tanpa token, jadi dengan `scoped` web melihat tampilan tamu sampai worker
> `web-v2` (F6.3) mengirim token/cookie. Staging yang menjalankan web lama perlu `SNAPSHOT_SCOPE=full` sementara.

## Endpoint

| Method | Path | Isi |
|---|---|---|
| GET | `/api/health` | Status server + database |
| GET | `/api/v1/state` | Snapshot `DataState` per viewer + `serverNow`, `ETag` |
| POST | `/api/v1/auth/demo-login` | `{userId}` → token sesi (khusus DEMO_MODE; yang disimpan hanya hash SHA-256) |
| GET | `/api/v1/auth/me` | User dari token |
| POST | `/api/v1/actions/{name}` | `{args:[...], id?}` → `{ok, error?, value?, version}`. Aktor (argumen pertama) wajib pemilik sesi (403). Header opsional `Idempotency-Key` |
| POST | `/api/v1/photos` | multipart `file, stage, bookingId, angle, source, width, height` → PhotoRecord (hash + timestamp dari server) |
| POST | `/api/v1/photos/demo` | `{url, stage, bookingId?, angle?}`: daftarkan foto contoh `/img/*` (di-hash server) |
| GET | `/api/v1/photos/{id}` | Berkas foto + header `X-Content-SHA256` |
| GET | `/api/v1/listings/search` | `lat, lng, radiusKm` + opsional `q` (full-text), `kategori, min, max, serah, jaminan, penyedia, mulai, selesai` |
| GET | `/api/v1/events` | WebSocket: `{"type":"hello"\|"changed","version":n}` |

Aksi: registerUser, updateUser, submitVerification, reviewVerification, createListing, updateListing,
setListingStatus, toggleFavorite, createBooking, acceptBooking, declineBooking, cancelBooking, payBooking,
submitCheckout, addCheckoutPhotos, agreeCheckout, rejectAtCheckout, submitCheckin, returnCollateral, confirmCollateral,
submitReview, sendMessage, markThreadRead, markNotificationsRead, openDispute, proposeSolution, respondProposal,
escalateDispute, adminRecommend, resolveDispute, createReport, resolveReport, addStrike, setUserStatus,
updateCommission, updateCollateralRules, setBannedKeywords, createWanted, offerWanted, closeWanted. Kontrol demo
(DEMO_MODE, user mana pun yang login): advanceClock, resetClock, resetDemo.

**Idempotency-Key:** aksi dengan kunci yang sama (per user) dijalankan sekali. Kirim ulang → respons yang sama persis
(header `Idempotency-Replayed: true`), termasuk penolakan aturan. Kunci yang sama untuk permintaan lain → `422`.
Kunci dibersihkan setelah 24 jam.

**Pencarian:** aturan radius dua arah `ST_DWithin(location, titik, LEAST(radius_penyewa, radius_penyedia))` (FR-S3),
full-text `simple` (judul + deskripsi + kategori, cocok awalan kata), filter tanggal `mulai`/`selesai` yang menghitung
stok (booking ACCEPTED/ACTIVE yang bertumpuk < stok). Urut jarak lalu relevansi. Jarak dibulatkan ke 100 m (FR-S6).

## Menjalankan

Go tidak perlu terpasang: `scripts/go.sh` menjalankan Go 1.27 di Docker (cache bersama).

```bash
cp .env.example .env
make db-up                 # Postgres + PostGIS lokal di 127.0.0.1:55491
make run                   # migrasi (advisory lock) + seed demo otomatis kalau DB kosong dan DEMO_MODE=true
scripts/go.sh run ./cmd/api migrate status   # atau: make migrate-status
scripts/go.sh run ./cmd/api migrate up       # migrasi saja lalu keluar (MIGRATE_ON_START=false untuk mematikan di start)
make generate              # sqlc generate (image sqlc/sqlc:1.30.0); commit hasil di internal/db/dbq
make generate-check        # gagal kalau kode sqlc yang di-commit basi (untuk CI)
```

Tes:

```bash
make test                                  # tanpa DB: tes integrasi di-skip
export TEST_DATABASE_URL=$(scripts/test-db.sh up)   # Postgres sekali pakai per worktree
scripts/go.sh test -count=1 ./...          # atau: make test-integration
scripts/test-db.sh down
scripts/compare-v1.sh                      # bandingkan /api/v1/state + pencarian v1 vs v2 di seed yang sama,
                                           # lalu start v2 di atas DB berskema v1
```

Setiap tes integrasi memakai database baru (salinan dari template yang sudah dimigrasi). Isinya: port tes engine v1,
tes per aksi (sukses + ditolak), siklus HTTP lengkap ala `sewain-web/e2e/api.mjs`, privasi snapshot, idempotency,
konkurensi stok (≥ 20 goroutine), job dengan jam palsu, pencarian, migrasi `00001` di atas skema v1, trigger
append-only.

Web lokal ke API ini: `NEXT_PUBLIC_API_URL=http://localhost:8080 pnpm dev` di `sewain-web`.

### Environment

| Variabel | Default | Isi |
|---|---|---|
| `PORT` | `8080` | Port HTTP |
| `DATABASE_URL` | (wajib) | DSN Postgres |
| `APP_ENV` | `development` | `demo`, `development`, `staging`, `production` |
| `DEMO_MODE` | `false` | Login demo, jam demo, reset, seed otomatis. **Ditolak** bersama `APP_ENV=production` |
| `SNAPSHOT_SCOPE` | `scoped` | `scoped` (per viewer) atau `full` (seperti v1, hanya dengan `DEMO_MODE=true`) |
| `MIGRATE_ON_START` | `true` | Jalankan migrasi goose saat start |
| `UPLOAD_DIR` | `./data/uploads` | Folder foto (`LocalPhotoStore`) |
| `WEB_ORIGIN` | `http://localhost:3000` | Asal `/img/*` untuk foto contoh |
| `CORS_ORIGINS` | `http://localhost:3000` | Daftar origin, dipisah koma |

Data demo (`internal/seed/seed.json`) diekspor dari seed web: `make seed-export`. Saat seeding semua timestamp dan
tanggal digeser ke "sekarang"; jam demo (`clockOffset`) disimpan di `app_settings`. `resetDemo` = truncate + seed
ulang dalam satu transaksi (token sesi tetap berlaku, versi tetap naik).

### Migrasi

Berkas goose di `internal/db/migrations/NNNNN_nama.sql` (`-- +goose Up/Down`). Rentang nomor: inti `00001–00099`,
auth `00100–00119`, media `00120–00139`, notify `00140–00159`, ops `00160–00179`, pembayaran `00200–00249`.
`00001` mendeteksi skema v1 (tabel JSONB dengan kolom `data`) dan men-drop tabel v1 sebelum membuat skema v2; data demo
lalu di-seed ulang.

## Deploy

Push ke `main` → GitHub Actions menjalankan gofmt, vet, test, dan build → image `ghcr.io/mrfroglab/sewain-api` →
runner self-hosted menjalankan pull dan restart container `sewain-api` di `/opt/stacks/apps/sewain` (db + api + web).
`develop` → stack staging. Caddy meneruskan `/api/*` ke API dan sisanya ke web. Setup awal atau darurat:
`deploy/deploy.sh`.

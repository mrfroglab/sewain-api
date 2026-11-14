# sewain-api

Backend **Sewain** (PRD §13): Go + Postgres/PostGIS. Server ini jadi sumber data tunggal untuk prototipe, sehingga
penyewa, penyedia, dan admin bisa memakai perangkat berbeda saat demo.

- Live: https://sewain.hallojanu.me/api/health (dipakai oleh [`sewain-web`](https://github.com/mrfroglab/sewain-web))
- Stack: Go 1.27 (`net/http`, `pgx/v5`, `coder/websocket`), Postgres 16 + PostGIS 3.4, image distroless

## Arsitektur

```
[sewain-web (Next.js PWA)] ──HTTPS/JSON──► [sewain-api (Go)] ──► [Postgres + PostGIS]
          ▲                                      │
          └────── WebSocket /api/v1/events ──────┤──► /data/uploads (foto bukti; R2 di produk)
```

- **Domain** (`internal/domain`): aturan bisnis di-porting 1:1 dari store web, yaitu:
  - state machine booking REQUESTED → ACCEPTED → ACTIVE → RETURNED → COMPLETED, ditambah DECLINED, EXPIRED, CANCELLED, DISPUTED, RESOLVED, dan flag OVERDUE
  - kesepakatan sewa ber-hash SHA-256
  - jaminan dan saran jaminan, komisi (persen/flat, min/maks, penanggung)
  - sengketa L1 → L2 → L3, moderasi, papan permintaan, log audit
- **Engine:** data dipegang di memori di bawah satu mutex. Setiap aksi mencatat baris yang berubah, lalu baris itu langsung
  di-*upsert* ke Postgres (write-through) sebelum respons dikirim. Setelah itu event `changed` dikirim ke semua klien
  lewat WebSocket.
- **Penyimpanan** (`internal/store`): satu tabel per entitas. Dokumen disimpan dalam JSONB dengan bentuk yang sama
  dengan tipe di web. Kolom filter/sort/join dibuat sebagai *generated column* yang terindeks. Tabel `listings` punya
  kolom `geography(Point,4326)` dengan index GiST.
- **Pencarian radius dua arah** (FR-S3): `ST_DWithin(location, :titik, LEAST(:radius_penyewa, radius_penyedia))`.
- **Foto bukti** (FR-P1..P4): hash SHA-256 dihitung dari byte yang diterima server, timestamp dari jam server, dan
  datanya append-only. File disajikan dengan header `X-Content-SHA256`. Foto KTP/selfie simulasi tidak pernah dikirim
  ke server (PRD §7.2).
- **Job berkala** (setiap 15 detik): kadaluarsa 24 jam, flag telat, selesai otomatis 48 jam setelah jaminan
  dikembalikan, dan sengketa L1 yang naik otomatis ke L2.

## Endpoint

| Method | Path | Isi |
|---|---|---|
| GET | `/api/health` | Status server + database |
| GET | `/api/v1/state` | Snapshot seluruh data (bentuk `DataState` web) + `serverNow` |
| POST | `/api/v1/auth/demo-login` | `{userId}` → token sesi (khusus DEMO_MODE; OTP disimulasikan) |
| GET | `/api/v1/auth/me` | User dari token |
| POST | `/api/v1/actions/{name}` | `{args:[...], id?}`: menjalankan aksi domain. Aktor (argumen pertama) wajib sama dengan pemilik token |
| POST | `/api/v1/photos` | multipart `file, stage, bookingId, angle, source` → PhotoRecord (hash + timestamp dari server) |
| POST | `/api/v1/photos/demo` | `{url, stage, ...}`: daftarkan foto contoh `/img/*` (di-hash server) |
| GET | `/api/v1/photos/{id}` | File foto bukti |
| GET | `/api/v1/listings/search` | `lat, lng, radiusKm, q, kategori, min, max, serah, jaminan, penyedia` → hasil PostGIS |
| GET | `/api/v1/events` | WebSocket: `{"type":"changed","version":n}` |

Aksi yang tersedia: registerUser, updateUser, submitVerification, reviewVerification, createListing, updateListing,
setListingStatus, toggleFavorite, createBooking, acceptBooking, declineBooking, cancelBooking, payBooking,
submitCheckout, addCheckoutPhotos, agreeCheckout, rejectAtCheckout, submitCheckin, returnCollateral, confirmCollateral,
submitReview, sendMessage, markThreadRead, markNotificationsRead, openDispute, proposeSolution, respondProposal,
escalateDispute, adminRecommend, resolveDispute, createReport, resolveReport, addStrike, setUserStatus,
updateCommission, updateCollateralRules, setBannedKeywords, createWanted, offerWanted, closeWanted. Ditambah kontrol
demo advanceClock, resetClock, dan resetDemo.

## Menjalankan

```bash
cp .env.example .env
make db-up        # Postgres + PostGIS lokal di 127.0.0.1:55491
make run          # migrasi + seed demo otomatis saat database kosong
make test         # unit test domain (siklus sewa, otorisasi, komisi, kadaluarsa, …)
```

Web lokal ke API ini: `NEXT_PUBLIC_API_URL=http://localhost:8080 pnpm dev` di `sewain-web`. Kalau API tidak bisa
dijangkau, web otomatis kembali ke mode offline (data di browser).

Environment variable (lihat `.env.example`): `PORT`, `DATABASE_URL`, `APP_ENV`, `DEMO_MODE`, `UPLOAD_DIR`,
`WEB_ORIGIN`, `CORS_ORIGINS`.

**DEMO_MODE** mengaktifkan login demo, kontrol jam demo, dan reset data. Server **menolak start** kalau
`DEMO_MODE=true` dipakai bersama `APP_ENV=production` (tabel risiko PRD).

Data demo (`internal/seed/seed.json`) diekspor dari seed web supaya kedua sisi identik:
`make seed-export`. Saat seeding, semua timestamp dan tanggal digeser ke "sekarang".

## Deploy

Push ke `main` → GitHub Actions menjalankan gofmt, vet, test, dan build → image `ghcr.io/mrfroglab/sewain-api` →
runner self-hosted menjalankan pull dan restart container `sewain-api` di `/opt/stacks/apps/sewain` (db + api + web).
Caddy meneruskan `/api/*` ke API dan sisanya ke web. Setup awal atau darurat: `deploy/deploy.sh`.

## Batasan prototipe

- Login memakai token demo; OTP, eKYC, dan pembayaran masih simulasi.
- Snapshot `/api/v1/state` berisi seluruh data demo. Di produk, data dibatasi per pengguna.
- Satu instance (state di memori + write-through). Untuk skala besar, query dipindah langsung ke database (sqlc).
- Foto disimpan di volume lokal. Di produk dipindah ke Cloudflare R2 (bucket write-once).

# Integrasi gelombang 2 (worker `integrate-v2`)

Semua paket gelombang 1 dipasang ke proses API. Wiring ada di `internal/app` (dipakai `cmd/api` dan tes E2E),
rute di `internal/httpapi`, logika inti di `internal/service`. Dokumen ini mencatat keputusan, perubahan pada paket
milik worker lain, dan hal yang masih terbuka. Cara pakai dan env: `README.md`; operasional: `deploy/OPS.md`.

## Yang dipasang

| Area | Pemasangan |
|---|---|
| Platform | Rantai `ops.md` §1 (request ID → RealIP → log → recover → header → CORS → limit publik → batas body → timeout → auth). Profil per rute di `httpapi.Routes`. Server metrics di `METRICS_ADDR`. `/api/ready` = ping + tidak ada migrasi tertunda (`db.SchemaVersion`). `server.Run` untuk API + metrics, hub ditutup lewat `RegisterOnShutdown`, runner latar dihentikan setelah server. `/api/health` = liveness + `version/commit/demoMode/clients`. |
| Auth | `app.userDirectory` di atas repo inti (nomor E.164 → format inti `08…`). `ViewerResolver`/`DemoTokens` v1 dihapus: semua handler membaca `auth.ViewerFrom`. Demo login membuat sesi `auth_sessions` (migrasi `00002` menghapus `demo_sessions`). Subcommand `admin grant/revoke`. |
| KYC | Hook `OnSubmitted`/`OnDecided` → `service.KYCSubmitted/KYCDecided` di transaksi KYC. `reviewVerification` untuk pengajuan KYC asli memanggil `kyc.Decide`. `setUserStatus(blacklist)` memasukkan NIK ke `nik_blacklist` dan mencabut sesi di transaksi aksi. |
| Media | `media.Storage` (`MEDIA_BACKEND`), `Inspect`, `StripGPS` untuk `referensi`, `media.Handler` untuk `GET/HEAD /api/v1/photos/{id}` (tanda tangan atau sesi pihak booking), `src` snapshot = `SignFor` per viewer, `geo.FuzzPoint` untuk lokasi publik listing dan papan. |
| Realtime/notify | `realtime.Hub` + `Publisher` (setelah commit) + `PublishTx` (transaksi milik modul lain) + `Listener`. `notify.Enqueue` di `txn.notify`. Dispatcher + gauge outbox di runner. `/api/v1/push/*`, `/api/v1/me/notification-prefs`. `api vapid gen`. |
| Retensi | `App.RunRetention` (advisory lock sesi `0x5e3a0003`), lihat `OPS.md` §6. |
| Endpoint baru | `config`, `me/export`, `DELETE me`, `admin/metrics`, `admin/listings/import`, aksi `confirmDirectPayment`, `PAYMENT_MODE`. |
| Kebutuhan web | Semua butir "Keputusan koordinator" di `sewain-web-v2-api-needs.md` (lihat tes `TestSnapshotFields`, `TestIdempotencyBodyKey`, `TestConfigEndpoint`). |
| Review keamanan | C1 (pencarian), I1, K2, L2, L4, L5 dari `docs/security/review-wave1.md`, plus I2 (`slog.SetDefault`). |

## Keputusan

| Keputusan | Alasan |
|---|---|
| Paket `internal/app` memegang wiring, `cmd/api` tipis | Tes E2E memakai jalur yang sama dengan produksi (`apptest`), tanpa menduplikasi wiring. |
| Notifikasi ke `u-admin` (konvensi v1) dikirim ke semua akun ber-role admin | Di luar seed demo `u-admin` tidak ada, sehingga FK gagal. Ini bug nyata: job L1→L2 dan laporan akan gagal di produksi. |
| `registerUser` dan `submitVerification` simulasi hanya di `DEMO_MODE` | Di luar demo keduanya membuat akun tanpa OTP / verifikasi tanpa KTP. |
| Unggah foto ke booking hanya oleh pihak booking (atau admin) | v1 tidak memeriksa, siapa pun bisa menempel foto ke booking orang lain. |
| `markThreadRead` hanya untuk pihak booking/admin, event ke kedua pihak | `chatReads` kedua pihak kini ada di snapshot (web #7). |
| Snapshot `scope=full` (demo): bukti yang tidak boleh dilihat viewer ditandatangani sebagai penyewa (salinan ber-watermark) | Mode `full` = perilaku v1 (semua terlihat) untuk web lama yang mengambil snapshot sebagai tamu. Hanya demo, ditolak di produksi. |
| ETag snapshot ikut jendela 15 menit | URL foto bertanda tangan berubah tiap 15 menit; tanpa ini 304 bisa mengembalikan URL kedaluwarsa. |
| `Idempotency-Key` di body (`idempotencyKey`) diterima, header menang; hash permintaan = argumen + id klien | Keputusan koordinator #10. Kunci bisa pindah dari body ke header saat retry tanpa dianggap permintaan lain. Kunci sama + argumen lain → 409 (sebelumnya 422). |
| `reviewVerification` untuk pengajuan KYC tidak menyimpan kunci idempotensi | Keputusan berjalan di transaksi KYC; `kyc.Decide` menolak keputusan kedua, jadi retry aman. |
| Pencarian: radius 1/3/5/10/25 km, keanggotaan + jarak + jangkauan penyedia dari titik publik | Review C1. Jangkauan penyedia juga dari titik publik (bukan asli) karena titik cari dipilih penyerang, jadi boolean pun bisa jadi oracle. Prefilter SQL memakai titik asli + margin 500 m (> fuzz 400 m), sehingga hasil hanya fungsi titik publik. Tes trilaterasi membuktikan estimasi jatuh di titik publik (> 150 m dari asli). |
| `ListUsers` memuat akun terhapus | Booking/ulasan pihak lawan tetap merujuk; profilnya "Pengguna terhapus". |
| Listing publik hanya milik pemilik `aktif` | Keputusan koordinator #3 (selain `status` di profil). |
| Hapus akun = anonimisasi di satu transaksi inti, lalu `auth.RevokeAllSessions` + `kyc.EraseUser`; gagal erase diulang runner retensi | Review K2. Ditolak kalau ada booking `REQUESTED/ACCEPTED/ACTIVE/RETURNED/DISPUTED` atau sengketa terbuka. Consent dipertahankan (bukti persetujuan). |
| Ekspor tanpa pesan pihak lawan | "Tanpa data pihak lain kecuali nama publik": pesan yang ditulis pihak lawan adalah data mereka. Pesan sendiri + pesan sistem ikut. |
| Impor CSV: semua atau tidak sama sekali; listing pemilik belum terverifikasi berstatus `jeda` | Admin memperbaiki file lalu unggah ulang. Pemilik yang belum verifikasi tidak bisa menerima booking (`CanTransact`), jadi listing aktif hanya akan menolak penyewa. |
| `PAYMENT_MODE=offline`: hanya metode `langsung`; `simulated` ditolak di produksi, `gateway` ditolak saat start | D12. |
| Sesi demo bertahan setelah `resetDemo` (disalin ke tabel sementara lalu dikembalikan untuk user yang ada lagi) | Perilaku v1: tab web menyimpan token. |
| IP klien untuk auth dari resolver tepercaya; `CLIENT_IP_HEADER` diabaikan | Review A3. `clientAddr` juga mengisi `RemoteAddr`. |
| `?token=` WebSocket hanya kalau `SESSION_BEARER_LOGIN` | Keputusan koordinator (tambahan). |
| `config.LoadFrom` mengumpulkan semua galat | Operator memperbaiki env sekali jalan; tes startup memeriksa pesannya. |
| Jarak pencarian dibulatkan `geo.RoundDistanceKm` | `media.md` §7. |
| Runner retensi pakai jam demo untuk baris berstempel jam demo (booking, notifikasi) dan jam nyata untuk sesi/OTP/KYC/outbox | Lompatan jam demo berperilaku seperti waktu berjalan. |

## Perubahan di paket worker lain (untuk merge hati-hati)

| Paket | Perubahan |
|---|---|
| `internal/notify/http.go` | Review L2: `SaveSubscription` hanya memindahkan endpoint ke akun lain kalau kunci `p256dh/auth` sama; selain itu `ErrSubscriptionTaken` → 409. Tes di `http_test.go`. |
| `internal/auth` | Tidak diubah. `auth.GrantAdmin` tidak dipakai (mencari `phone` dalam E.164 sementara inti menyimpan `08…`, dan fixture tesnya tanpa `deleted_at`); CLI memakai `service.SetRoleByPhone` (format inti, `deleted_at IS NULL`, diaudit). |
| `internal/db` | Migrasi inti `00002` (hapus `demo_sessions`, indeks), `LockRetention`, `WithSessionLock`, `LatestMigration`, `SchemaVersion`. Query `ListUsers`, `ListListingsVisibleTo`, `SearchListings` diubah; `dbq` diregenerasi (sqlc 1.30.0). |
| `.golangci.yml` | Pengecualian `TODO(core-v2)` dihapus; tidak ada pengecualian baru. Empat `#nosec` inline dengan alasan (`G704` healthcheck loopback, `G705` snapshot JSON, 2× `G120` form dengan `MaxBytesReader`). |

## Terbuka

- Web: tombol "Sudah bayar langsung" + konfirmasi penyedia, unduh ekspor, hapus akun, halaman impor CSV admin.
  Login OTP + KYC unggah (F6.1/F6.2) memakai endpoint yang sudah ada.
- Job re-enkripsi `KYC_KEYS` lama (`KeyRing.Rewrap`), endpoint terima ulang S&K versi baru (`auth.md` §8).
- Rate limit in-memory per instance (ops I4); Postgres/Redis sebelum multi-instance.
- `deploy/docker-compose.yml` masih menggambarkan stack demo v1; stack staging/produksi v2 perlu `env_file: .env`.
- `scripts/compare-v1.sh` membandingkan byte snapshot v1/v2: field baru (`scope`, `viewerId`, `listingBusy`,
  `publicStats`, `visibleAt`) adalah tambahan yang disengaja.

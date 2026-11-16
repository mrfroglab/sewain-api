# Integrasi dengan core-v2

Dokumen ini untuk worker `auth`, `media`, `notify`, dan `ops`: apa yang disediakan inti v2 dan cara memasangnya.
Inti v2 = `internal/service` (aksi transaksional), `internal/db` (pool, migrasi, sqlc), `internal/httpapi` (wiring).
Hanya core-v2 yang mengubah `cmd/` dan `internal/httpapi`; modul lain cukup menyediakan paket + catatan, lalu
koordinator memasangnya di `cmd/api/main.go` seperti contoh di bawah.

## Gambaran wiring (`cmd/api/main.go`)

```go
pool, _ := db.OpenWithRetry(ctx, cfg.DatabaseURL, 30, 2*time.Second, nil)
db.Migrate(ctx, pool)                         // semua migrasi (semua modul) di internal/db/migrations, advisory lock
svc := service.New(pool, service.Options{DemoMode: cfg.DemoMode, SnapshotScope: cfg.SnapshotScope})
svc.SeedIfEmpty(ctx)                          // hanya DEMO_MODE
srv := httpapi.New(cfg, svc, log,
    httpapi.WithViewerResolver(...),          // auth
    httpapi.WithPhotoStore(...),              // media
)
svc.OnChange(func(ch service.Change) { ... }) // realtime/notify
svc.OnNotification(func(ctx, tx, n) error { ... }) // outbox notify
go svc.RunScheduler(ctx, 15*time.Second)
```

## Migrasi

- Berkas goose ditaruh di `internal/db/migrations/` (satu folder, di-embed oleh `internal/db.Migrations`). Rentang:
  inti `00001–00099`, auth `00100–00119`, media `00120–00139`, notify `00140–00159`, ops `00160–00179`.
- Provider goose dibuat dengan `WithAllowOutofOrder(true)` dan session locker Postgres (advisory lock `db.LockMigrate`),
  jadi migrasi modul yang di-merge belakangan tetap jalan walau nomornya lebih kecil dari yang sudah ada.
- FK ke `users(id)` (tipe `text`) boleh. Kalau migrasi modul dijalankan di atas DB demo yang di-reset, ingat
  `service.ResetDemo` men-`TRUNCATE … CASCADE` tabel inti (`users`, `bookings`, …): tabel modul yang punya FK ke
  tabel itu ikut kosong. Tabel yang harus bertahan saat reset (mis. sesi) jangan diberi FK ke `users`.
- Kalau modul butuh query sqlc, tambahkan berkas `.sql` di `internal/db/queries/` lalu `make generate` (paket `dbq`),
  atau pakai pgx langsung di paket modul. `make generate-check` dipakai CI.
- Advisory lock key: inti memakai `0x5e3a0001` (migrasi) dan `0x5e3a0002` (job). Pilih key lain untuk modul.

## Auth (worker `auth`)

### `httpapi.ViewerResolver`

```go
type ViewerResolver interface {
    Resolve(r *http.Request) (*service.Viewer, error) // nil = tamu; error = 500
}
type service.Viewer struct { UserID string; Role string /* user | admin */ }
```

- Implementasi sekarang: `httpapi.DemoTokens{Svc}` (bearer `Authorization: Bearer <token>` atau `?token=`), token
  dibuat oleh `POST /api/v1/auth/demo-login` dan yang disimpan hanya `sha256(token)` di tabel `demo_sessions`.
- Pasang implementasi OTP + cookie dengan `httpapi.WithViewerResolver(...)`. Untuk staging/demo tetap izinkan token
  demo: `httpapi.Chain(authResolver, httpapi.DemoTokens{Svc: svc})` (resolver pertama yang mengembalikan viewer menang).
- Resolver wajib mengisi `Role` dari DB (`svc.GetUserAuth`) supaya admin dikenali; user dengan status
  `suspend/blacklist` tetap boleh login (aturan transaksi ditolak oleh service dengan pesan yang sesuai).
- Semua handler memakai resolver yang sama: snapshot, aksi (aktor = argumen pertama harus `viewer.UserID`, kalau
  tidak 403), foto, `auth/me`. Cek `Origin` (CSRF) untuk cookie sebaiknya dibuat sebagai middleware di paket auth;
  koordinator akan membungkus `srv.Routes()`.

### Fungsi user untuk auth (`internal/service/users.go`)

| Fungsi | Isi |
|---|---|
| `svc.FindUserByPhone(ctx, phone) (*domain.User, error)` | `nil` kalau tidak ada. Nomor dinormalkan seperti v1: hanya digit, awalan `62` → `0` (`service.NormalizePhone`) |
| `svc.CreateUser(ctx, tx pgx.Tx, phone, name) (*domain.User, service.Change, error)` | Buat akun **di dalam transaksi pemanggil** (bersama sesi/consent). Mengirim notifikasi selamat datang seperti `registerUser`. Duplikat HP/email → `domain.UserError`. Setelah commit panggil `svc.Publish(change)` |
| `svc.GetUserAuth(ctx, id) (*service.UserAuth, error)` | `{ID, Role, Status, VerificationStatus}`, `nil` kalau tidak ada |
| `svc.User(ctx, id)` | Profil lengkap |

Kolom `users.phone_verified_at` disediakan untuk OTP. KYC: `verification_requests.kyc_submission_id` (text, tanpa FK)
bisa diisi id `kyc_submissions` milik modul auth. Aksi `submitVerification` sekarang masih simulasi (langsung
disetujui); saat KYC nyata dipasang, alurnya: modul auth menyimpan berkas terenkripsi lalu memanggil logika yang sama
(perubahan ini lewat koordinator).

## Realtime (worker `notify`)

Setiap transaksi yang mengubah data menghasilkan:

```go
type service.Change struct {
    Version int64    // versi baru (= "version" di snapshot)
    Users   []string // user yang data privatnya berubah: pihak booking, penerima notifikasi, aktor
    Admins  bool     // selalu true (admin melihat semuanya)
    Public  bool     // data publik berubah: listing, profil publik, ulasan dirilis, konfigurasi, papan, jam demo
}
```

- Daftarkan listener dengan `svc.OnChange(func(service.Change))`. Dipanggil setelah commit, di goroutine pemanggil
  (jangan blok; kirim ke channel).
- Baris yang sama ada di tabel `change_log(version, users, admins, public, at)` (ditulis di transaksi yang sama), jadi
  fan-out antar instance bisa memakai `LISTEN/NOTIFY` atau polling `version > n`. Baris lama dipangkas otomatis
  (menyisakan 10.000 terakhir).
- Hub v1 (`internal/httpapi/hub.go`) sekarang broadcast `{"type":"changed","version":n}` ke semua klien. Hub per user
  cukup mengirim event ke `Users` + admin, dan ke semua klien kalau `Public`. Snapshot yang diambil setelah event selalu
  punya `version ≥ n` (versi snapshot = `max(change_log.version)` di transaksi baca yang sama).
- Koordinator akan mengganti listener broadcast di `httpapi.New` dengan hub notify.

## Notifikasi (worker `notify`)

- Semua notifikasi in-app dibuat oleh satu fungsi: `txn.notify` di `internal/service/txn.go`. Fungsi itu memanggil
  hook yang didaftarkan dengan:

```go
svc.OnNotification(func(ctx context.Context, tx pgx.Tx, n *domain.Notification) error {
    // enqueue ke notification_outbox di transaksi yang sama
})
```

- Hook jalan **di dalam transaksi aksi**: kalau mengembalikan error, aksi dibatalkan. `n` berisi `ID, UserID, Kind,
  Title, Body, Href, At` (epoch ms jam demo). Kind: `booking`, `chat`, `sengketa`, `verifikasi`, `papan`, `sistem`.
- Notifikasi admin dialamatkan ke `domain.AdminID` (`u-admin`), sama seperti web v1.

## Media (worker `media`)

### `httpapi.PhotoStore`

```go
type PhotoStore interface {
    Put(ctx context.Context, key string, data []byte, contentType string) error // write-once
    Serve(w http.ResponseWriter, r *http.Request, key string) bool          // false = tidak ada
}
```

- Implementasi sekarang: `httpapi.LocalPhotoStore{Dir: UPLOAD_DIR}`. Pasang `media.Storage` lewat adapter kecil dan
  `httpapi.WithPhotoStore(...)`. Semua akses berkas foto ada di `internal/httpapi/photos.go` (unggah, foto contoh, serve).
- Metadata foto disimpan oleh `svc.AddPhoto(ctx, service.NewPhoto{...})` (kolom `object_key`, `mime`, `sha256`,
  `server_ts`). Baca dengan `svc.Photo(ctx, id)` (`ObjectKey`, `Mime`). Aturan akses per tahap ada di
  `svc.CanSeePhoto(ctx, photo, viewer)` (KTP/selfie: pengunggah + admin; foto booking: pihak booking + admin; referensi:
  publik). `GET /api/v1/photos/{id}` **belum** menegakkan aturan ini (sama seperti v1, `<img>` tidak bisa mengirim
  header). Dengan URL bertanda tangan (F3.3) handler bisa memanggil `CanSeePhoto` atau memverifikasi tanda tangan.
- Validasi unggahan sekarang: maks 10 MB, `http.DetectContentType` JPEG/PNG/WebP/GIF. Validasi magic bytes/dimensi dan
  strip EXIF dari modul media bisa menggantikan bagian awal `uploadPhoto`.
- `photos` append-only: UPDATE/DELETE hanya dengan `SET LOCAL sewain.retention = 'on'` (job retensi F3.6).

### Lokasi publik

- `domain.PublicLocation(lat, lng) (lat, lng)`: snap ke pusat sel grid 500 m (± 250 m per sumbu), stabil. Dipakai
  snapshot untuk listing milik orang lain. Ganti isi fungsi ini (atau panggil versi `media/geo`) tanpa mengubah
  pemanggil. Pemilik listing selalu menerima titik presisi.
- Pencarian membulatkan `distanceKm` ke 0,1 km.

## Keputusan di wilayah core-v2 (dan alasannya)

| Keputusan | Alasan |
|---|---|
| Versi dari `change_log` (`max(version)` di transaksi baca), bukan langsung `last_value` sequence | Sequence tidak transaksional: versi bisa terlihat sebelum datanya commit, lalu klien yang membandingkan versi tidak refetch |
| Kolom `ord` dari satu sequence global di tabel yang tampil di snapshot | Mereproduksi urutan v1 persis (append vs prepend), termasuk untuk seed |
| `acceptBooking` memeriksa stok (bentrok jadwal) | v1 hanya memeriksa saat request; tanpa ini dua request yang sama-sama disetujui melebihi stok (F1.10). Pesan baru: "Stok barang di tanggal itu sudah terpakai booking lain. …" |
| Penolakan aturan (UserError) me-rollback transaksi | Tidak ada efek setengah jadi. Respons tetap `200 {ok:false, error, version}` seperti v1 |
| Respons idempotency disimpan sebagai `text` | Replay byte-identik (jsonb mengubah format) |
| Email unik (case-insensitive) | Skema §3. Duplikat → "Email sudah dipakai akun lain." |
| `markThreadRead` untuk booking yang tidak ada diabaikan | `chat_reads` punya FK ke booking |
| Sesi demo tanpa FK ke `users` | Sesi tetap berlaku setelah `resetDemo` (perilaku v1, web menyimpan token) |
| Ulasan: kolom `visible_at` + `released` | Rilis 7 hari dilakukan job sehingga versi naik dan ETag/klien ikut berubah |
| Profil publik memakai nama depan sebagai `name`, `phone` kosong, `strikes` 0 | Bentuk JSON tetap `User` (web tidak berubah), data privat tidak ikut |
| Tawaran papan permintaan hanya untuk peminta/penawar | Pesan tawaran ditujukan ke peminta |
| Nomor HP disimpan seperti v1 (digit, awalan 0) | Seed dan web memakai format ini; `NormalizePhone` menerima `+62…` |

## Hal terbuka

- Web mengambil snapshot tanpa token → mode `scoped` = tampilan tamu sampai F6.3. Staging dengan web lama:
  `SNAPSHOT_SCOPE=full`.
- `sewain-web/e2e/api.mjs` lama mengharapkan ≥ 5 listing tersembunyi dari Pogung; seed sekarang menghasilkan 4 (v1 dan
  v2 sama, lihat `scripts/compare-v1.sh`). Versi `api.mjs` terbaru sudah memeriksa aturan jangkauan secara umum dan lulus.
- Filter `serah=` di pencarian v1 selalu error (operator jsonb `?` bentrok dengan placeholder); di v2 sudah jalan.

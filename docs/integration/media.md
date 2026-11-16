# Integrasi `internal/media` dan `internal/geo` (Fase 3)

Paket mandiri dari worker `media`. Tidak ada akses DB, tidak ada migrasi. Inti (`core-v2`) memasangnya di
`cmd/` dan `internal/httpapi` saat integrasi. Isi paket:

| Berkas | Isi |
|---|---|
| `storage.go`, `local.go`, `s3.go`, `config.go` | `Storage` write-once, `LocalStorage`, `S3Storage` (R2/MinIO), `FromEnv()` |
| `inspect.go`, `strip.go` | `Inspect` (validasi unggahan), `StripGPS` |
| `signer.go`, `policy.go` | `Signer` (URL bertanda tangan), `CanView`, `SignFor` |
| `handler.go` | `Handler` untuk `GET /api/v1/photos/{id}`, interface `PhotoLookup` |
| `watermark.go` | `Watermark`, `WatermarkedCopy` (cache di `wm/<id>.jpg`) |
| `retention.go` | `PlanPhotoRetention`, `RetentionKeys`, `Purge` |
| `internal/geo` | `FuzzPoint`, `DistanceKm`, `RoundDistanceKm`, `FormatDistance` |

## 1. Variabel lingkungan

| Env | Default | Keterangan |
|---|---|---|
| `MEDIA_BACKEND` | `local` | `local` atau `s3` (alias `r2`). |
| `UPLOAD_DIR` | `./data/uploads` | Root `LocalStorage`. Dibuat otomatis (0750, berkas 0640). |
| `S3_ENDPOINT` / `R2_ENDPOINT` | | `host[:port]` tanpa skema. Kalau kosong dan `R2_ACCOUNT_ID` diisi → `<account>.r2.cloudflarestorage.com`. |
| `R2_ACCOUNT_ID` | | Opsional, lihat di atas. |
| `S3_BUCKET` / `R2_BUCKET` | | Wajib untuk `s3`. |
| `S3_REGION` / `R2_REGION` | `auto` | R2 memakai `auto`. |
| `S3_ACCESS_KEY_ID` / `R2_ACCESS_KEY_ID` | | Kredensial aplikasi (**tanpa izin delete**, lihat §6). |
| `S3_SECRET_ACCESS_KEY` / `R2_SECRET_ACCESS_KEY` | | |
| `S3_USE_SSL` | `true` | `false` hanya untuk MinIO lokal. |
| `S3_PREFIX` / `R2_PREFIX` | | Opsional, mis. `staging` supaya staging dan produksi bisa satu bucket. |
| `S3_RETENTION_ACCESS_KEY_ID` / `S3_RETENTION_SECRET_ACCESS_KEY` (alias `R2_RETENTION_*`) | | Hanya dibaca `RetentionStorageFromEnv()` (job retensi). |
| `MEDIA_SIGNING_KEY` | **wajib** | ≥ 32 byte setelah decode. Boleh hex, base64, atau string mentah. Buat dengan `openssl rand -hex 32`. |
| `MEDIA_SIGNING_KEY_PREVIOUS` | | Kunci lama yang masih diterima saat rotasi (isi selama ≥ 1 jam + 15 menit, lalu hapus). |

Nama `S3_*` didahulukan, `R2_*` sebagai alias. `scripts/go.sh` hanya meneruskan `UPLOAD_DIR`; kalau perlu
env lain saat `go run` lewat Docker, tambahkan di daftar `envs` skrip itu (wilayah koordinator).

## 2. Mengganti penyimpanan lokal v1 di `internal/httpapi`

Saat ini `uploadPhoto` menulis `os.WriteFile(UploadDir/<id><ext>)`, `servePhoto` mencari dengan `filepath.Glob`.
Penggantinya:

```go
// cmd/api: sekali saat start
store, err := media.FromEnv()          // MEDIA_BACKEND, UPLOAD_DIR, S3_*/R2_*
signer, err := media.SignerFromEnv()   // MEDIA_SIGNING_KEY. Produksi: gagal start kalau kosong.
// Demo/staging boleh membuat kunci acak saat start kalau env kosong (URL lama jadi tidak valid setelah restart).

// POST /api/v1/photos (handler inti)
r.Body = http.MaxBytesReader(w, r.Body, media.MaxUploadBytes+1<<20) // + ruang untuk field multipart
data, _ := io.ReadAll(file)
info, err := media.Inspect(data)
var ue *media.UploadError
if errors.As(err, &ue) { writeErr(w, ue.Status, ue.Msg); return }   // pesan sudah Bahasa Indonesia
if stage == media.StageReferensi {
    // Foto listing publik: buang EXIF/XMP (GPS). Hash & ukuran dihitung ulang dari berkas yang disimpan.
    if data, err = media.StripGPS(data); err == nil { info, err = media.Inspect(data) }
}
// Foto bukti: JANGAN di-strip. Disimpan utuh supaya sha256 = byte yang diterima (FR-P3).
key := media.PhotoKey(id, info.Ext)                 // "photos/<id>.jpg"
if err := store.Put(ctx, key, data, info.MIME); err != nil { ... } // ErrExists = bug ID ganda, bukan ditimpa
// INSERT photos(id, booking_id, listing_id, stage, uploader_id, object_key=key, sha256=info.SHA256,
//               server_ts=now(), width=info.Width, height=info.Height, bytes=info.Bytes, mime=info.MIME, ...)
// Respons: photo.src = signer.SignFor(viewer, photo, booking, 0)

// GET /api/v1/photos/{id}
mux.Handle("GET /api/v1/photos/{id}", &media.Handler{
    Storage: store, Signer: signer, Lookup: corePhotoLookup, Log: logger,
    SessionViewer: nil, // opsional: func(r) *media.Viewer dari cookie sesi (D6) untuk akses tanpa tanda tangan
})
```

Catatan:

- Lebar/tinggi dari form v1 (`width`, `height`) tidak dipakai lagi. Ambil dari `info` (hasil dekode server).
- Tahap valid: `referensi`, `checkout`, `checkin`, `jaminan`, `masalah`, `sengketa`, `chat` (konstanta `Stage*`).
  `ktp`/`selfie` tidak pernah lewat endpoint ini (KYC punya endpoint admin sendiri milik worker `auth`).
- `Storage` dipakai juga oleh worker `auth` untuk berkas KYC terenkripsi (key mis. `kyc/<submission>/ktp.bin`).
  Aturan key: segmen `[A-Za-z0-9._-]` dipisah `/`, tanpa segmen kosong, `.`/`..`, atau berawalan titik.
- `demoPhoto` (foto contoh `/img/...`) tetap menyimpan `src` statis. Kalau mau seragam, inti bisa mengambil
  berkasnya lalu menyimpannya lewat `Put` seperti unggahan biasa.

## 3. URL bertanda tangan di snapshot

Field `src` setiap foto di `GET /api/v1/state` diisi **per viewer** saat snapshot dibangun:

```go
src := signer.SignFor(viewer, photo, booking, 0) // "" → foto jangan dimasukkan ke snapshot viewer ini
```

- `referensi` → `/api/v1/photos/{id}` tanpa tanda tangan (publik, `Cache-Control: public, immutable`).
- Bukti, pihak booking → `/api/v1/photos/{id}?exp=…&u=<viewer>&v=wm&sig=…` (salinan ber-watermark).
- Bukti, admin/mediator → URL yang sama tanpa `v=wm` (berkas asli, FR-P9).
- Tamu/user lain → `""`. Aturan ini sama dengan tabel visibilitas ROADMAP §4, jadi biasanya foto memang sudah
  tersaring sebelumnya. `SignFor` adalah pengaman kedua.

Detail tanda tangan:

- HMAC-SHA256 atas `photoID`, `exp`, `u`, `v` (dibingkai dengan panjang). Diverifikasi waktu-konstan, mendukung
  rotasi kunci.
- `exp` dibulatkan ke atas ke kelipatan 15 menit, jadi URL untuk pasangan (foto, viewer) **stabil** selama
  ±15 menit. Snapshot yang di-refetch tidak mengubah `src`, cache browser dan `ETag` snapshot tetap hangat.
  Masa berlaku efektif 1 jam sampai 1 jam 15 menit.
- `v=wm` ikut ditandatangani: pihak booking tidak bisa menghapus `v=wm` untuk mendapat berkas asli.
- Tanda tangan adalah kapabilitas. Handler tidak memeriksa ulang apakah `u` masih pihak booking, jadi inti
  **wajib** memanggil `CanView` (atau `SignFor`) sebelum menandatangani. Di URL ada `u` untuk audit.
- Klien yang mendapat `403` dengan pesan "kedaluwarsa" cukup refetch snapshot.

Header respons: `Content-Type` dari deteksi magic bytes, `ETag` = sha256 byte yang dikirim, `X-Content-SHA256` =
sha256 byte yang dikirim (untuk `v=wm` ini hash salinan watermark), `X-Original-SHA256` = hash berkas asli
(sama dengan `photos.sha256`), `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; sandbox`,
`Referrer-Policy: no-referrer`. Bukti: `Cache-Control: private, max-age=<sisa masa URL>`. Mendukung
`If-None-Match` → 304, `Range`, dan `HEAD`.

Handler juga mengecek integritas: kalau sha256 berkas di storage tidak sama dengan `photos.sha256`, respons 500
dan log `photo hash mismatch` (indikasi berkas bukti diubah).

**Perubahan perilaku untuk web:** sebelumnya `X-Content-SHA256` selalu hash asli. Untuk tampilan "hash cocok",
web sebaiknya memakai `sha256` dari snapshot atau header `X-Original-SHA256`.

## 4. Interface `PhotoLookup`

```go
type PhotoLookup interface {
    // ErrNotFound untuk ID yang tidak ada. booking = nil untuk foto listing.
    LookupPhoto(ctx context.Context, id string) (media.Photo, *media.Booking, error)
}
```

Implementasi inti (satu query, tanpa `FOR UPDATE`):

```sql
SELECT p.id, p.stage, coalesce(p.booking_id,''), coalesce(p.listing_id,''), p.uploader_id, p.object_key,
       p.sha256, p.mime, p.bytes, p.width, p.height, p.server_ts,
       b.id, b.code, b.renter_id, b.provider_id, b.status, b.completed_at, b.resolved_at
FROM photos p LEFT JOIN bookings b ON b.id = p.booking_id
WHERE p.id = $1
```

Isi `media.Booking.Code` dengan kode booking (tampil di watermark, fallback ke ID). Bisa juga memakai
`media.PhotoLookupFunc` untuk adaptor fungsi.

## 5. Job retensi (F3.6)

Bagian murni sudah ada. Inti memasang job harian (mis. di ticker yang sama dengan job waktu lain, atau
`api retention` sebagai perintah terpisah):

```go
st, _ := media.RetentionStorageFromEnv() // kredensial yang boleh delete
// 1. Kandidat: foto bukti dari booking COMPLETED/RESOLVED yang selesai > 90 hari, beserta sengketanya.
ids := media.PlanPhotoRetention(now, photos, bookings, disputes)
// 2. Hapus objek dulu (idempoten), lalu baris.
var keys []string
for _, p := range photosByID(ids) { keys = append(keys, media.RetentionKeys(p)...) } // asli + wm/<id>.jpg
n, err := media.Purge(ctx, st, keys) // lanjut walau ada yang gagal; err = gabungan
// 3. Baris DB: hanya untuk foto yang semua key-nya terhapus.
//    BEGIN; SET LOCAL sewain.retention = 'on'; DELETE FROM photos WHERE id = ANY($1); COMMIT;
//    + catat di audit_log (aktor "system", jumlah, rentang ID).
```

Aturan `PlanPhotoRetention`:

- Hanya tahap bukti. Foto `referensi` ikut siklus hidup listing, KYC diurus worker `auth`.
- Jam mulai = `CompletedAt` (COMPLETED) atau `ResolvedAt` (RESOLVED). Kalau ada sengketa yang selesai lebih
  belakangan, jam mulai = waktu sengketa selesai.
- Sengketa yang belum selesai (`ResolvedAt == nil`) menahan semua foto booking itu.
- Data tidak lengkap (booking tidak dikenal, waktu kosong) = simpan.
- Batas inklusif: tepat 90 hari sudah boleh dihapus.

Urutan "objek dulu, baris kemudian" memastikan tidak ada baris yang menunjuk berkas hilang tanpa terdeteksi.
Kalau job mati di tengah, run berikutnya mengulang dengan aman karena `Delete` idempoten.

Untuk query kandidat, cukup ambil booking yang `completed_at`/`resolved_at < now() - interval '90 days'`
beserta foto dan sengketanya, lalu serahkan ke `PlanPhotoRetention` sebagai pemutus akhir.

## 6. Bucket R2 write-once

Write-once dijaga tiga lapis: `Put` mengirim `If-None-Match: *` (R2 dan MinIO menolak timpa dengan 412 →
`ErrExists`), pengecekan `Exists` sebelum `Put`, dan kredensial aplikasi yang tidak bisa menghapus.

Langkah di Cloudflare (kerjakan lewat opencode sesuai aturan server, bukan manual):

1. Buat bucket `sewain-media` (lokasi APAC), **tanpa** akses publik dan tanpa custom domain. Semua foto lewat API.
2. Token API aplikasi: R2 "Object Read & Write" terbatas ke bucket ini. R2 belum punya izin "write tanpa
   delete" per token, jadi lapisan aturannya:
   - Aktifkan **Bucket Lock** (R2 object lock) dengan aturan retensi untuk prefix `photos/` selama
     90 hari. Objek tidak bisa dihapus/ditimpa sebelum umurnya lewat, siapa pun kredensialnya. Hapus di job
     retensi selalu ≥ 90 hari setelah booking selesai, jadi tidak bentrok.
   - Prefix `wm/` (cache watermark) dan `kyc/` (berkas KYC, dihapus setelah keputusan verifikasi) **tidak**
     dikunci.
3. Token kedua untuk job retensi ("Object Read & Write", bucket yang sama), disimpan hanya di env job sebagai
   `S3_RETENTION_ACCESS_KEY_ID`/`S3_RETENTION_SECRET_ACCESS_KEY`. Proses API tidak memegang token ini.
   Kalau suatu saat dipindah ke S3/MinIO yang mendukung IAM policy, token aplikasi cukup `s3:PutObject`,
   `s3:GetObject`, `s3:ListBucket` (tanpa `s3:DeleteObject`), dan token retensi menambah `s3:DeleteObject`.
4. Uji setelah setup: `Put` dua kali key yang sama harus `ErrExists`, dan `Delete` objek `photos/` berumur < 90 hari
   harus ditolak.

Staging boleh berbagi bucket dengan `S3_PREFIX=staging`.

## 7. Lokasi publik (`internal/geo`)

```go
lat, lng := geo.FuzzPoint(listing.ID, listing.Lat, listing.Lng) // untuk tamu dan user selain pemilik
km := geo.DistanceKm(viewerLat, viewerLng, lat, lng)            // jarak dari titik publik, bukan titik asli
label := geo.FormatDistance(km)                                  // "± 1,2 km"
```

- Grid 400 m (pusat sel ≤ 283 m dari titik mana pun di sel) + offset 40–100 m dari hash `(listingID, sel)`.
  Jarak ke titik asli selalu ≤ 400 m dan tidak pernah 0.
- Stabil: tidak berubah antar request, jadi tidak bisa dirata-rata. Rata-rata banyak listing di titik yang sama
  menuju pusat sel, bukan titik asli. Hasil dibulatkan 6 desimal.
- Pemilik dan admin tetap melihat titik presisi (ROADMAP §4). `meeting_point_detail` baru dibuka setelah
  `ACCEPTED` (FR-S6), itu aturan inti.
- Pencarian PostGIS tetap memakai titik asli di server. Yang dikirim ke klien hanya titik publik dan jarak
  yang dibulatkan (`RoundDistanceKm`: < 0,5 km → 0,5; < 10 km → 1 desimal; selebihnya bilangan bulat).

## 8. Tes

```sh
scripts/go.sh test ./internal/media/ ./internal/geo/
```

Tes S3 (`TestS3StorageMinio`) menjalankan MinIO sekali pakai (`sewain-test-minio-<acak>`, port acak di
127.0.0.1, dihapus di akhir tes) dan di-skip kalau CLI `docker` tidak ada. Di dalam `scripts/go.sh` tidak ada
Docker, jadi tes itu di-skip. Untuk menjalankannya:

```sh
docker run --rm -i --network host --user "$(id -u):$(id -g)" \
  --group-add "$(stat -c %g /var/run/docker.sock)" \
  -v /var/run/docker.sock:/var/run/docker.sock -v /usr/bin/docker:/usr/bin/docker:ro \
  -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e GOFLAGS=-buildvcs=false \
  -v sewain-go-cache:/cache -v "$PWD":"$PWD" -w "$PWD" golang:1.27.3 \
  go test -count=1 -run Minio -v ./internal/media/
```

Image MinIO: `pgsty/minio:RELEASE.2026-08-04T00-00-00Z` (build komunitas; image resmi `minio/minio` sudah tidak
dipublikasikan di Docker Hub/Quay). Bisa diganti lewat `MEDIA_TEST_MINIO_IMAGE`.

## 9. Keputusan yang diambil di wilayah media

| Keputusan | Alasan |
|---|---|
| Tanpa migrasi | Tabel `photos` milik inti. Media cukup struct + interface. Cache watermark disimpan di storage, bukan DB. |
| `LocalStorage` memakai `os.Root` + `link(2)` | `os.Root` menutup traversal lewat symlink. Hard link gagal kalau nama sudah ada, jadi write-once atomik tanpa race. Content type disimpan di `.meta/<key>`. |
| Polyglot: tolak byte setelah penanda akhir (EOI/IEND/ukuran RIFF), kecuali padding nol | Menolak JPEG+ZIP/HTML dan "motion photo" (video ditempel di JPEG). Klien v2 mengompres lewat canvas (FR-P7), jadi berkas normal lolos. |
| Pemindaian markup (`<script`, `<svg`, `<?php`, …) hanya di segmen metadata | Data piksel terkompresi acak dan bisa memicu positif palsu. Payload tersembunyi ada di metadata/ekor berkas. |
| Dekode penuh setelah cek header | Membuktikan berkas utuh. Dimensi dan perkiraan memori dekode (`lebar × tinggi × max(4, byte/piksel)` ≤ `MaxDecodeBytes` = 64 MB, ±16 MP) dicek dulu dari header, jadi bom dekompresi ditolak sebelum alokasi. Dekode penuh (`Inspect`, `Watermark`) dijalankan satu per satu per proses (`MaxConcurrentDecodes`), karena container API dibatasi 256 MB. Lihat temuan M1 di `docs/security/review-wave1.md`. |
| `StripGPS` juga untuk PNG (`eXIf`, chunk teks) dan WebP (`EXIF`, `XMP `), orientasi JPEG dipertahankan | Foto listing bisa datang dalam ketiga format. Tanpa orientasi, foto ponsel tampil miring. |
| Watermark: font bitmap `basicfont` diskalakan ~3% sisi pendek, kanan bawah, pita hitam α≈0,43, teks putih α≈0,84, JPEG q85, orientasi EXIF diterapkan | Tanpa berkas font tambahan, terbaca di ponsel, tidak menutupi barang. |
| `SignFor`: pihak booking dapat `v=wm`, admin dapat asli | FR-P5 (tampilan ber-watermark) dan FR-P9 (mediator membandingkan berkas asli). |
| `u` (viewer) ada di URL dan ikut ditandatangani | `<img>` mode bearer per tab tidak membawa sesi. `u` untuk audit, dan URL tidak bisa dipindah ke viewer lain tanpa merusak tanda tangan. |
| Grid 400 m + offset 40–100 m (bukan grid 500 m) | Grid 500 m + offset bisa melewati batas 400 m. 283 + 100 < 400. |

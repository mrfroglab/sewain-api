# Integrasi `polish` (sisa temuan sebelum go-live)

Branch `januarsyah901/polish`. Menutup L1–L4, L6, I1 dari [`docs/security/review-wave2.md`](../security/review-wave2.md)
(§5) dan temuan 7.1–7.3 dari [`docs/loadtest/2026-11-report.md`](../loadtest/2026-11-report.md) (§10).

## Perubahan kontrak (untuk web dan operator)

| Area | Perubahan |
|---|---|
| `GET /api/v1/users/{id}` (baru) | Publik, profil rate limit `public`, `Cache-Control: public, max-age=60`. Respons `{ok, user, listings, reviews}`. `user` = entri `users[]` snapshot tamu (`publicProfile` + `publicStats`, `demo` di DEMO_MODE), sama untuk siapa pun yang meminta (termasuk pemilik profil). `listings` = listing aktif pemilik aktif, lokasi publik (`geo.FuzzPoint`), tanpa `meetingPoint`. `reviews` = ulasan tentang dia yang sudah tampil (`released` atau `visibleAt` ≤ sekarang). Tidak ada/terhapus/blacklist → 404 `{ok:false,error:"Pengguna tidak ditemukan."}` |
| `SNAPSHOT_USERS` | Default `referenced`. DEMO_MODE: + semua akun persona demo (`demo IS NOT NULL`). `all` lewat env |
| ETag `GET /api/v1/state` | Dihitung dari versi `change_log` terakhir yang relevan untuk viewer. `version` di body tetap versi global (makna tidak berubah untuk event realtime) |
| Realtime | Booking yang masuk/keluar `ACCEPTED`/`ACTIVE`/`DISPUTED`/`COMPLETED` kini event `Public` (tamu ikut mendapat `changed`) |
| Aksi | Pesan galat baru (Bahasa Indonesia) untuk batas panjang, nominal usulan, aksi admin atas entitas final, foto listing bukan unggahan sendiri |
| `listing.photos` | Kirim `/api/v1/photos/<id>` (atau id) foto tahap `referensi`. `/img/…` hanya di DEMO_MODE. Server menyimpan bentuk kanonis `/api/v1/photos/<id>` |
| Impor CSV | `handover` menerima `cod`, `jemput`, `kurir`, `antar` (tidak peka huruf besar). `photos`: id/URL foto yang diunggah admin atau pemilik; `/img/…` hanya di DEMO_MODE |
| Env baru | `RATE_IP_PER_MIN` (6000), `RATE_IP_BURST` (3000): rem per IP sebelum auth |

## Keputusan

- **ETag per viewer dari `change_log`, bukan hash body.** Hash body butuh membangun snapshot di setiap request, padahal
  biaya build adalah leher botol uji beban. Versi relevan = `max(version)` dengan filter `public OR $me = ANY(users)`
  (semua untuk admin dan scope `full`), dengan `min(version)` sebagai pengganti saat baris viewer sudah dipangkas
  (nilai selalu naik, jadi ETag lama tidak pernah muncul kembali setelah ada perubahan). Ini aman karena setiap
  penulisan yang mengubah snapshot seseorang memanggil `touch(user)` atau `touchPublic()`, yang juga dipakai realtime.
  Saya mengaudit semua aksi, job, KYC, foto, hapus akun, dan modul payments (tidak menulis tabel inti). Satu
  pengecualian kecil: CLI `api admin grant` di modul auth mengubah `users.role` tanpa `change_log`. Dampaknya hanya
  label role di snapshot sampai jendela ETag 15 menit berganti.
- **Hal yang bergantung waktu** (ulasan rilis 7 hari, `listingBusy` lewat tengah malam) tetap ditangani jendela ETag
  15 menit, sama seperti sebelumnya.
- **`touchPublic` di `saveBooking` berdasarkan transisi status.** Status lama dibaca dari DB (satu query murah pada
  baris yang sudah dikunci), karena job memuat booking dari baris tanpa `bookingForUpdate`.
- **`/api/v1/users/{id}` selalu proyeksi publik**, juga untuk pemiliknya sendiri, karena respons boleh di-cache
  publik. Listing pemilik yang `suspend` tidak dikirim (sama dengan snapshot). Sumber data sama dengan snapshot
  (`publicProfile`, `snapReader.publicStats`), jadi tidak bisa menyimpang.
- **Batas nominal usulan (PRD §11.3)** mengikuti `ProposalSheet` web: potong deposit ≤ `payment.deposit`, ganti
  perbaikan/nilai ≤ `agreement.itemValue`. Tambahan: perpanjang ≤ nilai barang, tanpa kompensasi tanpa nominal,
  rekomendasi admin ≤ maks(nilai barang, deposit).
- **L4 tanpa migrasi data.** Alasan dan query pemeriksaan ada di review-wave2 §5.
- **L3 normalisasi hanya di server.** Web boleh tetap memakai filter lamanya. Server adalah penjaga terakhir.
- **Rem `ip` 6000/menit, burst 3000.** Di atas puncak NAT kampus yang terukur (±3.600 request/menit untuk 300 VU) dan
  tes `TestCampusNATDefaults` (2.700 request sekaligus).

## Hal terbuka

- Belum ada uji beban k6 ulang untuk mengukur rasio 304 yang baru (perlu staging/stack sementara, di luar wilayah
  worker ini). Benchmark Go ada di laporan uji beban §10.1.
- Filter kata terlarang di web belum sama dengan server (L3).

# Review keamanan gelombang 1 (bagian awal F5.8)

Reviewer independen untuk paket hasil worker auth, media, notify, dan ops yang sudah ada di `develop`:
`internal/auth`, `internal/kyc`, `internal/crypto`, `internal/media`, `internal/geo`, `internal/realtime`,
`internal/notify`, `internal/platform/*`, dan migrasi `internal/db/migrations/001xx`. Acuannya OWASP ASVS L1
ditambah konteks Sewain (PRD §12 dan §16).

Cara kerja: semua berkas di paket tersebut dibaca utuh. Setiap dugaan dibuktikan dengan tes yang gagal di
kode lama, lalu diperbaiki dengan perubahan sekecil mungkin. Tiap temuan punya commit sendiri (`fix(<paket>): …`)
dan tesnya ikut di commit itu. Dugaan yang ternyata aman dicatat di §3 beserta alasannya. Branch sudah di-rebase
ke `develop` terbaru (sudah berisi inti v2), jadi tes `internal/service` dan `internal/httpapi` juga ikut
menjalankan perubahan ini.

Severity: **tinggi** = bisa dieksploitasi dari luar tanpa syarat khusus dan dampaknya besar (layanan mati,
data pribadi bocor). **Sedang** = butuh kondisi tertentu (salah konfigurasi, IPv6, dsb.) atau dampaknya
terbatas. **Rendah** = dampak kecil atau butuh akses admin. **Info** = pengerasan atau catatan integrasi.

## 1. Tabel temuan

| ID | Paket | Severity | Ringkasan | Skenario serangan | Status | Commit |
|---|---|---|---|---|---|---|
| M1 | media | **tinggi** | Bom dekompresi: cek dimensi saja (≤ 8000 px per sisi) tidak membatasi memori dekode. Tidak ada pembatasan dekode paralel. | PNG RGBA 8000×8000 berisi nol, **242 KB** di jaringan, membuat `Inspect` mengalokasikan **244 MB** (16 bit: 484 KB → **488 MB**; diukur dengan `runtime.MemStats`). Container API `mem_limit: 256m`, jadi satu unggahan (atau satu render watermark) membuat proses OOM-kill. | diperbaiki: perkiraan memori dari header (`w×h×max(4, byte/piksel)` ≤ `MaxDecodeBytes` = 64 MB, ±16 MP) dicek sebelum dekode di `Inspect` dan `Watermark`. Dekode penuh dijalankan satu per satu (`MaxConcurrentDecodes` = 1). | `8dd8f04` |
| A1 | auth | sedang | `ValidateConfig` hanya membandingkan `APP_ENV == "production"`. | Operator salah tulis (`APP_ENV=prod`, `Production`, `live`) dengan `OTP_PROVIDER=echo`/`demo`. Server tetap jalan dan kode OTP dikembalikan di respons (atau tetap `123456`), jadi siapa pun bisa masuk sebagai nomor mana pun. | diperbaiki: `APP_ENV` yang tidak dikenal ditolak (fail closed). | `8c3274d` |
| P1 | platform/ratelimit | sedang | `ByIP` memakai alamat IPv6 penuh sebagai kunci. | Satu pelanggan IPv6 (rumah, VPS) memegang satu /64 utuh (2^64 alamat). Penyerang mengganti bit bawah tiap request, jadi selalu dapat bucket baru. Profil `auth` (OTP) praktis tanpa batas, dan limiter memori menyimpan satu bucket per alamat. | diperbaiki: IPv6 dikunci per /64 (`IPBucket`). IPv4 dan IPv4-mapped tetap per alamat. | `ae872a3` |
| A2 | auth | sedang | Batas OTP per IP (`OTPMaxPerIPHour`) menghitung string alamat persis. | Sama seperti P1, di lapisan DB: rotasi alamat dalam satu /64 membuat satu klien bisa meminta OTP untuk nomor tak terbatas. Akibatnya biaya SMS/WA membengkak ("SMS pumping") dan nomor orang lain di-spam. | diperbaiki: `otp_challenges.ip` menyimpan /64 untuk IPv6. | `ed19c84` |
| A3 | auth | sedang | `auth.ClientIP` memercayai `CLIENT_IP_HEADER` dari peer mana pun, dan untuk `X-Forwarded-For` mengambil hop **pertama** (ditulis klien; Caddy menambahkan di belakang). | Klien mengirim `X-Forwarded-For: <acak>` sehingga setiap request dapat bucket OTP per-IP baru, dan IP palsu tercatat di sesi/consent. | diperbaiki: IP hasil `middleware.RealIP` (hanya percaya `TRUSTED_PROXY_CIDRS`) didahulukan. Fallback `X-Forwarded-For` memakai hop terakhir. | `604de39` |
| A4 | auth | rendah | `ops.md` menjanjikan auth memanggil `middleware.SetUserID`, tetapi middleware auth tidak pernah memanggilnya. | Semua limit `ByUser` (write, upload) jatuh ke IP. Satu NAT kampus berbagi bucket 60/menit, jadi satu mahasiswa bisa menghabiskan kuota semua orang. Log akses juga tidak punya `user_id`. | diperbaiki | `604de39` |
| A5a | auth | rendah | `Origin: null` jatuh ke pengecekan `Referer`. | Konteks dengan origin opak (iframe sandbox, rantai redirect lintas origin) adalah kasus ketika browser sengaja menyembunyikan pemicu. Header yang dikendalikan konteks pemicu tidak boleh dipakai untuk menggantinya. | diperbaiki: `null` selalu ditolak. | `0bef035` |
| A5b | auth | sedang | Login CSRF: request tanpa cookie sesi dilewatkan dari cek Origin, dan decoder JSON tidak memeriksa `Content-Type`. | Situs mana pun bisa auto-submit form `text/plain` berisi `{"phone":"<nomor penyerang>","code":"<kodenya>",...}` ke `/otp/verify`. Browser korban lalu masuk ke akun penyerang, dan KTP/foto yang diunggah korban berikutnya masuk ke akun itu. | diperbaiki: mutasi tanpa kredensial yang membawa `Origin` asing ditolak 403. Klien tanpa `Origin` (aplikasi, curl) tetap boleh. | `0bef035` |
| R1 | realtime | sedang | `MaxGuestConns` default 0 = tanpa batas (dan memang dipasang begitu di `notify.md`). | Tiap koneksi memegang 2 goroutine, antrean, dan buffer socket. Satu klien (atau satu /64) bisa membuka socket tamu sampai container 256 MB habis. | diperbaiki: 0 berarti `DefaultMaxGuestConns` (1000 per instance). Nilai negatif berarti tanpa batas (eksplisit). | `f180f7d` |
| K1 | kyc | sedang | `PurgeFiles` selalu mengambil submission jatuh tempo tertua dan berhenti pada galat pertama. | Satu objek yang tidak bisa dihapus (izin, object lock, key ditolak storage) menahan purge **semua** KTP/selfie berikutnya selamanya. Data identitas tersimpan melewati kebijakan 30 hari (UU PDP). | diperbaiki: submission yang gagal dilewati untuk sisa run, galatnya digabung (`errors.Join`). | `590f8c9` |
| K2 | kyc | sedang | Tidak ada cara menghapus data identitas user (UU PDP, hak hapus). | Soft delete (inti) menyimpan NIK terenkripsi, nama, dan berkas KTP/selfie selamanya (submission `pending` tidak pernah di-purge). Hard delete meng-cascade baris DB tetapi berkas terenkripsi tertinggal di storage. | diperbaiki: `kyc.Service.EraseUser(ctx, userID)` menghapus berkas lalu baris submission dan identity, dicatat di audit (`kyc.erase`). `nik_blacklist` (hanya HMAC) dipertahankan supaya orang yang diblokir tidak bisa daftar ulang. **Inti perlu memanggilnya** saat `DELETE /api/v1/me` dan saat admin menghapus akun. | `1ad3fb6` |
| K3 | kyc | rendah | Saat audit gagal, `ReadFile` menulis id dari path URL apa adanya ke log. | `PathValue` sudah di-decode, jadi `…/kyc/x%0Akyc: audit ok/files/ktp` menyisipkan baris palsu ke log yang menjadi bukti siapa membuka dokumen identitas. Hanya admin yang bisa memicunya. | diperbaiki: `%q`. | `015c132` |
| K4 | kyc | info | `ReadFile` mengembalikan content type dari amplop terenkripsi tanpa dicek ulang. | Tidak bisa dieksploitasi sekarang (tipe di-sniff dan di-allow-list saat unggah, ditambah `nosniff` dan CSP sandbox). Ini pertahanan berlapis untuk data lama atau jalur tulis lain. | diperbaiki: tipe di luar JPEG/PNG/WebP disajikan sebagai `application/octet-stream`. | `015c132` |
| **C1** | **inti (`internal/service`, `queries/listings.sql`)** | **tinggi** | Pencarian menghitung `ST_Distance` dan filter `ST_DWithin(l.location, pt, @radius_m)` terhadap lokasi **asli**, dengan titik `lat/lng` dan `radiusKm` (float bebas) pilihan pemanggil. | Oracle keanggotaan: listing muncul jika dan hanya jika `jarak(asli, titik) ≤ radius`. Binary search pada `radiusKm` dari 3 titik memberi jarak persis, lalu trilaterasi memberi titik presisi. Ini membatalkan FR-S6 (fuzz ±250 m) dan membocorkan alamat rumah/kos penyedia. Pembulatan 100 m di `DistanceKm` tidak menolong karena filter radius tidak dibulatkan. | **terbuka (wilayah inti, tidak boleh saya sentuh).** Saran: simpan `public_location` (hasil `geo.FuzzPoint`) dan pakai kolom itu untuk `ST_Distance`/`ST_DWithin` publik, atau bulatkan titik dan radius masukan ke grid ≥ 400 m. Lokasi asli hanya untuk pemilik dan pihak booking yang sudah diterima. | – |
| L1 | auth | rendah | Tebakan OTP jangka panjang: 5 challenge/jam × 5 percobaan = 25 tebakan/jam per nomor. | Sekitar 1,8%/bulan untuk satu nomor target, tetapi korban akan menerima ±120 OTP/hari (sangat terlihat). | diterima. Saran: batas harian per nomor (mis. 10/hari) saat ada data gateway. | – |
| L2 | notify | rendah | `SaveSubscription` memindahkan langganan ke user lain lewat `ON CONFLICT (endpoint)`. | Penyerang yang tahu URL endpoint push korban bisa memindahkannya ke akunnya atau mengganti kunci (notifikasi korban berhenti). Endpoint adalah URL rahasia acak dari browser dan tidak bisa ditebak. | diterima | – |
| L3 | kyc | rendah | `Queue`/`Get` mendekripsi nama di KTP untuk admin tanpa entri audit (hanya akses berkas yang diaudit). | Admin nakal bisa melihat nama + NIK tersamar tanpa jejak. | diterima untuk pilot. Saran: audit `kyc.view` di `Get`. | – |
| L4 | notify | rendah | `notification_outbox.payload` menyimpan alamat email (`To`) dan isi notifikasi tanpa batas waktu. Baris `dead`/`sent` tidak pernah dibersihkan. | Retensi data pribadi berlebih (UU PDP), terutama untuk akun soft-delete. | terbuka untuk inti: job harian `DELETE FROM notification_outbox WHERE status IN ('sent','dead') AND updated_at < now() - interval '30 days'`, dan hapus baris user saat akun dihapus. | – |
| L5 | kyc | rendah | Submission `pending` tidak pernah di-purge (`files_purge_after` baru diisi saat keputusan). | Kalau antrean admin terbengkalai, KTP/selfie tersimpan tanpa batas. | diterima (desain D7). Saran: alarm antrean > 7 hari. | – |
| I1 | auth/inti | info | `Resolve` hanya memblokir status `blacklist`. User soft-delete atau status lain tetap punya sesi aktif. `GrantAdmin` tidak memfilter `deleted_at`. | Bergantung integrasi inti. | catatan integrasi: saat blacklist atau hapus akun, inti wajib memanggil `authSvc.RevokeAllSessions` lalu `kycSvc.EraseUser` (hapus akun). `FindUserByPhone`/`GetUserAuth` dan `GrantAdmin` perlu `deleted_at IS NULL` (sudah tercatat di `auth.md`). | – |
| I2 | kyc/auth | info | Log `log.Printf` stdlib di kyc/auth tidak lewat `RedactingHandler`. Galat provider KYC vendor (Fase 8) bisa memuat NIK. Galat Zenziva bisa memuat URL; kalau kredensial ditaruh di query `ZENZIVA_*_URL`, kredensial ikut tercatat. | – | catatan: pasang `slog.SetDefault(middleware.NewLogger(...))` dan arahkan `log` stdlib ke sana di `main`. Jangan taruh kredensial di URL gateway. | – |
| I3 | media | info | URL foto bertanda tangan adalah kapabilitas bearer (berlaku ≤ 75 menit, viewer di `u` tidak diotorisasi ulang). | URL yang dibagikan bisa dibuka siapa pun sampai kedaluwarsa. | diterima (desain D8; varian `wm` ikut ditandatangani). | – |
| I4 | platform | info | `ratelimit.Memory` per proses: dengan N instance, limit efektif N×. Limiter gagal-terbuka (tetapi `Memory` tidak pernah mengembalikan galat). | – | diterima (sudah didokumentasikan). Batas OTP yang penting ada di DB (A2). | – |
| I5 | platform/realtime | info | `/api/health` menampilkan versi dan commit. Pesan WS `version` memperlihatkan laju perubahan dataset ke tamu. | Fingerprinting ringan. | diterima | – |

Temuan golangci-lint (permintaan koordinator) dibereskan di commit `3a91a32`, `bbbed8d`, `105fe48`, dan `3cde853`.
Yang relevan keamanan sudah masuk tabel di atas (G706 menjadi K3, G705 menjadi K4). Penanda `#nosec` hanya dipasang
dengan alasan konkret:

- G120 di `kyc/handlers.go`: body sudah dibatasi `MaxBytesReader` (2×8 MB + 1 MB), memori form 1 MB, dan jumlah part dibatasi net/http.
- G124: `Secure` mengikuti `SESSION_COOKIE_SECURE`, yang default `true` di luar development/demo. `HttpOnly` dan `SameSite=Lax` tetap.
- G704: URL gateway berasal dari env operator, tidak pernah dari request.
- G118: `cancel` disimpan di `client.cancel` dan dipanggil `stop()`.
- G115: rentang dicek atau dijamin konstanta.
- G304: hanya di fixture tes.

Sisa lint hanya `misspell` (ejaan British), yang menurut koordinator akan dinonaktifkan di `.golangci.yml`.

## 2. Dampak perubahan API untuk pemasangan

- **media**:
  - Konstanta baru `MaxDecodeBytes` (64 MB) dan `MaxConcurrentDecodes` (1).
  - Foto di atas ±16 MP (mis. 5000×5000, atau PNG 16 bit di atas 8 MP) ditolak dengan pesan *"Resolusi foto terlalu besar…"*. Web perlu mengecilkan foto di klien (sisi terpanjang ≤ 4096 px), dan memang begitu seharusnya untuk 4G (PRD §16).
  - Unggahan dan render watermark kini antre satu per satu per proses.
- **auth**:
  - `ValidateConfig`/`ConfigFromEnv`/`NewService` menolak `APP_ENV` di luar `demo|development|staging|production`.
  - Middleware menolak (403) mutasi tanpa kredensial dengan `Origin` asing. Origin aplikasi mobile/Capacitor harus masuk `ALLOWED_ORIGINS`.
  - `Origin: null` selalu ditolak.
  - `ClientIP` mendahulukan `middleware.ClientIPFrom`, jadi `middleware.RealIP` harus ada di rantai sebelum `authSvc.Middleware` (urutan di `ops.md` sudah begitu).
  - `otp_challenges.ip` berisi /64 untuk IPv6.
  - auth kini meng-import `internal/platform/middleware` dan `internal/platform/ratelimit` (tidak ada siklus).
- **platform/ratelimit**: `ByIP` mengelompokkan IPv6 per /64. Fungsi baru `IPBucket`.
- **realtime**: `Options.MaxGuestConns` 0 sekarang berarti 1000. Pakai nilai negatif kalau memang ingin tanpa batas.
- **kyc**:
  - Fungsi baru `EraseUser`.
  - `PurgeFiles` tetap mengembalikan `(jumlah, error)`, tetapi kini melanjutkan setelah galat dan menggabungkan galatnya.

## 3. Dicek, aman

**Auth/OTP/sesi**

- Enumerasi nomor: respons `/otp/request` identik untuk nomor terdaftar dan tidak (ada tes). `consent_required`, `blacklist`, dan status akun baru terlihat setelah kode benar.
- Race rate limit: `RequestOTP` memakai advisory lock per nomor lalu per IP dalam transaksi (urutan tetap, tidak deadlock). Tes paralel 8 request memastikan hanya 1 yang lolos. `VerifyOTP` memakai `FOR UPDATE` pada challenge terbaru, jadi percobaan salah paralel tetap terhitung semua dan kode benar hanya terpakai sekali.
- Normalisasi nomor: semua bentuk (`0812…`, `62812…`, `+62 812-…`, `812…`) menjadi satu E.164. Digit non-ASCII, `0062…`, dan `+620…` ditolak, jadi tidak ada dua representasi untuk satu nomor.
- Timing: kode OTP dibandingkan sebagai HMAC dengan `hmac.Equal`. Token sesi di-hash SHA-256 lalu dicari lewat indeks unik, tidak dibandingkan string. Tanda tangan URL foto memakai `hmac.Equal`.
- Fixation: login selalu membuat token baru (32 byte acak), dan cookie lama tidak dipakai ulang.
- Blacklist dicek di setiap `Resolve`, jadi sesi langsung mati. Suspend boleh masuk sesuai desain.
- Cookie: `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` default di luar dev/demo. Mode bearer dilarang di produksi.
- Token tidak masuk URL. Log akses tidak mencatat query string. `RedactingHandler` menutup `token=`, `Bearer`, nomor HP, dan kunci rahasia.
- Provider demo/echo: ditolak di produksi (ditambah A1 untuk env tak dikenal). `DEMO_MODE` di produksi ditolak dua lapis (`config` dan `auth`).
- Pencocokan Origin persis (case-insensitive) terhadap daftar, jadi subdomain mirip (`sewain.example.evil.com`) tidak lolos.

**Kripto/KYC**

- Nonce AES-GCM 96 bit acak per Seal. Batas aman 2^32 pesan per kunci jauh di atas volume pilot. Kunci bisa dirotasi lewat key ring.
- AAD mengikat baris: `kyc:nik:<submission>`, `kyc:name:<submission>`, `kyc:file:<kind>:<submission>`, `kyc:identity-nik:<user>`. Tes memastikan blob yang ditukar antar baris gagal dibuka.
- Parsing key ring: id divalidasi, duplikat ditolak, panjang kunci tepat 32 byte, dan pesan galat menyamarkan kunci.
- NIK polos tidak masuk DB, log, atau galat (tes `TestSubmitStoresNoPlaintext` mencari NIK, nama, dan isi berkas di semua tabel dan blob). Ke luar hanya `nik_masked`.
- Satu NIK satu akun: NIK harus tepat 16 digit ASCII (spasi di tengah dan digit lebar penuh ditolak), jadi HMAC-nya tunggal. Ada `UNIQUE(identities.nik_hmac)` plus advisory lock NIK, dan pengajuan `pending` milik akun lain ikut dihitung.
- Blacklist dicek saat submit dan saat approve (persetujuan setelah blacklist ditolak).
- Akses berkas KTP/selfie: hanya admin, setiap percobaan (termasuk id/kind tak dikenal) diaudit sebelum data disentuh, dan akses ditolak (503) kalau audit gagal.
- Purge hanya menyentuh baris dengan `files_purge_after <= now` dan `files_purged_at IS NULL`. Status, `nik_masked`, dan identity dipertahankan (ada tes). Lihat K1/K2 untuk kekurangannya.

**Media**

- Path traversal: `ValidateKey` (segmen `[A-Za-z0-9._-]`, tanpa `.`/`..`/segmen tersembunyi/absolut). `LocalStorage` lewat `os.Root` (symlink tidak bisa keluar root). `S3Storage` memvalidasi key dan prefix.
- Berkas berbahaya: tipe ditentukan dari magic byte (SVG/HTML tidak pernah lolos). Struktur container diparse, sehingga byte setelah EOI/IEND (polyglot ZIP/HTML) ditolak. Markup dipindai di segmen metadata, lalu gambar didekode penuh. Saat disajikan: content type dari deteksi ulang, `nosniff`, `CSP: default-src 'none'; sandbox`.
- Tanda tangan URL: HMAC-SHA256 dengan framing panjang-prefiks atas id, exp, viewer, dan varian, jadi kedaluwarsa tidak bisa diperpanjang dan `v=wm` tidak bisa dibuang. `exp` harus kanonik dan varian tak dikenal ditolak.
- `CanView`: stage tak dikenal dan KYC ditolak (handler memberi 404 yang sama dengan "tidak ada"). Booking nil, booking tidak cocok, atau `BookingID` kosong ditolak untuk non-admin. Viewer kosong atau nil ditolak.
- Header cache: foto bukti `private` (bertanda tangan: `max-age` sampai exp; sesi: `no-cache`), dan hanya `referensi` yang `public, immutable`. Respons galat `no-store`.
- SSRF S3: endpoint hanya dari env operator, tidak dari input pengguna.
- Fuzz lokasi: offset deterministik dari id listing + sel 400 m. Membalik offset paling jauh hanya mengungkap sel, yang memang informasi publik yang dimaksud, dan rata-rata banyak respons tidak mengungkap apa-apa. Kebocoran nyata ada di pencarian inti (C1), bukan di `geo`.
- Integritas: hash SHA-256 asli dicek setiap kali foto disajikan.

**Realtime/notify**

- Event ke user lain: pesan hanya `{"type":"changed","version":n}`, dikirim ke user yang terdaftar di event, admin (kalau `Admins`), atau semua (kalau `Public`). Tamu hanya menerima `Public`. Data sebenarnya diambil lewat snapshot yang terotorisasi.
- Origin WS: daftar eksplisit atau host yang sama. `null` dan origin tak valid ditolak. Cookie `SameSite=Lax` tidak terkirim pada handshake lintas situs.
- DoS: `SetReadLimit(1024)`, write timeout, antrean per koneksi (klien lambat diputus), 5 koneksi per user, dan tamu dibatasi (R1).
- SSRF push: endpoint wajib `https`, tanpa userinfo, ≤ 1024 karakter, dan host di allow-list layanan push browser (atau subdomainnya). Panjang kunci p256dh/auth divalidasi.
- Injeksi header email: subject diratakan (newline dibuang) lalu di-Q-encode. Alamat diparse `net/mail`. Body HTML lewat `html/template` (escape dan sanitasi URL) dan body teks lewat `text/template` dalam quoted-printable.
- Outbox: `UNIQUE(notification_id, channel)` mencegah dobel saat enqueue. Klaim pakai `FOR UPDATE SKIP LOCKED` dengan lease 5 menit, lebih besar dari waktu batch terburuk (20 job, 4 worker, 30 s per kirim, sekitar 150 s). Hasil dicatat walaupun ctx dibatalkan. At-least-once didokumentasikan, dan Resend memakai `Idempotency-Key`.
- Zona waktu preferensi: `time.LoadLocation` menolak `..` dan path absolut, dan `Local` ditolak.

**Platform**

- `X-Forwarded-For`/`CF-Connecting-IP` hanya dipercaya kalau peer ada di `TRUSTED_PROXY_CIDRS`. XFF ditelusuri dari kanan.
- Panic: `Recover` membalas pesan generik dan detail hanya ke log. `ErrAbortHandler` diteruskan.
- Body limit: `Content-Length` berlebih ditolak sebelum handler, dan body chunked dibungkus `MaxBytesReader`. Pencocokan path memakai `URL.Path`, dan path tidak kanonik di-redirect oleh `ServeMux` sebelum handler jalan.
- Label metrics hanya berisi pola route `ServeMux` (atau `unmatched`), metode yang dinormalisasi, dan kode status, jadi kardinalitasnya terbatas.
- `X-Request-ID` divalidasi (charset dan panjang), jadi tidak bisa dipakai untuk injeksi log atau header.
- Server metrics default di `127.0.0.1:9090`, dan semua server punya `ReadHeaderTimeout`.

**SQL/migrasi**

- Semua query memakai parameter. Satu-satunya SQL dinamis adalah nama kolom (`ktp_object`/`selfie_object`) dari konstanta dan `submissionCols`/`prefsColumns` yang juga konstanta.
- FK ke `users(id)` `ON DELETE CASCADE` untuk sesi, consent, submission, identity, push, prefs, dan outbox. `identities.submission_id ON DELETE SET NULL`.
- Constraint: status/doc/channel lewat `CHECK`, `attempts >= 0`, `expires_at > created_at`, satu submission `pending` per user, `UNIQUE(nik_hmac)`.
- Indeks tersedia untuk semua query panas (rate limit OTP, lookup token, antrean KYC, purge, klaim outbox).
- Down migration diuji (`authtest.Migrate` menjalankan up, down, lalu up lagi).
- UU PDP: hapus akun lewat cascade + `EraseUser` (K2). Outbox masih perlu job pembersih (L4).

## 4. Tes yang dijalankan

```
export TEST_DATABASE_URL=$(scripts/test-db.sh up)
scripts/go.sh vet ./...                       # bersih
scripts/go.sh test ./...                      # semua ok (termasuk integrasi auth/kyc/notify/service/httpapi/db)
scripts/go.sh test -race ./internal/{auth,kyc,crypto,media,realtime,platform}/...   # ok
golangci-lint v2.15.0 (paket di atas)         # hanya misspell tersisa
```

Tes baru per temuan:

| Temuan | Tes |
|---|---|
| M1 | `TestInspectRejectsDecompressionBombBeforeDecoding`, `TestInspectAcceptsLargestAllowedPhoto`, `TestWatermarkRefusesOverBudgetOriginal`, `TestDecodesAreSerialized` |
| A1 | `TestValidateConfigRejectsUnknownAppEnv` |
| P1 | `TestByIPGroupsIPv6By64`, `TestIPBucket` |
| A2 | `TestOTPPerIPLimitGroupsIPv6By64` |
| A3 | `TestClientIPIgnoresSpoofedHeaders` |
| A4 | `TestMiddlewareRecordsUserForPlatform` |
| A5 | `TestCSRFRejectsNullOrigin`, `TestCSRFLoginFromForeignOriginRefused` |
| R1 | `TestGuestConnectionsCappedByDefault` |
| K1 | `TestPurgeContinuesPastAFailingSubmission` |
| K2 | `TestEraseUserRemovesIdentityData` |
| K3 | `TestReadFileAuditFailureLogIsNotInjectable` |

Semuanya gagal di kode lama dan lulus setelah perbaikan.

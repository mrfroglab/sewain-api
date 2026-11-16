# Review keamanan gelombang 2 (F5.8, inti v2)

Reviewer independen untuk **inti** yang sudah terintegrasi di `develop`: `internal/service` (semua aksi, dispatch,
snapshot per viewer, job, idempotensi, pencarian, ekspor/hapus akun, impor CSV, retensi), `internal/domain`,
`internal/httpapi`, `internal/app`, `internal/config`, dan query `internal/db/queries`. Acuan: OWASP ASVS L1, PRD §8.3
(state machine), §11 (sengketa), FR-S6/FR-P/FR-L4, dan tabel visibilitas ROADMAP §4.

Cara kerja sama dengan gelombang 1: semua berkas inti dibaca utuh, setiap dugaan dibuktikan dengan tes yang gagal di
kode lama, lalu diperbaiki dengan perubahan sekecil mungkin. Satu commit per temuan (`fix(<area>): …`) dan tesnya ikut
di commit itu (berkas `wave2_*_test.go`). Dugaan yang ternyata aman dicatat di §3. Branch sudah berisi `develop`
terbaru (termasuk tune-limits, `a329922`); area rate limit itu hanya dibaca sekilas (lihat L6).

Severity sama dengan gelombang 1: **tinggi** = bisa dieksploitasi dari luar tanpa syarat khusus, dampak besar.
**Sedang** = butuh kondisi tertentu atau dampaknya terbatas. **Rendah** = dampak kecil atau butuh akses admin.
**Info** = pengerasan atau catatan.

Hasil: **tidak ada temuan tinggi**. Sembilan temuan sedang dan tiga rendah diperbaiki; sisanya rendah/info dan dicatat.
Tindak lanjut worker `polish` (§5): L1, L2, L3, L4, L6, dan I1 kini diperbaiki. L5 dan I2 tetap diterima.

## 1. Tabel temuan

| ID | Area | Severity | Ringkasan | Skenario serangan | Status | Commit |
|---|---|---|---|---|---|---|
| V1 | snapshot | **sedang** | `listing.meetingPoint` dikirim ke tamu dan semua user. | FR-S6: "titik temu detail baru terbuka setelah ACCEPTED". Siapa pun (tanpa login) membaca titik temu semua penyedia (mis. "Warung Bu Tini, Klebengan", alamat toko/kos) dari `GET /api/v1/state`, padahal koordinat sengaja dibulatkan. | diperbaiki: `meetingPoint` dikosongkan untuk non-pemilik non-admin. Penyewa menerimanya lewat `booking.meetingPointDetail` setelah disetujui (web sudah memakai field ini). | `786146f` |
| T1 | aksi booking | **sedang** | `confirmCollateral` bisa diulang tanpa batas; tiap panggilan menaikkan `rentCount` publik listing dan mengirim notifikasi ke penyedia. Juga di booking `RESOLVED`. Sebaliknya job auto-complete tidak pernah menghitung sewa. | Penyewa (atau penyedia yang kolusi dengan akun kedua) memompa "disewa N kali" sebuah listing jadi ratusan, dan men-spam penyedia (push/email lewat outbox). | diperbaiki: konfirmasi kedua ditolak ("Jaminan sudah dikonfirmasi."), hitungan hanya saat transisi ke `COMPLETED`, auto-complete ikut menghitung. | `7074ce1` |
| B1 | aksi listing | **sedang** | `updateListing` tidak menerapkan aturan `createListing`: persetujuan manual untuk barang bernilai tinggi/kendaraan (PRD 11.4) dan stok 1 untuk akun individu. | Buat listing murah, lalu update `itemValue` 50 jt + `approval: otomatis` → booking barang mahal disetujui otomatis tanpa peninjauan penyedia. Akun individu menaikkan stok ke 5 dan menerima 5 booking bertumpuk untuk satu barang. | diperbaiki | `eaceee7` |
| M1 | domain/uang | **sedang** | `ValidateListing` hanya memeriksa harga harian dan nilai barang > 0. Booking menerima tanggal kembali berapa pun. | Batas denda telat 500% nilai barang tercetak di kesepakatan (PRD: maks 100%); diskon durasi 150% → sewa negatif; denda/harga mingguan/deposit negatif; deposit 300%; harga ≥ 4×10^18 → `days × price` overflow int64; radius 10.000 km; judul 10.000 karakter; `minDays > maxDays`; jenis jaminan/serah terima tak dikenal (500 dari CHECK DB). Booking sampai `9999-12-31` memblokir stok bertahun-tahun dan overflow. | diperbaiki: batas uang (`MaxRupiah` Rp10 miliar, persen 0–100, diskon 0–100 eksklusif), rentang (hari 1–365, radius ≤ 50 km, stok ≤ 1000, rating 1–5, lat/lng valid), panjang teks, jenis jaminan/serah terima, jumlah foto/opsi. Booking harus selesai ≤ 365 hari dari sekarang. | `47f93a1` |
| E1 | aksi booking/sengketa | **sedang** | Id foto bukti (check-out, foto tambahan, check-in, chat, sengketa) hanya dicek "ada". `openDispute` tidak mengecek sama sekali. | Penyedia menyerahkan barang dengan foto kondisi dari booking lama (kondisi lama, timestamp server lama), foto lepas milik user lain, atau foto katalog listing; tujuan FR-P2/P3 (bukti bertimestamp server) gugur saat sengketa. | diperbaiki: foto harus diunggah untuk booking ini (oleh pihak mana pun) atau oleh aktor tanpa booking, dan bukan tahap `referensi`/`ktp`/`selfie`. Web sudah mengunggah bukti dengan `bookingId`. | `c5aab30` |
| C2 | db/stok | **sedang** | `CountCommittedOverlaps` hanya menghitung `ACCEPTED/ACTIVE`. | Booking yang disengketakan saat barang masih di penyewa (`DISPUTED`, `returned_at` NULL) melepas unitnya: tanggal yang sama bisa diminta dan disetujui lagi (double booking), padahal `listingBusy` di snapshot sudah menandainya terpakai. | diperbaiki: `DISPUTED` yang belum kembali ikut memegang stok. `dbq` diregenerasi. | `2ac0406` |
| P1 | httpapi/produksi | **sedang** | `POST /api/v1/photos/demo` ("Pakai foto contoh") tidak dibatasi `DEMO_MODE`. | Di staging/produksi siapa pun yang login mendaftarkan gambar stok `/img/…` sebagai bukti check-out/check-in dengan hash + timestamp server, menggantikan foto barang asli. Server juga mengambil `WEB_ORIGIN + path` milik klien. | diperbaiki: 403 di luar DEMO_MODE. **Web**: sembunyikan tombol "Pakai foto contoh" bila `demoMode` false (`GET /api/v1/config`). | `e7d4c3d` |
| C3 | config/produksi | **sedang** | `APP_ENV=production` menerima `SESSION_COOKIE_SECURE=false`. | Salah konfigurasi membuat cookie sesi (D6) ikut terkirim lewat HTTP polos (mis. kunjungan `http://` sebelum redirect), lalu disadap di Wi-Fi kampus. | diperbaiki: produksi menolak nilai selain `true`. | `1b86be5` |
| K1 | domain/moderasi | **sedang** | Filter kata terlarang (FR-L4) hanya membandingkan kata a–z/0–9 utuh, dan hanya judul + deskripsi. | Lolos: `a i r s o f t`, `A.I.R.S.O.F.T`, `air-soft`, huruf lebar penuh `ＡＩＲＳＯＦＴ`, aksen `aírsoft`, huruf Kiril `Аirsoft`, spasi lebar nol, dan kata terlarang di catatan kondisi/aturan khusus. | diperbaiki: teks dan kata kunci dilipat sama (lebar penuh, aksen, homoglif Kiril/Yunani), dicocokkan juga dalam bentuk ringkas (tanda baca di dalam kata dibuang, huruf tunggal berspasi digabung). Kata yang sekadar mengandung kata kunci ("airy software", "air, soft") tetap lolos. Catatan kondisi + aturan khusus ikut diperiksa. Leetspeak (`4irs0ft`) belum (lihat L3). | `c5548f5` |
| R1 | aksi booking/job | rendah | `acceptBooking` hanya memeriksa status `REQUESTED`, bukan `expires_at`. | Antara `expires_at` dan job berikutnya (interval 15 s, runner mati, banyak instance) penyedia masih bisa menyetujui request yang menurut FR-R3 sudah kedaluwarsa. | diperbaiki: "Request ini sudah kedaluwarsa." | `3405dea` |
| D1 | hapus akun (UU PDP) | rendah | `DELETE /api/v1/me` mengosongkan `users.nik_masked` tetapi `verification_requests` tetap menyimpan nama di KTP dan NIK tersamar. | Admin (dan ekspor admin) tetap melihat identitas akun yang sudah dihapus. | diperbaiki: `name_on_ktp`/`nik_masked` dikosongkan (riwayat tetap ada), `idempotency_keys` user dihapus. | `3df25c8` |
| W1 | aksi papan permintaan | rendah | `offerWanted` menerima `listingId` apa pun. | Menawarkan listing penyedia lain (peminta diarahkan ke listing itu atas nama penawar), atau listing tersembunyi/tak ada. | diperbaiki: hanya listing aktif milik sendiri. | `d442b1b` |
| L1 | aksi admin | rendah | `adminRecommend`/`resolveDispute`/`resolveReport`/`reviewVerification` (simulasi) tidak memeriksa status sebelumnya; `updateCommission` menerima `min > max` dan `min` negatif. | Hanya admin: sengketa yang sudah selesai bisa "dibuka" lagi ke mediasi, laporan diputus dua kali, komisi dengan min > max (hasil = max). | diperbaiki (polish): rekomendasi/hasil atas sengketa `selesai`, keputusan kedua atas laporan/pengajuan verifikasi ditolak dengan pesan jelas; komisi menolak `min > max`, nominal negatif, dan di atas `MaxRupiah`. | `613484e` |
| L2 | input | rendah | Teks bebas di luar listing tanpa batas panjang selain body limit: pesan chat, alasan pembatalan/penolakan, deskripsi sengketa, catatan usulan, laporan, papan permintaan; `proposeSolution.amount` dan `wanted.budgetPerDay` boleh negatif/raksasa; `createReport` tidak memeriksa target ada. | Penyimpanan membengkak dan notifikasi panjang (dibatasi `BodyLimit` 1 MB per request + rate limit write). | diperbaiki (polish): batas panjang (`domain/limits.go`) untuk pesan chat, ulasan, catatan booking, alasan tolak/batal/lapor/strike/status, deskripsi sengketa, usulan, rekomendasi/hasil admin, papan permintaan + tawaran, nama, bio, nama toko; nominal usulan dibatasi PRD §11.3 (potong deposit ≤ deposit, ganti perbaikan/nilai ≤ nilai barang di kesepakatan, tanpa kompensasi tanpa nominal, tidak negatif), rekomendasi admin ≤ maks(nilai barang, deposit); `budgetPerDay` 0–`MaxRupiah`; target laporan wajib ada. | `613484e` |
| L3 | moderasi | rendah | Filter kata belum menangani leetspeak (`4irs0ft`, `a1rsoft`) dan homoglif di luar tabel. | Bisa dilewati dengan sedikit usaha; filter ini garis pertama, laporan pengguna + admin tetap jalan. | diperbaiki (polish): normalisasi sederhana di dalam kata yang memuat huruf (0→o, 1→i/l, 3→e, 4→a, 5→s, 7→t) + huruf berulang dipadatkan. Angka berdiri sendiri tidak diubah. Semua judul/deskripsi/catatan seed jadi tes negatif. Homoglif di luar tabel tetap lewat laporan + admin. | `c77aa03` |
| L4 | listing | rendah | `listing.photos` berisi string bebas (URL absolut pun diterima), juga lewat impor CSV. | Pemilik listing memasang URL eksternal sebagai foto → browser setiap pengunjung memuat gambar dari server penyerang (pelacakan IP), dan gambar tidak lewat `media.Inspect`. Tergantung CSP `img-src` web. | diperbaiki (polish): `createListing`/`updateListing`/impor CSV hanya menerima `/api/v1/photos/<id>` atau id foto tahap `referensi` yang diunggah pemilik (atau admin yang mengedit/mengimpor), dan `/img/…` hanya di DEMO_MODE. Foto yang sudah ada di listing tetap boleh dipertahankan. URL luar, `//host`, `data:`, query string ditolak. Tanpa migrasi data (lihat §5). | `a61ca8b` |
| L5 | hapus akun | rendah | Race sempit: `createBooking` yang sedang memegang kunci listing saat `DeleteAccount` berjalan masih bisa commit setelah pengecekan "tidak ada booking berjalan". | Akun terhapus dengan satu booking `REQUESTED` (kedaluwarsa sendiri 24 jam kemudian). | diterima. | – |
| L6 | platform (tune-limits) | rendah | Sejak `a329922` limit publik berjalan **setelah** `auth.Middleware`. | Request dengan token/cookie acak memicu lookup sesi di DB sebelum ada limit per-IP. Lookup memakai indeks unik (murah), tetapi tidak ada rem sama sekali di depannya. | diperbaiki (polish): profil `ip` (6000/menit, burst 3000, `RATE_IP_PER_MIN`/`RATE_IP_BURST`) dipasang sebelum `auth.Middleware`. | `07eaa82` |
| I1 | impor CSV | info | Impor menerima serah terima `antar`, sedangkan web memakai `cod/jemput/kurir`. | Listing hasil impor dengan `antar` tidak cocok dengan opsi web. | diperbaiki (polish): satu daftar `domain.HandoverMethods` (`cod`, `jemput`, `kurir`, `antar`, sama dengan `HandoverMethod` web) untuk validasi listing dan impor; impor kini menerima `kurir` dan tidak peka huruf besar. | `8a5eade` |
| I2 | realtime | info | WebSocket yang sudah terbuka tetap menerima event `changed` setelah sesi dicabut (blacklist/hapus akun) sampai tersambung ulang. | Hanya nomor versi, tanpa data; snapshot sesudahnya ditolak sebagai tamu. | diterima. | – |

## 2. Dampak perubahan untuk web dan operator

- Snapshot: `listing.meetingPoint` hanya untuk pemilik/admin. Tampilan penyewa memakai `booking.meetingPointDetail`.
- Aksi baru menolak (pesan Bahasa Indonesia): konfirmasi jaminan kedua, accept setelah kedaluwarsa, foto bukti dari
  booking lain/foto referensi, tawaran papan dengan listing orang lain, listing di luar batas (§1 M1), booking yang
  selesai lebih dari 365 hari ke depan, kata terlarang di catatan kondisi/aturan khusus.
- `POST /api/v1/photos/demo` → 403 di luar DEMO_MODE.
- Produksi menolak start dengan `SESSION_COOKIE_SECURE` selain `true`.
- `domain.MaxRupiah`, `MaxRentalDays`, `MaxListingStock`, `MaxListingRadius` diekspor untuk dipakai web/CSV.

## 3. Dicek, aman

**Otorisasi / IDOR**

- Aktor = pemilik token: `dispatch` menolak (`ErrForbidden` → 403) bila argumen pertama ≠ `viewer.UserID`, untuk
  semua aksi kecuali `registerUser` (demo saja). Tamu tidak bisa memanggil aksi apa pun selain itu.
- Aksi admin memeriksa role dari DB (`isAdmin`), bukan dari klaim sesi; `reviewVerification` jalur KYC memeriksa
  viewer = argumen + role admin sebelum `kyc.Decide`.
- Aksi booking memakai `partyBooking` (FOR UPDATE + cek pihak dengan peran) atau cek eksplisit `ProviderID`;
  pihak luar mendapat "Booking tidak ditemukan." (tidak membedakan ada/tidak). Sengketa lewat `disputeForParty`.
  `markThreadRead` pihak/admin saja, `markNotificationsRead` difilter `user_id`, `closeWanted` hanya peminta,
  `updateListing`/`setListingStatus` hanya pemilik/admin dan `diturunkan` hanya admin.
- State machine PRD §8.3: check-out hanya `ACCEPTED`, check-in hanya `ACTIVE` oleh penyedia, ACTIVE butuh dua
  persetujuan, rating hanya `COMPLETED/RESOLVED` sekali per pihak (dicek + `UNIQUE`), sengketa hanya
  `ACTIVE/RETURNED` dan satu yang berjalan, `resolveDispute` hanya admin, respons usulan bukan oleh pengusul.
- Id buatan klien (`PickID`): pola `^[a-z]{1,4}-[a-z0-9]{6,32}$`, prefiks wajib, dan dipakai hanya bila belum ada;
  bentrok saat insert = unique violation (500), tidak pernah menimpa. `u-admin` tidak cocok pola sehingga tidak bisa
  dibuat lewat id klien.
- Unggah foto ke booking hanya pihak/admin (`CanUploadTo`); `media.CanView` menegakkan akses bukti berdasarkan booking
  milik foto itu sendiri, jadi merujuk id foto orang lain di pesan tidak membuka aksesnya (ditambah E1).

**Snapshot per viewer** (ROADMAP §4, diuji `TestSnapshotPrivacy` + tes baru)

- Profil orang lain = `publicProfile` (tanpa HP, email, NIK, metode/waktu verifikasi, NIB toko). `publicStats.strikes`
  publik = keputusan koordinator #6 (`sewain-web-v2-api-needs.md`), detail strike tetap milik sendiri.
- Booking, pesan, foto bukti, sengketa (+ usulan + foto), notifikasi, favorit, laporan, strike, verifikasi: query
  `…OfUser`/`…VisibleTo` difilter viewer. `chatReads` hanya untuk booking tempat viewer jadi pihak. Audit hanya admin.
- Ulasan: `released OR from_id = viewer` (ulasan untuk saya yang belum dirilis tidak terlihat, FR-T2).
- Koordinat: listing orang lain dan papan permintaan memakai `geo.FuzzPoint`; `listingBusy` tanpa identitas;
  kesepakatan hanya memuat nama publik; pencarian hanya dari titik publik dengan radius bertingkat (C1 gelombang 1,
  tes trilaterasi). Satu-satunya kebocoran lokasi tekstual adalah V1.
- `SNAPSHOT_SCOPE=full` hanya berlaku bila `DEMO_MODE` (`SnapshotScope()`), ditolak di produksi oleh config.
- ETag = hash(versi, scope, `userId/role`, offset jam) + jendela tanda tangan 15 menit, jadi viewer berbeda tidak
  pernah berbagi ETag; `Cache-Control: no-cache` + `Vary: Authorization, Cookie`. 304 hanya untuk ETag viewer itu.
- WebSocket: hanya `{"type":"changed","version":n}` ke `Change.Users` + admin, ke semua hanya bila `Public`
  (tes `TestEventsOnlyToParties`).
- `GET /api/v1/me/export`: dibangun dari snapshot scoped viewer sendiri; pesan pihak lawan dibuang, pihak lain hanya
  id + nama publik, data modul tanpa hash token/NIK/kunci push, `Cache-Control: no-store`.

**Integritas transaksi**

- Stok/bentrok tanggal: `createBooking` dan `acceptBooking` mengunci listing (`FOR UPDATE`) sebelum menghitung
  overlap; tes paralel `TestConcurrentCreateBookingRespectsStock`/`…AcceptRespectsStock`. (C2 menambal status.)
- Idempotensi: kunci per `(user, key)`, diklaim di transaksi aksi (efek + respons commit bersama), duplikat paralel
  menunggu baris kunci lalu me-replay; hash = argumen + id klien, jadi kunci sama dengan argumen lain → 409.
- Job vs aksi: `ListBookingsDueForJobs`/`ListDisputesDue` memakai `FOR UPDATE SKIP LOCKED`; aksi mengunci baris yang
  sama, sehingga accept dan expire tidak bisa sama-sama menang; job juga membaca ulang status terbaru (READ COMMITTED +
  EvalPlanQual). Ditambah R1 untuk jeda sebelum job jalan.
- Retry: `db.WithTx` mengulang seluruh fungsi; notifikasi, outbox (hook `OnNotification` di transaksi yang sama),
  pesan sistem, dan audit ikut di-rollback, dan listener realtime dipanggil hanya setelah commit, jadi tidak ada efek
  ganda.
- Uang: semua rupiah `int64`; persen dibulatkan sekali (`math.Round`) dan pembagian komisi `charged − renter` sehingga
  jumlahnya selalu utuh; komisi hanya dari sewa (FR-K4). Overflow/negatif ditutup M1.

**Input**

- SQL: semua query sqlc berparameter; SQL mentah di service hanya konstanta dengan placeholder. Full-text: `TSQuery`
  hanya meloloskan huruf/angka + `:*`/`&` buatan sendiri, dan tetap dikirim sebagai parameter.
- Rating di luar 1–5 ditolak (aksi + `CHECK`), tanggal terbalik/lewat ditolak, radius pencarian dibatasi 50 km dan
  dibulatkan ke opsi.
- Impor CSV admin: 5 MB (`MaxBytesReader` + `LimitReader`), ≤ 1.000 baris, semua-atau-tidak, validasi per kolom +
  `ValidateListing` (kini dengan batas M1). Tidak ada ekspor CSV di API, jadi CSV injection (`=CMD…`) tidak berlaku;
  ekspor dan metrik berbentuk JSON.

**Hapus akun / UU PDP**

- Anonimisasi satu transaksi (nama, HP, email, bio, kampus, NIK tersamar, sesi, push, preferensi, outbox,
  notifikasi, favorit, OTP; listing diturunkan, papan ditutup), lalu `RevokeAllSessions` + `kyc.EraseUser`; gagal
  erase diulang runner retensi (`DeletedAccountsWithKYC`). Ditolak bila ada booking berjalan/sengketa terbuka.
  Ditambah D1.
- Retensi foto bukti (`PurgeEvidencePhotos`) hanya booking `COMPLETED/RESOLVED` ≥ 90 hari dan `media.PlanPhotoRetention`
  menahan foto yang dipegang sengketa; penghapusan baris hanya dengan `SET LOCAL sewain.retention`.

**Produksi**

- Config menolak di produksi: `DEMO_MODE=true`, `SNAPSHOT_SCOPE=full`, `PAYMENT_MODE=simulated|gateway`, OTP
  `demo|echo`, `CORS_ORIGINS=*`, kunci yang hilang, `APP_ENV` tak dikenal, `SESSION_BEARER_LOGIN=true`, dan kini
  cookie tidak Secure (C3).
- `DEMO_MODE` hanya dari env `DEMO_MODE` (persis `true`; nilai lain = mati) di config dan auth, keduanya dicek terhadap
  `APP_ENV=production`. Tidak ada jalur lain (header, setting DB, aksi).
- Kontrol demo: `advanceClock`/`resetClock`/`resetDemo` (403 di luar demo), `demo-login`, `registerUser`,
  `submitVerification` simulasi, `SeedIfEmpty`, dan kini `photos/demo` (P1) semuanya tertutup di luar DEMO_MODE.

## 4. Tes yang dijalankan

```
export TEST_DATABASE_URL=$(scripts/test-db.sh up)
scripts/go.sh vet ./...                                   # bersih
scripts/go.sh test -p 1 ./...                             # semua ok (termasuk integrasi)
scripts/go.sh test -p 1 -race -count=1 ./internal/{service,domain,httpapi,config,app}/   # ok, tanpa data race
golangci-lint v2.15.0 run ./...                           # 0 issues
make generate-check                                       # tanpa diff
```

`-p 1`: container tes dibatasi 512 MB; paket integrasi yang paralel sempat membuat Postgres di-OOM-kill.

| Temuan | Tes (semua gagal di kode lama, lulus setelah perbaikan) |
|---|---|
| V1 | `TestSnapshotHidesMeetingPointFromOthers` |
| T1 | `TestConfirmCollateralCountsOnce`, `TestAutoCompleteCountsRental` |
| B1 | `TestUpdateListingKeepsApprovalAndStockRules` |
| M1 | `TestListingMoneyAndRangeBounds`, `TestBookingDateHorizon` |
| E1 | `TestEvidencePhotosBelongToTheBooking` |
| C2 | `TestDisputedBookingHoldsStock` |
| P1 | `TestDemoPhotoOnlyInDemo` (internal/app, staging) |
| C3 | `TestProductionRefusesInsecureSessionCookie` (internal/config) |
| K1 | `TestBannedKeywordsBypass` |
| R1 | `TestAcceptAfterExpiryRefused` |
| D1 | `TestDeleteAccountErasesVerificationRequests` |
| W1 | `TestOfferWantedOnlyOwnListing` |

## 5. Tindak lanjut (worker `polish`, sebelum go-live)

| Temuan | Status | Tes (gagal di kode lama, lulus sesudahnya) |
|---|---|---|
| L1 | diperbaiki | `TestAdminActionsOnFinalEntities`, `TestUpdateCommissionBounds` |
| L2 | diperbaiki | `TestFreeTextLimits`, `TestProposalAmountBounds` |
| L3 | diperbaiki (normalisasi sederhana) | `TestBannedKeywordsLeetspeak`, `TestBannedKeywordsNoFalsePositiveOnSeed` (internal/domain) |
| L4 | diperbaiki | `TestListingPhotosMustBeOwnUploads` |
| L5 | diterima (tidak berubah) | – |
| L6 | diperbaiki | `TestIPBrakeBeforeAuth` (ratelimit), `TestFakeTokenFloodStopsBeforeAuth` (app); `TestCampusNATDefaults` dan `TestCampusNATNoFalsePositives` tetap lulus |
| I1 | diperbaiki | `TestImportHandoverMethods` |
| I2 | diterima (tidak berubah) | – |
| `DELETE /api/v1/me` | sudah benar, kini diuji lewat HTTP | `TestDeleteAccountBlockedUntilSettled` (409 saat sewa berjalan / sengketa terbuka, 200 setelah dibatalkan / diselesaikan) |

Catatan:

- **L1 jalur KYC.** `reviewVerification` atas pengajuan yang punya `kyc_submission_id` diteruskan ke `kyc.Decide`,
  yang sudah menolak keputusan kedua (`curStatus != pending`). Perbaikan L1 menutup jalur simulasi/manual.
- **L2 dampak web.** Batas sama dengan form web atau lebih longgar. Pesan galat: "`<Kolom>` terlalu panjang (maks N
  karakter)." Usulan "potong deposit" pada booking tanpa deposit ditolak ("Booking ini tidak memakai deposit."), sama
  dengan `ProposalSheet` web yang menonaktifkan pilihan itu. "Perpanjang" dibatasi nilai barang.
- **L3 dampak web.** Filter di web (konfigurasi `bannedKeywords`) belum memakai normalisasi ini, jadi server bisa
  menolak judul yang lolos di klien. Pesannya tetap "Ada kata yang dilarang: …".
- **L4 tanpa migrasi data.** Listing lama yang sudah memuat URL luar (hanya mungkin lewat impor CSV atau aksi API
  langsung sebelum perbaikan) tetap tampil sampai diedit. Saat diedit, URL luar ditolak walaupun sudah ada. Cek
  sebelum go-live: `SELECT listing_id, src FROM listing_photos WHERE src NOT LIKE '/api/v1/photos/%' AND src NOT LIKE '/img/%';`.
  Kalau ada hasilnya, turunkan listing itu atau hapus fotonya secara manual. Migrasi otomatis tidak dibuat karena
  bisa membuat listing kurang dari 3 foto tanpa sepengetahuan pemilik.
- **L6.** Respons 429 dari rem ini dihitung di `sewain_rate_limited_total{profile="ip"}`.

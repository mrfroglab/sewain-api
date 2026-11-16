# Integrasi paket `auth`, `kyc`, `crypto` (Fase 2)

Paket mandiri dari worker `auth`. Belum dipasang ke server: koordinator/`core-v2` yang memasangnya di `cmd/api`
dan `internal/httpapi`. Dokumen ini berisi semua yang dibutuhkan untuk itu.

| Paket | Isi |
|---|---|
| `internal/crypto` | `KeyRing` AES-256-GCM (envelope berisi id kunci, bisa dirotasi), `HMAC`, `SHA256`, decode kunci/pepper |
| `internal/auth` | OTP (F2.1), sesi + middleware + CSRF (F2.2), consents (F2.3), `RequireUser`/`RequireAdmin`/`CheckAdmin`, `GrantAdmin`/`RevokeAdmin` (F2.5) |
| `internal/auth/authtest` | Fixture tes integrasi: schema Postgres sekali pakai + goose (rentang 00100–00119), tabel `users` minimal, `Directory`, jam palsu |
| `internal/kyc` | Pengajuan KTP + selfie terenkripsi, satu NIK satu akun, blacklist, antrean admin, akses berkas ber-audit, purge 30 hari (F2.4), `KYCProvider` + `ManualProvider` (F2.6) |
| `internal/kyc/kyctest` | `MemBlobStore`, `MemAuditor`, `ProviderContract` (tes kontrak untuk vendor eKYC Fase 8) |
| Migrasi | `00100_auth_otp_sessions_consents.sql`, `00101_kyc_identities.sql` (FK ke `users(id)`, jadi harus jalan setelah migrasi inti yang membuat `users`; goose dengan `allow out-of-order`) |

Dependensi baru: `github.com/pressly/goose/v3 v3.30.1` (dipakai fixture tes. Inti juga memakainya untuk migrasi,
D4). Efek sampingnya `golang.org/x/sync` naik ke v0.25.0 dan `golang.org/x/text` ke v0.44.0 (MVS).

## 1. Env baru

Buat kunci dan pepper dengan `openssl rand -base64 32` (32 byte acak, base64).

| Env | Wajib | Default | Keterangan |
|---|---|---|---|
| `OTP_PROVIDER` | ya (prod/staging) | `demo` kalau `DEMO_MODE=true`, `echo` di `development` | `demo` (kode tetap `123456`, tidak mengirim apa pun, **hanya** dengan `DEMO_MODE=true`), `echo` (kode acak dikembalikan sebagai `devCode`, untuk staging), `zenziva` (WA lalu SMS). `demo`/`echo` **ditolak** saat `APP_ENV=production` |
| `OTP_PEPPER` | ya di staging/production | acak sementara di dev/demo | Kunci HMAC untuk hash kode OTP. ≥ 32 byte base64 |
| `KYC_KEYS` | ya di staging/production | kunci acak sementara di dev/demo | `v1:<base64 32 byte>,v2:<base64 32 byte>`. Yang **terakhir** = kunci aktif untuk data baru. Kunci lama **jangan dihapus** selama masih ada data yang dienkripsi dengannya |
| `NIK_PEPPER` | ya di staging/production | acak sementara di dev/demo | Kunci HMAC NIK (unik + blacklist). **Tidak bisa dirotasi**: kalau berubah, aturan satu NIK satu akun dan blacklist lama tidak berlaku. Simpan di 2 tempat (ROADMAP §8) |
| `SESSION_COOKIE_SECURE` | tidak | `true`, kecuali `APP_ENV` = `development`/`demo` | Flag `Secure` pada cookie `sewain_session` |
| `ALLOWED_ORIGINS` | ya di production | kosong (dev: host request sendiri) | Daftar origin dipisah koma, mis. `https://sewain.me,https://www.sewain.me`. Dipakai untuk cek CSRF |
| `TERMS_VERSION`, `PRIVACY_VERSION` | ya di production | `draft` | Versi dokumen yang dicatat ke `consents` saat akun baru dibuat |
| `SESSION_BEARER_LOGIN` | tidak | sama dengan `DEMO_MODE` | Mengizinkan `transport:"bearer"` di verify (token di body, untuk demo multi-akun per tab, D6). Ditolak di production |
| `CLIENT_IP_HEADER` | cadangan | kosong (pakai `RemoteAddr`) | Hanya dipakai kalau rantai platform (`middleware.RealIP` + `TRUSTED_PROXY_CIDRS`) tidak berjalan; kalau berjalan, IP dari `RealIP` yang dipakai. Untuk `X-Forwarded-For` diambil hop **terakhir**. Rate limit per IP menghitung IPv6 per /64 (`otp_challenges.ip` berisi prefiks /64 untuk IPv6) |

Variabel Zenziva (hanya kalau `OTP_PROVIDER=zenziva`):

| Env | Default | Keterangan |
|---|---|---|
| `ZENZIVA_WA_URL` | wajib | Endpoint kirim WhatsApp |
| `ZENZIVA_SMS_URL` | kosong | Endpoint SMS cadangan. Dipakai kalau WA gagal (non-2xx, timeout, atau `SUCCESS_FIELD` tidak cocok) |
| `ZENZIVA_USERKEY`, `ZENZIVA_PASSKEY` | wajib (kecuali `AUTH=none`) | Kredensial |
| `ZENZIVA_AUTH` | `body` | `body` (kredensial sebagai field), `basic`, `bearer` (userkey sebagai token), `none` |
| `ZENZIVA_BODY` | `form` | `form` (x-www-form-urlencoded) atau `json` |
| `ZENZIVA_FIELD_USERKEY` / `_PASSKEY` / `_TO` / `_MESSAGE` | `userkey` / `passkey` / `to` / `message` | Nama field |
| `ZENZIVA_PHONE_FORMAT` | `local` | `local` (`0812…`), `62` (`62812…`), `e164` (`+62812…`) |
| `ZENZIVA_MESSAGE` | "Kode masuk Sewain: {code}. Berlaku 5 menit. …" | Wajib memuat `{code}` |
| `ZENZIVA_SUCCESS_FIELD`, `ZENZIVA_SUCCESS_VALUE` | kosong | Kalau diisi, respons JSON harus punya field ini dengan nilai ini, kalau tidak dianggap gagal |
| `ZENZIVA_EXTRA` | kosong | Field tetap tambahan, `k=v;k2=v2` (mis. id brand/sender) |

> **Perlu diverifikasi dengan akun asli (ROADMAP §9 #2).** Adapter `zenziva` sengaja generik: tidak ada URL,
> nama field, atau format respons Zenziva yang di-hardcode, karena belum dicek ke akun dan dokumentasi resmi.
> Default nama field (`userkey`, `passkey`, `to`, `message`) hanya tebakan awal. Setelah akun ada: isi env di
> atas sesuai dokumentasi akun, kirim satu OTP ke nomor sendiri di staging (`OTP_PROVIDER=zenziva`), lalu cek
> jalur cadangan SMS dengan sengaja mengisi `ZENZIVA_WA_URL` yang salah. Perilakunya sudah dites dengan
> `httptest` (`internal/auth/config_test.go`).

Pemuatan config:

```go
authCfg, err := auth.ConfigFromEnv(os.Getenv)   // memanggil ValidateConfig, gagal start kalau demo/echo di production
sender, err := auth.NewSender(authCfg)
kycCfg, err := kyc.ConfigFromEnv(os.Getenv)
```

`auth.ValidateConfig(appEnv, demoMode, provider)` juga bisa dipanggil langsung dari `config.Load()` inti.

## 2. Memasang handler dan middleware

```go
authSvc, err := auth.NewService(pool, userDir, sender, authCfg) // pool = *pgxpool.Pool
kycSvc, err := kyc.NewService(pool, storage, auditor, kycCfg)  // storage = media.Storage, auditor = audit repo inti
kycSvc.Hooks = kyc.Hooks{OnSubmitted: core.OnKYCSubmitted, OnDecided: core.OnKYCDecided} // lihat §4

mux := http.NewServeMux()
auth.Handlers{S: authSvc}.Register(mux)
kyc.Handlers{S: kycSvc}.Register(mux)
mux.Handle("POST /api/v1/actions/{name}", actionsHandler) // handler inti
// …

handler := cors(authSvc.Middleware(mux)) // middleware membungkus SELURUH mux
```

- `authSvc.Middleware` membaca `Authorization: Bearer <token>` (prioritas) atau cookie `sewain_session`, lalu
  menaruh `auth.Viewer{UserID, Role, SessionID}` di context. Kredensial tidak valid/kedaluwarsa/dicabut = tamu
  (bukan error). Akun berstatus `blacklist` = tamu. Akun `suspend` tetap masuk, pembatasan aksinya urusan inti.
- Ambil viewer di handler inti: `v := auth.ViewerFrom(r.Context())`. `v.IsGuest()`, `v.IsAdmin()`.
- **CSRF:** request berbasis cookie dengan metode selain GET/HEAD/OPTIONS wajib punya `Origin` (atau `Referer`)
  yang ada di `ALLOWED_ORIGINS`, kalau tidak 403 `{"ok":false,"error":"Permintaan ditolak: asal permintaan tidak dikenal."}`.
  `Origin: null` selalu ditolak (tidak jatuh ke `Referer`). Request tanpa cookie dan tanpa bearer (mis. `/otp/verify`)
  yang membawa `Origin` asing juga ditolak 403 (login CSRF); klien tanpa header `Origin` (aplikasi, curl) tetap
  boleh. Jadi origin aplikasi mobile/Capacitor (`capacitor://localhost`, dll.) harus masuk `ALLOWED_ORIGINS`.
  Request bearer tidak dicek. CORS inti tetap perlu `AllowCredentials` untuk origin web kalau web dan API beda origin.
- Penjaga: `auth.RequireUser(h)`, `auth.RequireAdmin(h)` (401 tamu, 403 non-admin "Hanya admin."), dan untuk
  kode service (mis. dispatcher `POST /api/v1/actions/{name}` untuk aksi admin) `auth.CheckAdmin(v) error`.
  Semua fungsi admin di `kyc.Service` juga memanggil `CheckAdmin` sendiri (pertahanan berlapis).
- Hapus `X-Demo-Session`/login demo lama atau ganti dengan `authSvc.CreateSession(ctx, userID, meta)` untuk
  tombol "masuk sebagai" di DEMO_MODE.
- Job berkala (pakai advisory lock job inti, F1.7):
  - `kycSvc.PurgeFiles(ctx, now)` tiap jam: hapus berkas 30 hari setelah keputusan.
  - `authSvc.PurgeExpiredSessions(ctx, now.Add(-7*24*time.Hour))` dan
    `authSvc.PurgeOTPChallenges(ctx, now.Add(-24*time.Hour))` tiap hari (cutoff OTP minimal 24 jam ke belakang
    supaya plafon harian tetap benar).
- CLI `api admin grant <phone>` / `api admin revoke <phone>`: panggil `auth.GrantAdmin(ctx, pool, phone)` /
  `auth.RevokeAdmin(ctx, pool, phone)`. Fungsi ini menulis `users.role` langsung (`UPDATE users SET role=$2
  WHERE phone=$1 RETURNING id`). Kalau `users` inti memakai soft delete atau `updated_at`, sesuaikan query di
  `internal/auth/admin.go` saat integrasi (tambahkan `AND deleted_at IS NULL`). Pengguna harus sudah pernah masuk
  sekali. Catat ke audit log dari sisi CLI.

## 3. Mengimplementasikan `UserDirectory` di atas repo inti

```go
type UserDirectory interface {
    FindUserByPhone(ctx context.Context, phoneE164 string) (userID string, found bool, err error)
    CreateUser(ctx context.Context, tx pgx.Tx, phoneE164 string) (userID string, err error)
    GetUserAuth(ctx context.Context, userID string) (role string, status string, err error)
}
```

- `FindUserByPhone`: `SELECT id FROM users WHERE phone = $1 AND deleted_at IS NULL`. `found=false, err=nil`
  kalau tidak ada. Nomor yang masuk sudah E.164 (`+62…`), hasil `auth.NormalizePhone`.
- `CreateUser`: **pakai `tx` yang diberikan** (transaksi yang sama dengan pembuatan sesi + consents, dan FK
  `auth_sessions.user_id`/`consents.user_id` butuh barisnya terlihat di tx itu). Isi `id` berprefiks `u-…`,
  `phone`, `phone_verified_at = now()`, `role='user'`, `status='aktif'`, `verification_status` awal. Nama/profil
  diisi belakangan lewat aksi profil.
- `GetUserAuth`: `SELECT role, status FROM users WHERE id = $1`. Dipanggil di **setiap** request ber-sesi,
  jadi pastikan PK lookup saja (bisa di-cache sangat singkat kalau perlu, tetapi perubahan role/blacklist harus
  segera berlaku). Role selain `admin` diperlakukan sebagai `user`.
- Saat user di-blacklist atau dihapus, panggil `authSvc.RevokeAllSessions(ctx, userID)` dan
  `kycSvc.BlacklistUser(ctx, adminViewer, userID, reason)`.
- Contoh implementasi untuk tabel minimal ada di `internal/auth/authtest` (`Directory`).

## 4. Alur KYC ke `verification_requests` inti

1. User mengunggah (`POST /api/v1/kyc/submissions`). `kyc.Service.Submit` memvalidasi NIK (16 digit, kode
   provinsi 11–94, tanggal lahir 01–31/41–71, bulan 01–12, nomor urut ≠ 0000), nama, dan berkas (JPG/PNG/WebP
   dari byte asli, ≤ 8 MB), lalu:
   - mengunci (advisory lock) user dan HMAC NIK, menolak kalau NIK di `nik_blacklist` (403), sudah dipakai
     akun lain di `identities` atau pengajuan `pending` akun lain (409), user sudah terverifikasi (409), atau
     user masih punya pengajuan `pending` (409);
   - mengenkripsi NIK, nama, dan kedua berkas dengan `KYC_KEYS` (AAD = id pengajuan + jenis, jadi ciphertext
     tidak bisa ditukar antarbaris), menyimpan berkas ke `BlobStore` dengan kunci `kyc/<id>/ktp|selfie`
     (isi = ciphertext, content type `application/octet-stream`; content type asli ikut terenkripsi);
   - memanggil `KYCProvider.Verify` (`manual` → `pending`; error provider → jatuh ke antrean manual);
   - menyimpan baris `kyc_submissions` (`nik_hmac`, `nik_enc`, `nik_masked`, `name_enc`, object key) dan memanggil
     `Hooks.OnSubmitted(ctx, tx, sub)` **di transaksi yang sama**.
2. **Inti di `OnSubmitted`:** `INSERT INTO verification_requests (id, user_id, method='manual', name_on_ktp,
   nik_masked, status='pending', submitted_at, kyc_submission_id) VALUES (…, sub.ID)` dan set
   `users.verification_status='pending'`. `sub.NameOnKTP` **kosong** di hook ini (sengaja, supaya nama polos
   tidak tersebar). Kalau inti butuh nama di `verification_requests.name_on_ktp`, ambil lewat
   `kycSvc.Get(ctx, adminViewer, sub.ID)` atau, lebih baik, biarkan kolom itu kosong dan tampilkan nama dari
   antrean KYC (admin-only). Kalau provider memutuskan otomatis, `OnDecided` dipanggil juga di transaksi yang sama.
3. Admin memutuskan (`POST /api/v1/admin/kyc/{id}/decision`). Approve membuat baris `identities` (unik per
   `nik_hmac`, FR-A3; NIK dienkripsi ulang dengan AAD id user supaya tidak bergantung pada baris pengajuan),
   menjadwalkan `files_purge_after = keputusan + 30 hari`, menulis audit `kyc.approve`/`kyc.reject`, lalu
   `Hooks.OnDecided(ctx, tx, sub)`.
4. **Inti di `OnDecided`:** update `verification_requests` (`status`, `reviewed_at=sub.DecidedAt`,
   `reviewed_by=sub.DecidedBy`, `reason`) berdasarkan `kyc_submission_id = sub.ID`, dan `users`
   (`verification_status`, `verification_method='manual'`, `verified_at`, `nik_masked=sub.NIKMasked`) kalau approve.
   Notifikasi ke user juga dari sini (outbox notify).
5. `PurgeFiles` menghapus berkas dan mengosongkan `ktp_object`/`selfie_object`, set `files_purged_at`. Status,
   `nik_masked`, `nik_hmac`/`nik_enc`, dan `identities` tetap disimpan.

Aksi admin lama di `internal/domain` (approve/reject verifikasi) sebaiknya diarahkan ke `kycSvc.Decide`, supaya
hanya ada satu jalur keputusan. Aksi blacklist inti sebaiknya memanggil `kycSvc.BlacklistUser`.

Audit (`Auditor.Audit(ctx, actorID, action, target, old, new)`):

| action | target | kapan |
|---|---|---|
| `kyc.file.read` | `kyc_submission:<id>/<ktp\|selfie>` | **Setiap** percobaan baca berkas oleh admin, sebelum data disentuh (termasuk id/jenis yang tidak ada). Kalau audit gagal, akses ditolak (503) |
| `kyc.approve` / `kyc.reject` | `kyc_submission:<id>` | Keputusan (old `pending`, new status). Aktor `system:<provider>` untuk keputusan otomatis |
| `kyc.blacklist` | `user:<id>` atau `nik:<masked>` | Tambah blacklist (new = alasan) |
| `kyc.files.purge` | `kyc_submission:<id>` | Berkas dihapus job retensi (aktor `system`) |

## 5. Kontrak JSON endpoint

Semua galat berbentuk v1: `{"ok": false, "error": "<pesan Bahasa Indonesia>"}`, kadang dengan `"code"` dan,
untuk 429, `"retryAfter": <detik>` + header `Retry-After`. Galat internal selalu 500 dengan pesan generik.

### `POST /api/v1/auth/otp/request`

Request `{"phone": "0812-3456-7890"}` (format bebas: `08…`, `62…`, `+62…`, `8…`; hanya nomor seluler Indonesia).

200:
```json
{"ok": true, "phone": "+6281234567890", "expiresAt": "2026-11-16T09:05:00Z", "resendAfter": 60, "channel": "zenziva"}
```
`devCode` ikut hanya untuk provider `demo`/`echo`. Respons **sama** untuk nomor terdaftar dan belum terdaftar.

Galat: 400 nomor tidak valid; 429 jeda kirim ulang 60 detik ("Tunggu N detik…"), > `OTP_MAX_PER_PHONE_HOUR`
(default 5)/jam per nomor, > `OTP_MAX_PER_IP_HOUR` (default 200, NAT kampus)/jam per IP; 503 `code:"otp_daily_cap"`
kalau plafon global `OTP_MAX_SENDS_PER_DAY` (default 2000 per hari WIB) tercapai, dengan `Retry-After` sampai
00.00 WIB (lihat `docs/integration/ops.md` §2); 502 `code:"otp_send_failed"` kalau gateway gagal (kuota tidak
terpakai).

### `POST /api/v1/auth/otp/verify`

Request `{"phone": "0812…", "code": "123456", "acceptTerms": true, "transport": "cookie"}`
(`transport` opsional, `"bearer"` hanya kalau `SESSION_BEARER_LOGIN`).

200:
```json
{"ok": true, "userId": "u-…", "role": "user", "isNew": true,
 "session": {"id": "s-…", "expiresAt": "2026-12-16T09:00:00Z"}}
```
Mode cookie: `Set-Cookie: sewain_session=…; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax`. Mode
bearer: tambahan `"token": "<43 karakter base64url>"`, tanpa cookie.

Galat: 400 kode salah/kedaluwarsa/sudah dipakai (pesan sama untuk semua kasus); 429 setelah 5 kali salah ("minta
kode baru"); 400 `code:"consent_required"` kalau nomor belum terdaftar dan `acceptTerms` bukan `true` (kode
**tidak** hangus, web cukup menampilkan centang S&K lalu kirim ulang dengan kode yang sama); 403 akun diblokir.

Web: tampilkan centang "Saya setuju S&K dan Kebijakan Privasi" sejak awal di layar OTP dan kirim
`acceptTerms:true` kalau dicentang; untuk pengguna lama nilainya diabaikan. Saat versi dokumen naik,
`authSvc.NeedsConsent(ctx, userID)` → minta setuju lagi → `authSvc.AcceptCurrentDocs(ctx, userID, meta)`
(endpoint untuk ini belum dibuat, lihat "Terbuka").

### `POST /api/v1/auth/logout`

Tanpa body. 200 `{"ok": true}` dan cookie dihapus (`Max-Age=0`). Idempoten (tamu juga 200).

### `GET /api/v1/auth/sessions` (perlu masuk)

```json
{"ok": true, "sessions": [
  {"id": "s-…", "createdAt": "…", "lastSeenAt": "…", "expiresAt": "…", "userAgent": "Mozilla/5.0 …", "ip": "1.2.3.4", "current": true}
]}
```
`lastSeenAt` diperbarui paling sering tiap 5 menit. `expiresAt` bergeser = aktivitas terakhir + 30 hari.

### `DELETE /api/v1/auth/sessions/{id}` (perlu masuk)

200 `{"ok": true}`. 404 "Sesi tidak ditemukan." kalau bukan milik sendiri/sudah dicabut. Mencabut sesi sendiri
juga menghapus cookie.

### `DELETE /api/v1/auth/sessions` (perlu masuk)

Cabut semua sesi kecuali sesi ini. 200 `{"ok": true, "revoked": 2}`.

### `POST /api/v1/kyc/submissions` (perlu masuk)

`multipart/form-data`: `ktp` (berkas), `selfie` (berkas), `nik` (16 digit), `nameOnKtp`. Maks 8 MB per berkas,
JPG/PNG/WebP (dicek dari isi berkas).

201:
```json
{"ok": true, "submission": {"id": "kyc-…", "userId": "u-…", "nikMasked": "3404********0001", "status": "pending",
 "reason": "", "provider": "manual", "createdAt": "…", "decidedAt": null, "filesPurgeAfter": null, "filesPurgedAt": null}}
```
Galat: 400 NIK/nama/berkas kurang; 413 terlalu besar; 415 bukan gambar; 409 NIK dipakai akun lain / sudah
terverifikasi / masih ada pengajuan pending; 403 NIK diblokir.

### `GET /api/v1/kyc/status` (perlu masuk)

`{"ok": true, "submission": <seperti di atas> | null}`, pengajuan terakhir user. Kalau `rejected`, tampilkan
`reason` dan izinkan unggah ulang.

### Admin (role `admin`; 401 tamu, 403 "Hanya admin.")

- `GET /api/v1/admin/kyc?status=pending|approved|rejected|all&limit=50` → `{"ok": true, "submissions": [ … ]}`,
  urut terlama dulu, dengan `nameOnKtp` (didekripsi) untuk dicocokkan dengan nama profil (FR-A2). NIK lengkap
  tidak pernah dikirim.
- `GET /api/v1/admin/kyc/{id}` → `{"ok": true, "submission": {…, "nameOnKtp": "…", "decidedBy": "u-…"}}`.
- `POST /api/v1/admin/kyc/{id}/decision` body `{"decision": "approve"|"reject", "reason": "…"}` (alasan wajib
  untuk reject, ditampilkan ke user) → `{"ok": true, "submission": {…}}`. 409 kalau sudah diputuskan, 403/409 kalau
  NIK diblokir/dipakai akun lain saat approve.
- `GET /api/v1/admin/kyc/{id}/files/{ktp|selfie}` → byte gambar asli, `Content-Type` asli,
  `Cache-Control: no-store, private`, `X-Content-Type-Options: nosniff`, CSP sandbox. Selalu tercatat di audit.
  410 kalau berkas sudah di-purge. Web memuatnya lewat `fetch` (dengan cookie/bearer) lalu `URL.createObjectURL`,
  bukan `<img src>` langsung kalau memakai bearer.
- `POST /api/v1/admin/kyc/blacklist` body `{"userId": "u-…", "reason": "…"}` atau `{"nik": "…", "reason": "…"}` →
  `{"ok": true}`.

## 6. Kunci dan rotasi

```bash
openssl rand -base64 32   # untuk OTP_PEPPER, NIK_PEPPER, dan tiap kunci di KYC_KEYS
```

Rotasi `KYC_KEYS`: tambahkan kunci baru di **akhir** (`KYC_KEYS=v1:…,v2:…`) lalu restart. Data baru memakai `v2`,
data lama tetap terbaca karena id kunci tersimpan di setiap ciphertext. `crypto.KeyID(blob)` menunjukkan kunci
yang dipakai dan `KeyRing.Rewrap(blob, aad)` mengenkripsi ulang ke kunci aktif (job re-enkripsi belum dibuat,
lihat "Terbuka"). `v1` baru boleh dihapus setelah tidak ada data `v1` tersisa. `OTP_PEPPER` boleh diganti kapan
saja (hanya membatalkan OTP yang sedang berjalan). `NIK_PEPPER` tidak boleh diganti.

## 7. Keputusan yang diambil di paket ini

- **Rate limit dari tabel**, diserialisasi dengan `pg_advisory_xact_lock` per nomor lalu per IP (urutan tetap,
  tidak bisa deadlock), jadi aman multi-instance. Pengiriman ke gateway terjadi sebelum commit: kalau gagal,
  tantangan di-rollback dan kuota tidak terpakai.
- **Hanya tantangan terbaru** untuk satu nomor yang bisa diverifikasi. Kode lama otomatis tidak berlaku.
- **Hash kode** = HMAC(`OTP_PEPPER`, id tantangan | nomor | kode), dibandingkan constant-time.
- **Persetujuan S&K** diminta setelah kode terbukti benar (tidak membocorkan status pendaftaran ke orang yang
  tidak memegang nomor), dan kode tidak hangus saat `consent_required`.
- **Status `suspend`** tetap bisa masuk (inti membatasi aksinya). `blacklist` tidak bisa masuk dan sesinya
  langsung tidak berlaku.
- **Token bearer di body** hanya untuk demo/staging (`SESSION_BEARER_LOGIN`), ditolak di production, supaya token
  production tidak pernah terbaca JavaScript (D6).
- **Cookie diperbarui** (`Max-Age` 30 hari) setiap kali `last_seen_at` ditulis (maks tiap 5 menit).
- **Kolom tambahan** di luar ROADMAP §3: `kyc_submissions.nik_masked`, `reason`, `provider`, `decided_by`,
  `updated_at`; `identities.nik_masked`, `submission_id`; `otp_challenges.provider`. Satu pengajuan `pending`
  per user (unique index parsial).
- **Keputusan otomatis provider**: `approved`/`rejected` langsung diterapkan (aktor `system:<provider>`), error
  provider → antrean manual. Fase 8 bisa mengubah agar `rejected` otomatis tetap ditinjau manusia.
- **Fixture tes**: tiap tes memakai schema Postgres acak (`search_path`), menjalankan goose Up → DownTo(0) → Up
  untuk rentang 00100–00119, lalu schema di-drop. `AUTHTEST_KEEP_SCHEMA=1` menyimpan schema untuk diperiksa
  dengan `pg_dump`.

## 8. Terbuka

- Endpoint menerima ulang S&K versi baru (`NeedsConsent`/`AcceptCurrentDocs` sudah ada, belum ada handler).
- Job re-enkripsi data `KYC_KEYS` lama ke kunci aktif (`Rewrap` sudah ada).
- Verifikasi adapter Zenziva dengan akun asli (§9 #2).
- `Submit` memegang transaksi DB selama `BlobStore.Put` dan `KYCProvider.Verify`. Untuk manual + disk/R2 ini
  cepat; vendor eKYC Fase 8 sebaiknya dijalankan asinkron setelah commit.
- Query `GrantAdmin` perlu disesuaikan dengan kolom `users` final (soft delete, `updated_at`).

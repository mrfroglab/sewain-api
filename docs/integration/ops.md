# Integrasi `ops` (Fase 5: F5.1–F5.4)

Paket di `internal/platform/` sudah lengkap dengan tes, tapi **belum dipasang** ke `cmd/api` maupun
`internal/httpapi`. Pemasangannya dikerjakan koordinator setelah inti v2 (`core-v2`) selesai. Dokumen ini
menjelaskan cara memasangnya.

| Paket | Isi |
|---|---|
| `internal/platform/middleware` | Request ID, IP klien (proxy tepercaya), log akses JSON + masking PII, recover, batas body, timeout, header keamanan, CORS, `Chain` |
| `internal/platform/ratelimit` | Interface `Limiter`, token bucket in-memory (`Memory`), profil `Public`/`Write`/`Upload`/`Auth`/`Demo` (angka dari env `RATE_*`), middleware 429 |
| `internal/platform/metrics` | Registry Prometheus privat, `Instrument` (label = pola rute `ServeMux`), gauge WebSocket, durasi job, antrean outbox, collector `pgxpool`, server `/metrics` |
| `internal/platform/health` | `Live()` untuk `/api/health`, `Readiness{...}.Handler()` untuk `/api/ready`, variabel `Version`/`Commit` (diisi `-ldflags`) |
| `internal/platform/server` | `Run(ctx, srv, opts)`: serve sampai ctx selesai, lalu shutdown rapi dengan batas waktu |

## 1. Urutan rantai middleware

`middleware.Chain(h, a, b, c)` sama dengan `a(b(c(h)))`, jadi elemen pertama adalah lapisan terluar.

```go
mux := http.NewServeMux()
// ... daftarkan rute ...

handler := middleware.Chain(m.Instrument(mux), // metrics: WAJIB paling dalam, langsung membungkus mux
    middleware.RequestID,                       // 1. ID dulu supaya semua log membawanya
    middleware.RealIP(ipResolver),              // 2. IP klien (hanya percaya header dari TRUSTED_PROXY_CIDRS)
    middleware.Logger(log, middleware.LoggerOptions{Skip: isHealthProbe}), // 3. log akses
    middleware.Recover(log),                    // 4. panic → 500 JSON; di dalam Logger supaya status 500 tercatat
    middleware.SecurityHeaders,                 // 5. nosniff, Referrer-Policy, CORP, no-store untuk JSON
    middleware.CORS(middleware.DefaultCORS(cfg.CORSOrigins)), // 6. preflight dijawab sebelum batas/rate limit
    middleware.BodyLimit(middleware.DefaultBodyLimit,         // 7. 1 MB default, 12 MB untuk unggahan
        middleware.BodyRule{Method: "POST", Path: "/api/v1/photos", Limit: middleware.UploadBodyLimit},
        middleware.BodyRule{Method: "POST", Path: "/api/v1/kyc/submissions", Limit: middleware.UploadBodyLimit},
    ),
    middleware.Timeout(cfg.RequestTimeout, middleware.TimeoutOptions{}), // 8. WebSocket otomatis dikecualikan
    authSvc.Middleware,                                       // 9. viewer + middleware.SetUserID
    ratelimit.Middleware(limiter, profiles.Public, rlOpts),   // 10. batas kasar semua rute: per user, tamu per IP
)
```

Catatan penting:

- **`Instrument` harus membungkus `mux` langsung.** `ServeMux` mengisi `r.Pattern` pada request yang ia terima.
  `Instrument` membacanya setelah handler selesai, lalu menyimpannya lewat `middleware.SetRoute` supaya Logger
  di luar ikut mencetak `route`. Kalau ada middleware lain di antara keduanya, label rute jadi `unmatched`.
- **Auth** (worker `auth`) memanggil `middleware.SetUserID(r.Context(), userID)` setelah sesi terbaca. Log akses
  dan rate limit per user membaca nilai ini. Karena itu, rate limit `Write`/`Upload` dipasang **per rute**,
  setelah auth menentukan user, bukan di rantai global:

  ```go
  write := ratelimit.Middleware(limiter, ratelimit.Write, rlOpts)
  mux.Handle("POST /api/v1/actions/{name}", authn(write(http.HandlerFunc(s.action))))
  ```

  Tanpa user (tamu), `ByUser` otomatis memakai IP.
- **Batas body per rute ditentukan di lapisan luar** (`BodyRule`), karena `MaxBytesReader` di dalam tidak bisa
  menaikkan batas yang dipasang di luar. Handler yang membaca body cukup memeriksa
  `middleware.IsBodyTooLarge(err)` lalu memanggil `middleware.RespondTooLarge(w)`. `Content-Length` yang
  melebihi batas langsung ditolak 413 sebelum handler jalan.
- **Timeout** mem-buffer respons (seperti `http.TimeoutHandler`). WebSocket (`/api/v1/events`) dikecualikan
  otomatis. Rute lain yang streaming atau mengirim berkas besar bisa dikecualikan lewat `TimeoutOptions.Skip`.
  Handler wajib menghormati `r.Context()`. Saat waktu habis klien menerima 503
  `{"ok":false,"error":"Server sedang sibuk, coba lagi sebentar lagi."}`.
- **Recover** menulis `{"ok":false,"error":"Terjadi kesalahan di server."}` dan log berisi `stack` +
  `request_id`. `http.ErrAbortHandler` diteruskan.
- `/api/health` dan `/api/ready` didaftarkan di `mux` yang sama (`health.Live()`, `readiness.Handler()`).
  `Skip` di Logger dipakai untuk membuang log probe Docker tiap 15 detik:
  `func(r *http.Request) bool { return r.URL.Path == "/api/health" }`.

### Log dan masking

- Pakai satu logger proses: `log := middleware.NewLogger(os.Stdout, middleware.ParseLevel(os.Getenv("LOG_LEVEL")))`.
  Handler-nya `RedactingHandler`, jadi **semua** log (bukan hanya log akses) di-mask. `Logger(...)` juga membungkus
  logger yang belum me-redact, untuk berjaga-jaga.
- Aturan masking: key yang mengandung `token`, `authorization`, `cookie`, `password`, `secret`, `otp`, `session`,
  `nik`, `signature` → `[REDACTED]`. Key `phone`/`*_phone`/`msisdn`/`whatsapp`/`hp` → `+6281******890`. Teks bebas
  (termasuk pesan log, `error`, path) dipindai untuk nomor HP Indonesia (`08…`, `62…`, `+62…`), `Bearer …`, dan
  parameter `token=`/`otp=`/`code=`/`sig=`.
- Log akses tidak pernah memuat query string (v1 menerima `?token=` di WebSocket).
- Field log akses: `method`, `path`, `route`, `status`, `duration_ms`, `bytes`, `request_id`, `user_id`, `ip`.
  Level `ERROR` untuk 5xx, `WARN` untuk 4xx, `INFO` sisanya.
- `request_id` diterima dari header `X-Request-ID` kalau valid (8–128 karakter `[A-Za-z0-9._:-]`). Kalau tidak,
  dibuat baru (32 hex). Nilainya selalu dikirim balik di header respons. Simpan juga di `audit_log.request_id`
  lewat `middleware.RequestIDFrom(ctx)`.

### IP klien

Jalur di server ini: Cloudflare → cloudflared → Caddy (`caddy-proxy`, jaringan `gateway-net`) → API.
`CF-Connecting-IP` dan `X-Forwarded-For` **hanya dipercaya kalau peer langsung ada di `TRUSTED_PROXY_CIDRS`**.
Untuk stack sekarang isi dengan subnet `gateway-net` (cek `docker network inspect gateway-net`, biasanya
`172.16.0.0/12` sudah mencakup). Kalau kosong, IP = `RemoteAddr` (aman, tapi semua klien terlihat sebagai IP Caddy
sehingga rate limit per IP jadi global, jadi **wajib diisi di staging/produksi**).

## 2. Profil rate limit

Token bucket: `Burst` token maksimal, terisi `Limit` per menit. Semua angka dibaca dari env saat start
(`internal/config` → `Config.Rates` → `httpapi.ProfilesFromRates`). Kosong atau `0` = default di bawah. Nilai yang
bukan bilangan bulat 1–1.000.000 membuat proses **gagal start** dengan pesan yang menyebut nama variabelnya.

| Profil | Default | Burst | Key | Env | Dipakai untuk |
|---|---|---|---|---|---|
| `ip` | 6000/menit | 3000 | IP | `RATE_IP_PER_MIN`, `RATE_IP_BURST` | Rem murah di depan **semua** rute, dipasang **sebelum** `auth.Middleware` (review gelombang 2, L6): token/cookie acak dari satu IP berhenti di sini tanpa lookup sesi |
| `public` | 1200/menit | 300 | user (tamu → IP) | `RATE_PUBLIC_PER_MIN`, `RATE_PUBLIC_BURST` | Global (semua rute), termasuk `GET /api/v1/state` (snapshot), `GET /api/v1/listings/search`, `GET /api/v1/photos/{id}`, `GET /api/v1/events` (handshake WebSocket) |
| `write` | 120/menit | 40 | user (tamu → IP) | `RATE_WRITE_PER_MIN`, `RATE_WRITE_BURST` | `POST /api/v1/actions/{name}`, `POST/DELETE /api/v1/push/subscriptions`, `PUT /api/v1/me/notification-prefs`, `DELETE /api/v1/me`, `POST /api/v1/auth/logout`, `DELETE /api/v1/auth/sessions`, `POST /api/v1/admin/listings/import` |
| `upload` | 30/menit | 10 | user (tamu → IP) | `RATE_UPLOAD_PER_MIN`, `RATE_UPLOAD_BURST` | `POST /api/v1/photos`, `POST /api/v1/photos/demo`, `POST /api/v1/kyc/submissions` |
| `auth` | 60/menit | 20 | IP | `RATE_AUTH_PER_MIN`, `RATE_AUTH_BURST` | `POST /api/v1/auth/otp/request`, `POST /api/v1/auth/otp/verify` |
| `demo` | 600/menit | 100 | IP | `RATE_DEMO_PER_MIN`, `RATE_DEMO_BURST` | `POST /api/v1/auth/demo-login` (hanya ada di `DEMO_MODE`) |

### Kenapa angkanya segini: NAT kampus

Wi-Fi kampus UGM keluar lewat **satu IP NAT** untuk ratusan sampai ribuan mahasiswa. Hari pertama pilot (booth
pendaftaran, demo di kelas) semua lalu lintas datang dari satu IP. Batas lama (`public` 120/menit per IP, global)
membuat satu kelas saja sudah kena 429. Uji regresi staging gagal karena hal yang sama: semua request datang dari
satu mesin.

- **`public` per user, bukan per IP.** Limiter global dipasang **setelah** `auth.Middleware`, sehingga mahasiswa
  yang sudah masuk punya bucket sendiri. Tamu (dan token yang tidak valid) berbagi bucket IP. 1200/menit per IP
  untuk tamu cukup untuk sekitar 300 pengunjung anonim yang membuka snapshot dan beberapa foto per menit. Satu IP
  yang membanjiri tetap kena 429 setelah 300 request beruntun (lalu 20/detik).
- **`ip` sebelum auth.** Karena `public` berjalan setelah auth, request dengan token acak sempat memicu lookup sesi
  sebelum kena limit. Rem `ip` (6000/menit, burst 3000) hanya melihat alamat, jadi murah, dan jauh di atas NAT
  kampus: 300 mahasiswa aktif menghasilkan ±3.600 request/menit di uji beban (`docs/loadtest/2026-11-report.md`),
  dan tes `TestCampusNATDefaults` (300 mahasiswa × 9 request sekaligus = 2.700) tetap lulus. Tes
  `TestFakeTokenFloodStopsBeforeAuth` membuktikan request yang ditolak tidak pernah sampai ke auth.
- **`write`/`upload` per user.** Pola wajar satu mahasiswa: 1 snapshot + 5 aksi + 3 foto per menit. Default
  memberi ruang 4–24× pola itu, dan burst cukup untuk mengunggah semua foto checkout sekaligus.
- **`auth` 60/menit, burst 20 per IP.** Booth pendaftaran bisa memproses puluhan nomor per menit dari satu IP.
  Pertahanan biaya OTP yang sebenarnya ada di database (per nomor, per IP per jam, dan plafon harian global, lihat
  di bawah), bukan di profil ini.
- **`demo` terpisah dari `auth`.** Demo dan uji regresi login banyak persona dari satu mesin. Endpoint ini hanya
  ada di `DEMO_MODE` (ditolak di produksi), jadi profilnya longgar.
- Tes: `ratelimit.TestCampusNATNoFalsePositives` (300 user × 5 menit, jam palsu), `app.TestCampusNATDefaults`
  (300 user lewat HTTP sungguhan dari satu IP), `TestSingleIPFloodStillLimited`, `TestDemoLoginNotThrottled`.

### Batas OTP di database (paket `auth`)

Dihitung dari `otp_challenges`, jadi berlaku lintas instance. Kode yang gagal dikirim gateway tidak dihitung.

| Env | Default | Arti |
|---|---|---|
| `OTP_MAX_PER_PHONE_HOUR` | 5 | Kode per nomor per jam (ditambah jeda kirim ulang 60 detik) |
| `OTP_MAX_PER_IP_HOUR` | 200 | Kode per IP (IPv6 per /64) per jam. Tinggi karena NAT kampus |
| `OTP_MAX_SENDS_PER_DAY` | 2000 | Plafon **global** kode terkirim per hari kalender WIB (00.00–24.00 WIB) |

Plafon harian adalah pertahanan utama terhadap SMS/WA pumping: tiap OTP berbayar (PRD §15.2), dan penyerang bisa
memutar nomor serta IP. Saat plafon tercapai:

- Request OTP dijawab `503` `{"ok":false,"code":"otp_daily_cap","error":"Maaf, pengiriman kode OTP hari ini sudah
  mencapai batas. Coba lagi besok, ya. Kalau mendesak, hubungi admin Sewain."}` dengan `Retry-After` sampai
  00.00 WIB. Sesi yang sudah ada tidak terpengaruh.
- Penolakan pertama hari itu menulis log `WARN` `OTP daily send cap reached: ...` (field `day`, `cap`, `sent`).
- Semua admin mendapat **satu** notifikasi in-app (jenis `keamanan`, ikut push/email) per hari. Dedup lintas
  instance lewat tabel `otp_daily_cap_alerts` (migrasi `00102`). Kalau pengiriman notifikasi gagal, penolakan
  berikutnya mencoba lagi.
- Plafon tidak dikunci global (supaya OTP tidak antre di belakang panggilan gateway), jadi request yang berjalan
  bersamaan bisa melewatinya sebanyak request yang sedang berjalan saat itu.
- Kalau lalu lintasnya memang wajar (mis. hari peluncuran), naikkan `OTP_MAX_SENDS_PER_DAY` lalu restart.

Catatan:

- Rute yang punya profil khusus tetap kena `public`. Ini disengaja: `public` jadi batas atas per user/IP.
- Karena `public` berjalan setelah `auth.Middleware`, request dengan token palsu tetap memicu satu lookup sesi
  (indeks hash) sebelum kena 429. Untuk pilot ini dapat diterima.
- **Snapshot**: web me-refetch `GET /api/v1/state` setiap ada event WebSocket. Dengan `ETag` (core-v2) refetch
  murah, tapi tetap terhitung (per user).
- **Events**: yang dibatasi hanya handshake. Pesan di koneksi yang sudah terbuka tidak dihitung.
- **Healthcheck** dilayani dari `127.0.0.1` di dalam container. Kalau perlu, kecualikan dengan `KeyFunc` yang
  mengembalikan `""` untuk `/api/health`.
- Respons 429: header `Retry-After` (detik, dibulatkan ke atas), `X-RateLimit-Limit`, `X-RateLimit-Remaining`, body
  `{"ok":false,"error":"Terlalu banyak permintaan. Coba lagi dalam N detik."}`.
- Kalau `Limiter` mengembalikan galat, request **diloloskan** (fail open) dan galat dicatat di log.
- Pasang `limiter.StartJanitor(ctx, time.Minute)` supaya bucket idle (default 10 menit) dibuang.
- Hitung 429 di metrics:
  `rlOpts := ratelimit.Options{Log: log, OnLimited: func(p string, _ *http.Request) { m.RateLimited(p) }}`.

> **Batas in-memory berlaku per instance.** Profil `RATE_*` dihitung di memori proses: dengan N instance API, batas
> efektif menjadi N kali lipat, dan restart mengosongkan semua bucket. Untuk pilot (1 instance, D11) ini cukup.
> Sebelum menjalankan lebih dari satu instance, bagi angkanya dengan jumlah instance atau buat implementasi
> `ratelimit.Limiter` berbasis Redis/Postgres (interface-nya hanya `Allow(ctx, key, rate) (Decision, error)`).
> Batas OTP (`OTP_MAX_*`) **tidak** terpengaruh: dihitung di database.

### Keputusan worker `tune-limits`

- `public` dipindah ke **setelah** `auth.Middleware` dan di-key per user (tamu per IP). Dengan key per IP murni,
  300 mahasiswa × 9 request/menit = 2700 request/menit dari satu IP, jauh di atas 1200, jadi kriteria "NAT kampus
  tanpa 429" mustahil dipenuhi. Banjir dari satu IP tamu tetap dibatasi di angka yang dikonfigurasi.
- `demo-login` memakai profil sendiri (`demo`), bukan `auth`, supaya login demo tidak menghabiskan kuota OTP.
- Plafon OTP harian ditolak dengan **503** + `code:"otp_daily_cap"` (bukan 429): yang habis kuota layanan, bukan
  klien. Dihitung per hari kalender WIB (UTC+7 tetap) dari `otp_challenges` (indeks baru `created_at`).
- Notifikasi admin lewat `service.NotifyAdmins` (berkas baru `internal/service/alerts.go`, hanya menambah fungsi)
  yang memakai jalur `t.notify(domain.AdminID, …)`: in-app ke semua admin + outbox push/email. Jenis `keamanan`
  supaya melewati jam tenang.
- Purge OTP sekarang menolak cutoff < 24 jam (sebelumnya 1 jam), karena plafon harian membutuhkan baris hari ini.
  Runner retensi sudah memakai 24 jam.

## 3. Env baru

| Env | Default | Arti |
|---|---|---|
| `METRICS_ADDR` | `127.0.0.1:9090` | Alamat server `/metrics`. `off` = mati. Di Docker, `127.0.0.1` hanya bisa diakses dari dalam container. Untuk scrape dari container Prometheus di jaringan internal, pakai `:9090`, **jangan** di-publish (`ports:`) dan **jangan** dibuat blok Caddy-nya. |
| `TRUSTED_PROXY_CIDRS` | kosong | CIDR/IP proxy yang header `CF-Connecting-IP`/`X-Forwarded-For`-nya dipercaya, dipisah koma. Contoh: `172.16.0.0/12`. Parse dengan `middleware.ParseCIDRList` + `middleware.NewIPResolver`. |
| `REQUEST_TIMEOUT` | `15s` (saran) | Batas waktu per request (`time.ParseDuration`). Harus di atas p95 unggahan 12 MB dari HP lambat. WebSocket tidak terkena. |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`, lewat `middleware.ParseLevel`. |

Env ini dibaca di `internal/config` (wilayah core-v2) saat pemasangan. Paket `ops` tidak membaca env kecuali
`metrics.AddrFromEnv()`.

Untuk `http.Server` API, sarankan juga `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s`, dan **tanpa**
`WriteTimeout` global (akan memutus WebSocket). Timeout per request dipegang middleware `Timeout`.

## 4. Memasang server metrics dan health di `main`

```go
log := middleware.NewLogger(os.Stdout, middleware.ParseLevel(os.Getenv("LOG_LEVEL")))
m := metrics.New()
if err := m.RegisterPool(pool); err != nil { // pool *pgxpool.Pool milik core-v2
    return err
}

ready := health.Readiness{
    Ping:             pool.Ping,
    MigrationVersion: func(ctx context.Context) (int64, error) { return goose.GetDBVersionContext(ctx, stdDB) },
    MinMigration:     0, // atau versi migrasi terakhir yang dibutuhkan biner ini
}
mux.Handle("GET /api/health", health.Live())
mux.Handle("GET /api/ready", ready.Handler())

limiter := ratelimit.NewMemory(ratelimit.MemoryOptions{})
limiter.StartJanitor(ctx, time.Minute)

api := &http.Server{Addr: ":" + cfg.Port, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
api.RegisterOnShutdown(hub.CloseAll) // WebSocket hijacked tidak ditunggu Shutdown; tutup sendiri dengan StatusGoingAway

g, gctx := errgroup.WithContext(ctx) // atau dua goroutine + channel galat
g.Go(func() error { return server.Run(gctx, api, server.Options{Name: "api", Log: log}) })
if addr := metrics.AddrFromEnv(); addr != "" {
    g.Go(func() error { return server.Run(gctx, metrics.NewServer(addr, m), server.Options{Name: "metrics", Log: log}) })
}
return g.Wait()
```

- Hub WebSocket memanggil `m.WebSocketOpened()` saat koneksi diterima dan `defer m.WebSocketClosed()`.
- Job berkala (ticker auto-expire, retensi, dispatcher outbox) dibungkus `m.TimeJob("ticker", func() error {...})`.
  Dispatcher outbox memanggil `m.SetOutboxPending(channel, n)` setiap putaran.
- `server.Run` menunggu request yang sedang berjalan sampai `DefaultShutdownTimeout` (8 detik, di bawah
  timeout stop Docker 10 detik).
- Scrape lokal: `docker exec <api> /app/api ...` tidak bisa karena distroless tidak punya curl. Gunakan
  `curl http://127.0.0.1:9090/metrics` saat `go run` lokal, atau container Prometheus/`curlimages/curl` di
  jaringan Docker yang sama dengan `METRICS_ADDR=:9090`.
- Healthcheck Docker (`api healthcheck` → `GET /api/health`) tetap jalan. Karena `/api/health` sekarang murni
  liveness (tidak cek DB), container tidak dianggap unhealthy saat DB sebentar mati. Cek DB pindah ke
  `/api/ready`. Langkah deploy CI yang menunggu `healthy` tetap valid. Kalau ingin deploy juga menunggu DB,
  ganti target `healthcheck` ke `/api/ready` (perubahan di `cmd/`, wilayah core-v2).

### Metrics yang diekspos

| Nama | Tipe | Label |
|---|---|---|
| `sewain_http_request_duration_seconds` | histogram | `method`, `route` |
| `sewain_http_requests_total` | counter | `method`, `route`, `status` |
| `sewain_http_requests_in_flight` | gauge | |
| `sewain_http_rate_limited_total` | counter | `profile` |
| `sewain_websocket_connections_active` | gauge | |
| `sewain_job_duration_seconds` | histogram | `job`, `result` (`ok`/`error`) |
| `sewain_outbox_pending` | gauge | `channel` |
| `sewain_db_pool_*` | gauge/counter | (acquired/idle/total/max conns, acquire count/duration, dll.) |
| `go_*`, `process_*` | bawaan | |

`route` = pola `ServeMux` (mis. `GET /api/v1/photos/{id}`), bukan path mentah. Request yang tidak cocok rute
mana pun diberi label `unmatched`, dan method di luar daftar standar menjadi `OTHER`. Dengan begitu jumlah seri
tetap terbatas walaupun ada scanner.

## 5. CI (`.github/workflows/deploy.yml`)

Job lama (`test`, `image`, `deploy`) tetap sama: `main` → image `:latest` → demo, `develop` → `:develop` →
staging. Ada dua perubahan:

- `image` sekarang `needs: [test, lint, integration]`.
- `image` mengirim build arg `VERSION=<branch>` dan `COMMIT=<sha>` ke Dockerfile. Ini hanya mengisi
  `/api/health` + label OCI. Tag dan target deploy tidak berubah.

Job baru:

| Job | Isi | Memblokir image? |
|---|---|---|
| `lint` | `golangci/golangci-lint-action@v9`, golangci-lint **v2.15.0**, konfigurasi `.golangci.yml` | Ya |
| `integration` | Service `postgis/postgis:16-3.4-alpine`, `TEST_DATABASE_URL=postgres://sewain:test@127.0.0.1:5432/sewain?sslmode=disable`, `go test -count=1 ./...` | Ya |
| `security` | `govulncheck@v1.9.0 ./...` | Tidak (laporan; temuan stdlib bisa muncul sebelum image Go dinaikkan) |
| `workflow-lint` | `rhysd/actionlint:1.7.12` | Tidak |
| `sqlc-diff` | Hanya kalau `sqlc.yaml` ada: `sqlc vet`, `sqlc diff`, `sqlc generate` dengan image `sqlc/sqlc:1.31.1`, lalu gagal kalau `git status` tidak bersih | Tidak |
| `migrations` | Hanya kalau `internal/db/migrations` ada **dan** `cmd/api` punya subcommand `"migrate"`. Build biner, lalu `api migrate up` dua kali (harus idempoten) di DB PostGIS kosong, masing-masing dengan `timeout 120` | Tidak |

Setelah core-v2 masuk, koordinator bisa menambahkan `sqlc-diff` dan `migrations` ke `needs` milik `image`.
Keduanya sekarang otomatis dilewati karena berkasnya belum ada, jadi belum dimasukkan supaya tidak terlihat
"hijau palsu".

### `.golangci.yml`

- Linter: `errcheck`, `govet`, `staticcheck`, `ineffassign`, `unused`, `gosec`, `revive` (aturan dasar),
  `bodyclose`, `sqlclosecheck`, `rowserrcheck`, `misspell`, dan formatter `gofmt`.
- Paket v1 yang sedang ditulis ulang (`cmd/`, `internal/config`, `internal/domain`, `internal/httpapi`,
  `internal/seed`, `internal/store`) **dikecualikan sementara** dengan komentar `TODO(core-v2)`. Hapus
  pengecualian per paket begitu paket itu lulus lint. Paket baru (`internal/db`, `internal/service`, dll.)
  langsung ikut di-lint, **kecuali kalau namanya ada di daftar itu**.
- Menjalankan lokal (Go tidak terpasang di host):

  ```sh
  docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
    -e GOLANGCI_LINT_CACHE=/cache/lint -e GOFLAGS=-buildvcs=false -v sewain-go-cache:/cache \
    -v "$PWD":/src -w /src golangci/golangci-lint:v2.15.0 golangci-lint run ./...
  docker run --rm -v "$PWD:/repo" -w /repo rhysd/actionlint:1.7.12 -color
  ```

## 6. Dockerfile

- Tetap `gcr.io/distroless/static-debian12:nonroot`, `USER nonroot:nonroot`, `HEALTHCHECK` lewat
  `/app/api healthcheck`.
- Build arg `VERSION` (default `dev`) dan `COMMIT` (default `unknown`) masuk ke
  `internal/platform/health.Version/Commit` lewat `-ldflags -X`, serta ke label `org.opencontainers.image.*`.
  Selama `cmd/api` belum mengimpor paket `health`, `-X` tidak berefek (aman).

## 7. Keputusan dan hal terbuka

Keputusan dalam wilayah `ops`:

- **Timeout pakai middleware sendiri**, bukan `http.TimeoutHandler`, supaya respons 503-nya JSON Bahasa Indonesia
  dan panic tetap sampai ke `Recover`. WebSocket dikecualikan otomatis.
- **`/api/health` = liveness murni.** v1 `/api/health` mengecek DB dan mengembalikan `demoMode`/`clients`.
  Handler v2 hanya mengembalikan `ok`, `version`, `commit`. Kalau web/E2E membaca `demoMode` dari
  `/api/health`, core-v2 perlu memindahkannya ke `/api/v1/state` atau membungkus `Live()`.
- **`/api/ready` tidak membocorkan detail galat** (hanya `db: down`, `migrations: behind/unknown`), karena
  endpoint ini publik lewat Caddy.
- **Preflight dari origin yang tidak diizinkan dijawab 403.** Request biasa dari origin asing diteruskan tanpa
  header CORS, sehingga browser yang memblokir. `*` diabaikan kalau `AllowCredentials` aktif.
- **`Cache-Control: no-store`** hanya ditambahkan ke respons JSON yang belum menyetel `Cache-Control` sendiri.
  Foto tetap bebas menyetel cache-nya.
- **`Cross-Origin-Resource-Policy: same-site`**: web (`sewain.hallojanu.me`) dan API berada di site yang sama,
  jadi `<img>` foto tetap bisa dimuat. Kalau API pindah ke domain lain, ganti ke `cross-origin` untuk rute foto.
- **Rate limit gagal = fail open**, supaya limiter yang rusak tidak membuat API mati.
- **`go.mod`**: menambah `prometheus/client_golang v1.25.0`. MVS ikut menaikkan `golang.org/x/sync` (0.22→0.23)
  dan `golang.org/x/text` (0.41→0.42), keduanya dependensi tidak langsung. Kalau terjadi konflik merge di
  `go.mod`/`go.sum`, ambil versi tertinggi lalu jalankan `scripts/go.sh mod tidy`.

Hal terbuka (untuk koordinator):

1. Memasang semua di atas ke `cmd/api` + `internal/httpapi` (menunggu core-v2), termasuk env baru di
   `internal/config` dan `deploy/docker-compose.yml`.
2. Mengganti `CORS`/`logRequests` milik v1 di `internal/httpapi/server.go` dengan paket ini, dan menghapus
   `?token=` dari WebSocket kalau auth cookie sudah ada.
3. Menambahkan `sqlc-diff` dan `migrations` ke `needs` job `image` setelah berkasnya ada. Menghapus pengecualian
   `TODO(core-v2)` di `.golangci.yml`.
4. Menyiapkan Prometheus (atau minimal scrape manual) di jaringan internal. Belum ada di stack.
5. **govulncheck merah saat ini:** `GO-2026-6859` (race header `http.MaxBytesReader` yang bisa membuat server HTTP/1
   crash). Bug ini ada di `net/http@go1.27.3` dan diperbaiki di **go1.27.4** (rilis 11 Nov 2026). Masalah ini relevan karena
   `BodyLimit` memakai `MaxBytesReader`, dan handler unggah v1 juga memakainya. Untuk memperbaikinya, naikkan direktif `go` di `go.mod` ke `1.27.4`
   (CI memakai `go-version-file`) dan `GO_IMAGE` default di `scripts/go.sh` ke `golang:1.27.4`. Ini keputusan
   lintas worker, jadi tidak diubah di branch `ops`. Image produksi (`golang:1.27-alpine`) otomatis memakai
   1.27.4 saat dibangun ulang tanpa cache.

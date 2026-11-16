# Integrasi paket `notify` + `realtime` (Fase 4)

Paket mandiri untuk ROADMAP F4.1–F4.6. Belum dipasang ke `cmd/api` / `internal/httpapi`; dokumen ini menjelaskan
cara koordinator/`core-v2` memasangnya.

| Paket / berkas | Isi |
|---|---|
| `internal/realtime` | `Hub` WebSocket per user, `Publisher` (`pg_notify`), `Listener` (LISTEN + reconnect) |
| `internal/notify` | `Enqueuer` (outbox), `Dispatcher`, `PushSender` (VAPID), `EmailSender` + `SMTPMailer`/`ResendMailer`/`LogMailer`, `WhatsAppStub`, preferensi + jam tenang, handler HTTP, `ConfigFromEnv` |
| `internal/db/migrations/00140_notify.sql` | `push_subscriptions`, `notification_prefs`, `notification_outbox` (FK hanya ke `users(id)`) |

Dependensi baru (dipin): `github.com/SherClockHolmes/webpush-go v1.4.0`, `github.com/pressly/goose/v3 v3.30.1`
(dipakai tes untuk menerapkan migrasi; `core-v2` juga memakai goose, samakan versinya saat merge).

## 1. Environment

| Variabel | Wajib? | Keterangan |
|---|---|---|
| `VAPID_PUBLIC_KEY` | untuk push | base64url, titik P-256 tak terkompresi (65 byte). Dibuat dengan `notify.GenerateVAPIDKeys()` |
| `VAPID_PRIVATE_KEY` | untuk push | base64url, 32 byte. **Rahasia**, beda per lingkungan (F7.1) |
| `VAPID_SUBJECT` | untuk push | `mailto:ops@sewain.me` atau URL `https://…` (kontak untuk operator push service) |
| `MAIL_PROVIDER` | tidak | `none` (default), `smtp`, `resend`, `log` (hanya mencatat ke log; cocok untuk staging) |
| `MAIL_FROM` | kalau email aktif | mis. `Sewain <noreply@sewain.me>`. Domain harus punya SPF/DKIM di provider |
| `SMTP_HOST`, `SMTP_PORT` | kalau `smtp` | port default 587 |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | opsional | AUTH PLAIN, hanya lewat TLS |
| `SMTP_SECURITY` | opsional | `starttls` (default, wajib ditawarkan server), `tls` (port 465), `none` (relay lokal saja) |
| `RESEND_API_KEY` | kalau `resend` | API key Resend (`re_…`) |
| `PUBLIC_URL` | kalau email aktif | origin web, mis. `https://sewain.hallojanu.me`. Dipakai untuk tautan absolut di email |
| `NOTIFY_PREFS_PATH` | opsional | rute halaman preferensi di web, default `/profil/notifikasi` |

Semua kosong = valid: push dan email mati, in-app tetap jalan. `ConfigFromEnv(os.Getenv)` menolak kombinasi
setengah jadi (VAPID tidak lengkap, provider tanpa kunci, `MAIL_FROM`/`PUBLIC_URL` kosong saat email aktif).

Subcommand CLI pembuat kunci (untuk `cmd/api`, belum dibuat):

```go
case "vapid-keys":
	pub, priv, err := notify.GenerateVAPIDKeys()
	if err != nil { log.Fatal(err) }
	fmt.Printf("VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", pub, priv)
```

## 2. Memasang di `main`

```go
ncfg, err := notify.ConfigFromEnv(os.Getenv)
if err != nil { return fmt.Errorf("notify config: %w", err) }

// Realtime: satu hub per proses, listener khusus (koneksi pgx sendiri, bukan dari pool).
instanceID := realtime.NewInstanceID()
hub := realtime.NewHub(realtime.Options{
	Resolve:        auth.ResolveViewer,      // func(*http.Request) (userID, role string); "" = tamu
	AllowedOrigins: cfg.CORSOrigins,          // origin penuh, mis. "https://sewain.hallojanu.me"
	CurrentVersion: core.CurrentVersion,      // func(ctx) (int64, error) dari sequence versi
	Logger:         log,
})
publisher := &realtime.Publisher{DB: pool, Hub: hub, Origin: instanceID}
listener := &realtime.Listener{ConnString: cfg.DatabaseURL, Hub: hub, Origin: instanceID, Logger: log}
go func() { _ = listener.Run(ctx) }()     // reconnect otomatis, berhenti saat ctx selesai

// Outbox
enqueuer := &notify.Enqueuer{Channels: ncfg.Channels(), PublicURL: ncfg.PublicURL}
dispatcher := &notify.Dispatcher{DB: pool, Senders: ncfg.Senders(pool, log), Logger: log}
go func() { _ = dispatcher.Run(ctx) }()   // aman dijalankan di banyak instance (SKIP LOCKED)

// HTTP
mux.Handle("GET /api/v1/events", hub)
(&notify.API{
	DB:             pool,
	Viewer:         func(r *http.Request) string { id, _ := auth.ResolveViewer(r); return id },
	VAPIDPublicKey: ncfg.VAPID.PublicKey,
	Available:      ncfg.Channels(),
}).Register(mux)

// Shutdown: hub.Close() sebelum server.Shutdown (WebSocket adalah koneksi hijacked, Shutdown tidak menunggunya),
// lalu batalkan ctx untuk listener dan dispatcher. Dispatcher tetap mencatat hasil kirim yang sedang berjalan.
```

`/api/health` bisa memakai `hub.Count()` sebagai pengganti `s.hub.Count()` v1.

## 3. Yang dipanggil inti

### 3a. Enqueue (di transaksi yang sama dengan notifikasi in-app)

```go
// di service inti, setelah INSERT INTO notifications (...) di tx yang sama
_, err := enqueuer.Enqueue(ctx, tx, notify.Notification{
	ID:     n.ID,        // notifications.id
	UserID: n.UserID,
	Kind:   n.Kind,      // booking | chat | sengketa | sistem | verifikasi | papan | otp | ...
	Title:  n.Title,
	Body:   n.Body,
	Href:   n.Href,      // path aplikasi, mis. "/sewaan/b-123"
	// Tag: opsional, notifikasi push dengan tag sama saling menimpa di perangkat (default = ID)
	// Important: true untuk melewati jam tenang (sengketa/otp/keamanan otomatis penting)
})
```

- Rollback transaksi inti = baris outbox ikut batal. Idempoten per `(notification_id, channel)`.
- Pemilihan channel: channel aktif di server **dan** preferensi user untuk kategori itu menyala **dan** user bisa
  dihubungi (punya ≥ 1 baris `push_subscriptions`; `users.email` tidak kosong). Alamat email disalin ke payload
  saat antre.
- Pemetaan kind → kategori ada di `notify.CategoryOf`: `booking/papan/pembayaran/ulasan` → booking,
  `chat/pesan` → chat, `sengketa` → sengketa, sisanya (termasuk `verifikasi`, `otp`) → sistem.
- **Ketergantungan skema:** `Enqueue` membaca `users.email` (kolom di ROADMAP §3). Kalau inti menamainya lain,
  ubah satu query di `outbox.go`.

### 3b. Event realtime

Setiap transaksi yang mengubah data menghasilkan satu `realtime.Event{Version, Users, Admins, Public}`:

- `Users`: user yang snapshot terbatasnya berubah (para pihak booking, pemilik listing, penerima notifikasi, ...).
- `Admins`: semua admin perlu refetch (antrean verifikasi, laporan, sengketa, ...).
- `Public`: data publik berubah (listing, profil publik, ulasan yang sudah tampil, konfigurasi). Semua koneksi
  termasuk tamu refetch.

Dua cara mengirim, pilih salah satu per aksi:

```go
// (1) setelah commit: langsung ke hub lokal + NOTIFY ke instance lain
if err := tx.Commit(ctx); err == nil {
	_ = publisher.Publish(ctx, ev)
}

// (2) di dalam transaksi: Postgres hanya mengirim NOTIFY kalau commit; semua instance (termasuk asal) menerima
//     lewat Listener. Lebih atomik, tambah latensi satu round-trip NOTIFY.
_ = publisher.PublishTx(ctx, tx, ev)
```

Payload NOTIFY > 7900 byte otomatis diganti versi ringkas `{Version, Public: true}` untuk instance lain (hub lokal
tetap menerima daftar user lengkap). Setelah listener tersambung ulang, hub lokal menerima event `Public` supaya
klien refetch perubahan yang terlewat.

## 4. Mengganti hub v1

1. Hapus `internal/httpapi/hub.go` dan field `hub` di `Server`.
2. `GET /api/v1/events` → `realtime.Hub` (lihat §2). Tidak ada lagi `origins := []string{"*"}`; daftar origin
   diambil dari `CORS_ORIGINS`. Origin yang sama dengan host API dan klien tanpa header `Origin` selalu boleh.
   Untuk demo yang memang terbuka, `"*"` masih didukung.
3. `commit()` v1 (`s.hub.Broadcast(changed)`) diganti `publisher.Publish(ctx, ev)` dengan `ev` dari service.
4. Format pesan tidak berubah: `{"type":"hello","version":n}` saat terhubung, `{"type":"changed","version":n}`
   untuk tiap event. `connectEvents` di `sewain-web/src/lib/api.ts` jalan tanpa perubahan.
5. **Autentikasi WebSocket:** browser tidak bisa mengirim header `Authorization` di WebSocket. Dengan cookie sesi
   (produksi, D6) cookie terkirim otomatis. Untuk mode bearer per tab (demo), resolver perlu membaca token dari query
   (mis. `?token=`) dan `connectEvents` perlu menambahkannya (tugas `web-v2`). Tanpa token, koneksi diperlakukan
   sebagai tamu (hanya event `Public`), bukan ditolak.

Perilaku hub:

- Batas 5 koneksi per user per instance (`MaxConnsPerUser`); koneksi ke-6 dapat HTTP 429 sebelum upgrade (klien web mundur
  sendiri dengan backoff). Tamu tidak dibatasi kecuali `MaxGuestConns` diisi.
- Ping tiap 25 detik, pong ditunggu 10 detik. Antrean kirim 16 pesan per koneksi; koneksi yang antreannya penuh
  atau menulis > 5 detik diputus. `Publish` tidak pernah menunggu klien.

## 5. Kontrak JSON untuk worker web

Semua galat: `{"ok": false, "error": "<pesan Bahasa Indonesia>"}`. Endpoint user (selain `vapid-key`) butuh sesi
(401 `"Masuk dulu, ya."`). Request yang mengubah data tetap melewati middleware CSRF/Origin milik `auth`.

### `GET /api/v1/push/vapid-key` (publik)

```json
{ "ok": true, "publicKey": "BEl6...base64url" }
```

404 kalau push tidak aktif di server → sembunyikan opsi push di UI.

### `POST /api/v1/push/subscriptions`

Body = `JSON.stringify(subscription)` dari `PushManager.subscribe` apa adanya:

```json
{ "endpoint": "https://fcm.googleapis.com/fcm/send/…", "expirationTime": null,
  "keys": { "p256dh": "BNc…", "auth": "tBH…" } }
```

201 `{"ok": true}`. Endpoint harus `https` di push service yang dikenal (FCM, Mozilla, Windows, Apple), kunci harus
valid (p256dh 65 byte, auth 16 byte); kalau tidak 400. Endpoint yang sama dari akun lain berpindah ke akun yang
sedang masuk. Maksimal 10 langganan per user (yang paling lama dihapus).

### `DELETE /api/v1/push/subscriptions`

Body `{"endpoint": "https://…"}` → 200 `{"ok": true, "removed": true|false}`. Panggil saat user mematikan push di
perangkat itu dan saat logout.

### `GET /api/v1/me/notification-prefs`

```json
{
  "ok": true,
  "prefs": {
    "channels": {
      "inapp":    { "booking": true,  "chat": true,  "sengketa": true, "sistem": true },
      "webpush":  { "booking": true,  "chat": true,  "sengketa": true, "sistem": true },
      "email":    { "booking": false, "chat": false, "sengketa": true, "sistem": true },
      "whatsapp": { "booking": false, "chat": false, "sengketa": false, "sistem": false }
    },
    "quietHours": { "enabled": true, "start": "22:00", "end": "06:00", "timezone": "Asia/Jakarta" }
  },
  "available": { "inapp": true, "webpush": true, "email": true, "whatsapp": false },
  "pushSubscriptions": 1
}
```

- `available.<channel> = false` → channel tidak dikonfigurasi di server; tampilkan non-aktif.
- `inapp` selalu `true` dan tidak bisa diubah. `webpush` hanya terkirim kalau `pushSubscriptions > 0`.
- Jam tenang menunda push/email kategori biasa sampai `end`. Notifikasi penting (sengketa, OTP, keamanan) tetap
  langsung terkirim. Tulis di UI: "Kecuali notifikasi penting seperti sengketa."

### `PUT /api/v1/me/notification-prefs`

Patch parsial, semua field opsional:

```json
{ "channels": { "email": { "booking": true }, "webpush": { "chat": false } },
  "quietHours": { "enabled": false, "start": "23:00", "end": "07:00", "timezone": "Asia/Jakarta" } }
```

200 dengan bentuk sama seperti GET. 400 untuk jam bukan `JJ:MM`, zona waktu tidak dikenal, channel/kategori tidak
dikenal.

## 6. Service worker web untuk push (F6.5)

`public/sw.js` sekarang hanya cache. Tambahkan:

```js
self.addEventListener("push", (event) => {
  let m = {};
  try { m = event.data ? event.data.json() : {}; } catch {}
  event.waitUntil(self.registration.showNotification(m.title || "Sewain", {
    body: m.body || "",
    tag: m.tag,                       // notifikasi dengan tag sama saling menimpa
    data: { href: m.href || "/notifikasi" },
    icon: "/icons/icon-192.png",
    badge: "/icons/icon-192.png",
    lang: "id",
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data?.href || "/", self.location.origin).href;
  event.waitUntil(clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
    const same = list.find((c) => c.url === url);
    if (same) return same.focus();
    const any = list.find((c) => new URL(c.url).origin === self.location.origin);
    return any ? any.navigate(url).then((c) => c && c.focus()) : clients.openWindow(url);
  }));
});

// Browser memutar kunci langganan: daftar ulang ke server.
self.addEventListener("pushsubscriptionchange", (event) => {
  event.waitUntil((async () => {
    const res = await fetch("/api/v1/push/vapid-key");
    if (!res.ok) return;
    const { publicKey } = await res.json();
    const sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: publicKey });
    await fetch("/api/v1/push/subscriptions", { method: "POST", credentials: "include",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify(sub) });
  })());
});
```

Alur opt-in di halaman (setelah user menekan tombol, bukan saat halaman dibuka):

1. `GET /api/v1/push/vapid-key` (404 → sembunyikan fitur).
2. `Notification.requestPermission()` → `"granted"`.
3. `registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: publicKey })`
   (string base64url diterima Chrome/Firefox; untuk Safari ubah ke `Uint8Array`).
4. `POST /api/v1/push/subscriptions` dengan `JSON.stringify(subscription)`.
5. Matikan: `subscription.unsubscribe()` + `DELETE /api/v1/push/subscriptions`. Logout juga memanggil DELETE.

Payload push: `{title, body, href, tag}`, ≤ 3 KB, `href` berupa path relatif. iOS hanya menerima push untuk PWA
yang sudah "Add to Home Screen" (iOS 16.4+).

Rute halaman preferensi yang ditautkan dari email: `/profil/notifikasi` (ubah lewat `NOTIFY_PREFS_PATH` kalau web
memilih rute lain).

## 7. Outbox dan dispatcher

- Status: `pending` (belum dicoba) → `sent`, atau `failed` (menunggu retry) → … → `dead`.
- Retry setelah gagal ke-1..5: 30 detik, 2 menit, 10 menit, 1 jam, 6 jam. Gagal ke-6 → `dead`. Galat permanen
  (alamat ditolak 5xx SMTP, 4xx Resend selain 429, semua langganan push 404/410, channel tidak dikonfigurasi)
  langsung `dead`.
- Klaim: `UPDATE … FROM (SELECT … FOR UPDATE SKIP LOCKED LIMIT 20)` sekaligus menaikkan `attempts` dan menyewa baris
  5 menit (`next_attempt_at = now + lease`). Pengiriman berjalan di luar transaksi. Kalau proses mati di tengah
  jalan, baris dicoba lagi setelah sewa habis (at-least-once). Resend memakai `Idempotency-Key` = id outbox, email
  SMTP memakai `Message-ID` yang sama, push memakai `tag` sehingga duplikat jarang terlihat.
- Push ke beberapa perangkat: sukses kalau minimal satu perangkat menerima (retry akan menduplikasi di perangkat
  itu). Langganan yang dibalas 404/410 dihapus; `last_success_at` diperbarui untuk yang berhasil.
- Pantau: `SELECT status, count(*) FROM notification_outbox GROUP BY 1;` dan baris `dead` terbaru beserta
  `last_error` (bisa masuk metrik `ops`).

## 8. WhatsApp

Channel `whatsapp` hanya stub: kolom preferensi ada (default mati), `Config.Channels()` selalu `false`, sehingga
`Enqueue` tidak pernah mengantre WhatsApp dan `WhatsAppStub` menandai job `dead` kalau ada. Notifikasi WhatsApp
butuh akun BSP berbayar + template pesan yang disetujui Meta (ROADMAP §9 #2/#9, PRD §21 pertanyaan 8). Setelah
akun ada: implementasikan `notify.Sender` untuk provider itu, isi nomor dari `users.phone` saat `Enqueue`, dan
aktifkan channel di `Config.Channels()`.

## 9. Keputusan yang diambil di paket ini

| Keputusan | Alasan |
|---|---|
| Hub menolak koneksi ke-6 per user dengan 429 (bukan memutus yang tertua) | Memutus yang tertua membuat tab-tab saling menendang karena klien auto-reconnect |
| Tamu tanpa sesi diterima sebagai tamu, tidak ditolak | Klien web sekarang selalu membuka WebSocket; tamu butuh event `Public` untuk katalog |
| `Publish` lokal + NOTIFY dengan `origin`, listener melewati origin sendiri | Latensi rendah di instance asal, tidak ada pesan dobel |
| Listener menyiarkan `Public` setelah reconnect | NOTIFY yang terlewat saat putus tidak bisa diulang; refetch menjamin konsisten |
| `notification_prefs` satu baris per user dengan kolom eksplisit (bukan JSONB) | D3: JSONB hanya untuk snapshot; kolom + default SQL = default produk |
| Jam tenang menunda (bukan membuang) push/email | Request sewa jam 23.00 tetap berguna jam 06.00; in-app sudah ada seketika |
| Email alamat disalin saat antre | Dispatcher tidak bergantung pada skema `users`; isi email konsisten dengan saat kejadian |
| Endpoint push dibatasi ke host push service dikenal | Mencegah SSRF (server POST ke URL buatan user) |
| `time/tzdata` di-embed | Jam tenang butuh `Asia/Jakarta` di image minimal tanpa zoneinfo |
| ID tabel `text` berprefiks (`ps-…`, `nob-…`) dari `gen_random_uuid()` | Konsisten dengan konvensi ID inti |

## 10. Masih terbuka

- Kolom `users.email` (dan nanti `users.phone` untuk WhatsApp) harus ada di skema inti.
- Resolver viewer (`auth`) dan sumber versi (`core-v2`) disuntik saat wiring.
- Rate limit endpoint push/prefs dan metrik outbox: milik `ops` (F5.2/F5.3).
- Record DNS SPF/DKIM untuk domain pengirim email setelah akun Resend/Brevo ada (ROADMAP §9 #3).

# Runbook operasional sewain-api (F5.7)

Untuk staging (`/opt/stacks/apps/sewain-staging`, branch `develop`) dan produksi (`/opt/stacks/apps/sewain-prod`,
F7.1). Demo (`/opt/stacks/apps/sewain`, branch `main`) dibekukan sampai gerbang ROADMAP §6 lulus. Semua perintah
dijalankan di server sebagai user biasa (grup docker). Ganti `<stack>` dan `<api>` (nama container API:
`sewain-staging-api`, `sewain-prod-api`, …).

Jalur trafik: Cloudflare Tunnel → `localhost:80` → `caddy-proxy` → container di `gateway-net`. Perubahan hostname/DNS
dikerjakan lewat opencode (lihat aturan server), bukan dashboard Cloudflare.

## 1. Deploy

Rutin: push/merge ke `develop` (staging) atau `main` (demo/produksi). CI menjalankan lint, tes, tes integrasi
Postgres, membangun image `ghcr.io/mrfroglab/sewain-api:<develop|latest>` + `:sha-<7 karakter>`, lalu
`docker compose pull api && up -d --no-deps api` dan menunggu status `healthy`.

Sebelum deploy versi yang menambah env:

1. Bandingkan `.env` stack dengan [`deploy/.env.example`](.env.example). Isi variabel baru dulu (`chmod 600`).
2. Pastikan compose stack meneruskan `.env` ke service `api` (`env_file: .env`).
3. Deploy. Server menolak start dan mencetak **semua** masalah konfigurasi sekaligus kalau ada yang salah:
   `docker logs <api> | head -30`.

Setelah deploy:

```sh
curl -s https://<host>/api/health     # {"ok":true,"version":"develop","commit":"<sha>",...}
curl -s https://<host>/api/ready      # 200 = DB + semua migrasi terpasang
curl -s https://<host>/api/v1/config  # appEnv, demoMode, paymentMode, otpProvider sesuai harapan
docker logs --since 5m <api> | grep -c '"level":"ERROR"'
```

Staging: jalankan suite regresi web (`pnpm e2e --base https://sewain-staging.hallojanu.me`, 5 suite).

Migrasi jalan otomatis saat start (advisory lock, aman dengan beberapa instance). Manual:
`docker exec <api> /app/api migrate status`.

## 2. Rollback ke image sebelumnya

Setiap build punya tag `sha-<commit>`. Cari tag yang terakhir sehat (`docker image ls ghcr.io/mrfroglab/sewain-api`
atau halaman package GHCR), lalu:

```sh
cd /opt/stacks/apps/<stack>
docker pull ghcr.io/mrfroglab/sewain-api:sha-abc1234
docker tag ghcr.io/mrfroglab/sewain-api:sha-abc1234 ghcr.io/mrfroglab/sewain-api:<develop|latest>
docker compose up -d --no-deps api
docker inspect -f '{{.State.Health.Status}}' <api>   # tunggu healthy
```

Migrasi tidak di-rollback otomatis. Migrasi Sewain bersifat aditif. Kalau sebuah rilis memuat migrasi yang
memecah versi lama (tercatat di catatan rilis), rollback = restore backup sebelum deploy (§3) atau
`goose down` manual yang sudah diuji di staging. Matikan deploy CI berikutnya (revert commit di branch) supaya
image yang rusak tidak terpasang lagi.

## 3. Backup dan restore

Backup harian (timer systemd user 02:40): `pg_dump -Fc` + folder uploads, 7 harian + 4 mingguan, SHA256SUMS, salinan
offsite terenkripsi kalau `BACKUP_REMOTE` diisi.

Uji restore (wajib bulanan dan sebelum go-live; tidak menyentuh DB produksi):

```sh
/opt/stacks/apps/<stack>/backup.sh verify    # restore dump terbaru ke DB sementara, hitung baris, hapus
```

Restore penuh (data hilang/rusak):

1. Umumkan maintenance. Hentikan API: `docker compose stop api` (DB tetap jalan).
2. Pilih backup: `ls /opt/stacks/backup/<stack>/` lalu cek `sha256sum -c SHA256SUMS` di folder itu. Kalau lokal
   rusak, ambil dari offsite: `rclone copy <BACKUP_REMOTE>/<tanggal> /tmp/restore-<tanggal>`.
3. Simpan kondisi sekarang dulu (untuk forensik): `docker exec <db> pg_dump -U sewain -Fc sewain > /tmp/before-restore.dump`.
4. Pulihkan DB:

   ```sh
   docker exec <db> psql -U sewain -d postgres -c "DROP DATABASE sewain WITH (FORCE)" -c "CREATE DATABASE sewain OWNER sewain"
   docker exec -i <db> pg_restore -U sewain -d sewain --no-owner < /opt/stacks/backup/<stack>/<tanggal>/db.dump
   ```

5. Pulihkan berkas (storage lokal): `rsync -a --delete <backup>/uploads/ /opt/stacks/apps/<stack>/data/uploads/` lalu
   `sudo chown -R 65532:65532 data/uploads` (user distroless). Dengan R2, objek tidak ikut backup lokal dan tidak
   perlu dipulihkan (write-once, object lock).
6. `docker compose start api`, cek §1 "Setelah deploy". Data setelah waktu backup hilang (RPO 24 jam): catat dan
   kabari pengguna terdampak.

Pindah ke server baru (D11): pasang Docker + stack, salin `.env` dari password manager, restore seperti di atas,
pindahkan hostname lewat opencode.

## 4. Rotasi kunci

Kunci baru selalu dibuat di server (`openssl rand …`) dan disimpan juga di password manager. Restart = `docker compose
up -d --no-deps api`.

| Kunci | Cara | Dampak |
|---|---|---|
| `KYC_KEYS` | Tambah kunci baru di **akhir**: `KYC_KEYS=v1:<lama>,v2:<baru>`, restart. Data baru memakai `v2`, data lama tetap terbaca (id kunci ada di ciphertext). Hapus `v1` hanya setelah tidak ada data `v1` (berkas KYC dihapus 30 hari setelah keputusan; NIK di `identities` perlu re-enkripsi, job `Rewrap` belum dibuat, jadi untuk sekarang `v1` disimpan). | Tidak ada downtime |
| `MEDIA_SIGNING_KEY` | Pindahkan nilai lama ke `MEDIA_SIGNING_KEY_PREVIOUS`, isi `MEDIA_SIGNING_KEY` baru, restart. Setelah ≥ 75 menit kosongkan `_PREVIOUS`, restart. | URL foto lama tetap valid sampai kedaluwarsa |
| VAPID | `api vapid gen` → ganti `VAPID_PUBLIC_KEY`/`VAPID_PRIVATE_KEY`, restart, lalu `DELETE FROM push_subscriptions;` (langganan lama terikat kunci lama). Web meminta izin push lagi. | Push berhenti sampai pengguna berlangganan ulang. Rotasi hanya kalau kunci bocor |
| `OTP_PEPPER` | Ganti, restart. | Kode OTP yang sedang berjalan batal (5 menit) |
| `NIK_PEPPER` | **Jangan dirotasi.** Kalau bocor: tetap pakai (HMAC NIK tanpa data lain tidak membuka NIK), catat insiden. Rotasi berarti menghitung ulang `nik_hmac` dari NIK terenkripsi, butuh job khusus. | – |
| `DB_PASSWORD` | `ALTER USER sewain PASSWORD '…'` di DB, ubah `.env`, `docker compose up -d`. | Restart singkat |
| Token Zenziva/Resend/R2 | Buat token baru di vendor, ganti env, restart, cabut token lama. | – |

Kalau kunci dicurigai bocor: rotasi segera lalu jalankan §5.

## 5. Insiden kebocoran data pribadi (UU PDP No. 27/2022)

Kegagalan pelindungan data pribadi wajib diberitahukan **tertulis paling lambat 3 × 24 jam** kepada subjek data dan
lembaga pelaksana PDP (Pasal 46). Isi pemberitahuan: data apa yang terungkap, kapan dan bagaimana, serta upaya
penanganan dan pemulihan.

1. **Jam 0: deteksi dan catat.** Tulis kronologi (jam WIB, siapa menemukan, bukti). Simpan log:
   `docker logs <api> --since 72h > /tmp/incident-<tgl>.log` dan `audit_log` terkait
   (`SELECT * FROM audit_log WHERE at > now() - interval '7 days' ORDER BY at`).
2. **Bendung.** Sesuai sumber: cabut sesi (`UPDATE auth_sessions SET revoked_at = now() WHERE revoked_at IS NULL;`),
   rotasi kunci (§4), matikan fitur (deploy image sebelumnya, §2) atau putus akses (`docker compose stop api`,
   halaman maintenance). Jangan hapus bukti.
3. **Nilai dampak (≤ 24 jam).** Data mana (HP, email, foto bukti, KTP/selfie terenkripsi, NIK tersamar), berapa
   pengguna, apakah kunci ikut bocor (data KYC terenkripsi tanpa kunci tidak terbaca). Daftar pengguna terdampak dari
   DB + log akses (`request_id`, `user_id`).
4. **Beri tahu (≤ 72 jam sejak diketahui).** Janu (penanggung jawab) menyetujui teks. Kirim ke pengguna terdampak
   (email/WA/in-app) dan ke lembaga PDP (Komdigi selama lembaga belum terbentuk). Isi: apa yang terjadi, data apa,
   langkah yang sudah diambil, yang perlu dilakukan pengguna (waspada penipuan atas nama Sewain), kontak.
5. **Pulihkan dan perbaiki.** Perbaikan + tes yang mereproduksi celahnya, review oleh orang kedua, deploy.
6. **Pasca-insiden (≤ 7 hari).** Laporan singkat di repo (`docs/security/incident-<tgl>.md`): kronologi, akar
   masalah, dampak, perbaikan, pencegahan.

## 6. Pekerjaan berkala

| Kapan | Apa |
|---|---|
| Otomatis tiap 15 detik | Job waktu: request kedaluwarsa 24 jam, telat, selesai otomatis 48 jam, L1→L2, rilis ulasan |
| Otomatis tiap `RETENTION_INTERVAL` (1 jam) | Retensi: foto bukti 90 hari setelah selesai (kecuali sengketa), berkas KYC 30 hari setelah keputusan, data identitas akun terhapus, sesi/OTP kedaluwarsa, `idempotency_keys` > 24 jam, notifikasi terbaca > 180 hari, outbox terkirim/mati > 30 hari, alarm admin untuk KYC menunggu > 48 jam |
| Harian | Backup (timer). Cek notifikasi gagal backup |
| Mingguan | `SELECT status, count(*) FROM notification_outbox GROUP BY 1;` (baris `dead` + `last_error`), log ERROR, `/api/v1/admin/metrics` |
| Bulanan | `backup.sh verify`, `govulncheck` (job CI `security`), update image dasar |

Metrics (`METRICS_ADDR`): `sewain_http_*`, `sewain_websocket_connections_active`, `sewain_job_duration_seconds{job="retention"}`,
`sewain_outbox_pending{channel}`, `sewain_db_pool_*`. Scrape dari jaringan internal saja.

## 7. Cek go-live (F7.5)

- [ ] `APP_ENV=production`, `DEMO_MODE=false`, `SNAPSHOT_SCOPE=scoped`, `PAYMENT_MODE=offline`, `OTP_PROVIDER=zenziva`.
      Bukti penolakan: start dengan `DEMO_MODE=true` gagal (tes `cmd/api` `TestProductionRefusesDemoConfig` + uji manual sekali).
- [ ] Kunci produksi **baru** (bukan salinan staging): `OTP_PEPPER`, `NIK_PEPPER`, `KYC_KEYS`, `MEDIA_SIGNING_KEY`,
      VAPID, `DB_PASSWORD`; tersimpan di 2 tempat.
- [ ] `CORS_ORIGINS`/`ALLOWED_ORIGINS` = domain produksi, `TRUSTED_PROXY_CIDRS` = subnet `gateway-net`,
      `SESSION_COOKIE_SECURE=true`, `TERMS_VERSION`/`PRIVACY_VERSION` = versi teks final.
- [ ] OTP Zenziva terkirim ke nomor sendiri (WA dan cadangan SMS).
- [ ] Admin awal: masuk lewat OTP sekali, lalu `docker exec <api> /app/api admin grant <hp>`.
- [ ] Data awal: aturan jaminan, komisi 0% (pencatatan "seandainya"), kata terlarang; impor listing Early Lender
      (`POST /api/v1/admin/listings/import?dryRun=1` dulu).
- [ ] `/api/health`, `/api/ready`, `/api/v1/config` sesuai; metrics tidak bisa diakses dari internet.
- [ ] Backup pertama + `backup.sh verify` lulus hari yang sama; offsite (`BACKUP_REMOTE`) + notifikasi gagal aktif.
- [ ] Uji beban k6 (F5.5) dan review keamanan tanpa temuan tinggi (F5.8).
- [ ] Monitoring uptime + alarm (health, backup, error rate) mengirim alarm uji.
- [ ] Runbook ini diuji sekali (deploy, rollback, restore) di staging.

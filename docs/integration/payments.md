# Integrasi paket `payments` + `ekyc` (Fase 8)

Paket mandiri untuk ROADMAP F8.1–F8.5. **Belum dipasang** ke `cmd/api`, `internal/httpapi`, atau `internal/service`,
dan baru dipasang setelah ada badan usaha dan akun Xendit (ROADMAP §9 #12–13). Selama itu `PAYMENT_MODE` tetap
`offline` (pilot) atau `simulated` (demo), dan tabel di bawah kosong saja.

| Paket / berkas | Isi |
|---|---|
| `internal/payments` | Ledger double-entry (`ledger.go`, `store.go`), rencana uang murni (`settlement.go`: `PlanPayment`, `PlanRelease`, `PlanSettlement`), komisi (`commission.go`), interface `Provider` + `FakeProvider` + adapter `Xendit`, `Service` (intent, webhook, eksekusi settlement, refund, payout, laporan, rekonsiliasi), handler HTTP, `ConfigFromEnv` |
| `internal/ekyc` | Interface `Vendor` (OCR KTP, liveness, face match, Dukcapil), `Provider` yang memenuhi `kyc.KYCProvider`, stub `Manual`, placeholder `XenditEKYC`/`Verihubs`, tabel biaya per panggilan |
| `internal/db/migrations/00200_payments_ledger.sql` | `payment_intents`, `ledger_txns`, `ledger_entries` (+ trigger seimbang & append-only), `ledger_balance_snapshots`, view `ledger_balances`, `webhook_events`, `refunds`, `payouts`. FK hanya ke `users(id)`; `booking_id` berupa `text` tanpa FK supaya paket ini tidak bergantung pada skema inti |
| `internal/db/dbq/models.go` | Hasil `make generate` (sqlc) karena ada tabel baru. Commit terpisah; kalau bentrok saat merge, cukup jalankan ulang `make generate` |

Tidak ada dependensi baru.

## 1. Env

Semua kosong = `PAYMENT_MODE=offline`, paket tidak aktif. `ConfigFromEnv(os.Getenv)` menolak kombinasi setengah jadi.

| Env | Wajib | Default | Keterangan |
|---|---|---|---|
| `PAYMENT_MODE` | tidak | `offline` | `simulated` (demo, tombol "lunas"), `offline` (pilot, bayar langsung ke penyedia), `gateway` (paket ini) |
| `PAYMENT_PROVIDER` | kalau `gateway` | `xendit` | `xendit` atau `fake` (sandbox lokal; **ditolak** kalau `APP_ENV=production`) |
| `PAYMENT_COMMISSION_ACTIVE` | tidak | `false` | Sakelar global FR-K6. `false` = komisi tidak ditagih walau snapshot berkata aktif; nilai "seandainya" tetap dilaporkan |
| `PAYMENT_INTENT_TTL` | tidak | `30m` | Masa berlaku QR (format Go duration, minimal `1m`) |
| `PAYMENT_MDR_BP` | tidak | `70` | Biaya gateway dalam basis poin (QRIS 0,7% = 70), dicatat ke `platform:fees` |
| `PAYMENT_FAKE_TOKEN` | kalau `fake` | – | Token callback provider palsu (header `X-Fake-Callback-Token`) |
| `XENDIT_SECRET_KEY` | kalau `xendit` | – | Secret API key (`xnd_development_…` untuk sandbox). HTTP Basic, password kosong |
| `XENDIT_CALLBACK_TOKEN` | kalau `xendit` | – | Token verifikasi webhook dari dashboard Xendit |
| `XENDIT_BASE_URL` | tidak | `https://api.xendit.co` | |
| `XENDIT_API_VERSION` | tidak | `2024-11-11` | Header `api-version` untuk `/v3/payment_requests` |
| `XENDIT_QRIS_CHANNEL_CODE` | tidak | `QRIS` | **PERLU VERIFIKASI SANDBOX** |
| `XENDIT_STATUS_PATH` | tidak | `/v3/payment_requests` | GET `{path}/{id}`. **PERLU VERIFIKASI SANDBOX** |
| `XENDIT_REFUND_PATH` | tidak | `/refunds` | |
| `XENDIT_PAYOUT_PATH` | tidak | kosong (payout mati) | **PERLU VERIFIKASI SANDBOX**. Kosong = `ErrNotSupported`, payout tetap antre |

## 2. Alur uang

Akun ledger (rupiah `bigint`, entri bertanda: positif = saldo akun bertambah, setiap transaksi berjumlah nol):

| Akun | Arti |
|---|---|
| `escrow:<bookingId>` | Sewa ditahan sampai serah terima (sudah dikurangi bagian komisi penyedia) |
| `deposit:<bookingId>` | Deposit uang ditahan gateway (FR-J8) |
| `provider:<userId>` | Hak penyedia yang belum di-payout |
| `renter:<userId>` | Hak penyewa yang belum di-refund |
| `platform:commission` | Komisi |
| `platform:fees` | Biaya gateway (MDR). Negatif = biaya yang ditanggung platform (PRD §14.3) |
| `gateway:<provider>` | Batas uang masuk/keluar di gateway. **Tambahan** di luar enam akun di brief: tanpa akun lawan, transaksi bayar/refund tidak bisa berjumlah nol. `−saldo` = uang yang seharusnya ada di saldo merchant |

Contoh: sewa Rp100.000, komisi 10% dibagi 50:50, deposit Rp200.000, QRIS 0,7%.

```
 PENYEWA                        GATEWAY (Xendit)                         LEDGER
 ───────                        ────────────────                         ──────
 scan QR sewa  Rp105.000 ─────► payment.capture ──► webhook ──► pay:<intent sewa>
   (sewa + 5.000 bagian           (x-callback-token)              gateway:xendit  −104.265
    komisi penyewa)                                               escrow:b1        +95.000
                                                                  platform:commission +10.000
                                                                  platform:fees      −735
 scan QR deposit Rp200.000 ───► payment.capture ──► webhook ──► pay:<intent deposit>
                                                                  gateway:xendit  −198.600
                                                                  deposit:b1      +200.000
                                                                  platform:fees    −1.400
 ── serah terima (ACTIVE) ───────────────────────────────────► release:b1
                                                                  escrow:b1        −95.000
                                                                  provider:p       +95.000 ──► payout (job)
 ── COMPLETED tanpa masalah ──────────────────────────────────► settle:b1 (case "selesai")
                                                                  deposit:b1      −200.000
                                                                  renter:r        +200.000 ──► refund QRIS penuh (job)
 ◄──────────── refund.succeeded ◄── webhook ─────────────────► refund:<id>
                                                                  renter:r        −200.000
                                                                  gateway:xendit  +200.000
 ── atau: sengketa RESOLVED "potong deposit Rp260.000 (biaya perbaikan)" ──► settle:b1 (case "rusak-ringan")
                                                                  deposit:b1      −200.000
                                                                  provider:p      +200.000 ──► payout
                                                                  + intent baru jenis "claim" Rp60.000 ke penyewa
                                                                    (lunas → provider:p +60.000 → payout)
```

Aturan kasus (fungsi murni `PlanSettlement`, diuji tabel untuk setiap baris PRD §11.3):

| Case (`Outcome.Case`) | Uang |
|---|---|
| `selesai`, `lecet-ringan`, `jaminan-ditahan`, `deposit-ditahan`, `klaim-palsu` | Deposit kembali penuh. Escrow yang masih ada ke penyedia. `Claim` wajib 0 |
| `rusak-ringan`, `rusak-berat`, `hilang`, `tidak-dikembalikan`, `kendaraan` | `min(Claim, deposit)` ke penyedia, sisa deposit kembali, `Claim − deposit` jadi intent `claim` baru ke penyewa |
| `telat` | Sama, tetapi `Claim` dipotong `ClaimCap` (default `agreement.lateFeeCap`) |
| `tidak-sesuai`, `penyedia-batal`, `penyewa-tidak-datang` | Batal sebelum serah terima: sewa + bagian komisi penyewa + deposit kembali, komisi dibalik. Ditolak kalau escrow sudah dilepas |

`Outcome.RentRefund` (opsional) mengembalikan sebagian sewa dari escrow untuk kasus non-batal.

## 3. Endpoint (konstruktor `payments.NewHandlers(svc).Register(mux)`)

| Endpoint | Siapa | Isi |
|---|---|---|
| `POST /api/v1/payments/intents` | penyewa booking `ACCEPTED` | Body `{"bookingId","kind":"rent"\|"deposit"}`. 201 + `intent` (berisi `qrString`, `expiresAt`). Dipanggil ulang = intent hidup yang sama. 409 `Sudah lunas.` kalau sudah dibayar |
| `GET /api/v1/payments/intents/{id}` | pembayar atau admin | Status intent (pending kedaluwarsa otomatis tampil `expired`). Web polling ini atau menunggu event realtime |
| `POST /api/v1/payments/webhooks/{provider}` | provider | **Tanpa sesi dan tanpa cek Origin** (autentikasi lewat token callback). 200 = tersimpan/duplikat, 401 = token salah, 5xx = gagal diproses → provider mengulang |
| `GET /api/v1/admin/payments/commission?from=YYYY-MM-DD&to=YYYY-MM-DD` | admin | `gateway` (komisi, fee, net dari ledger) + `seandainyaAktif` (FR-K6, booking tanpa pembayaran gateway). Tanggal WIB, `to` eksklusif |
| `GET /api/v1/admin/payments/reconcile?from=&to=` | admin | Selisih ledger vs status provider + invarian ledger |

Rute webhook harus dikecualikan dari middleware CSRF/Origin dan dari rate limit per user (boleh rate limit per IP
yang longgar). Batas body 256 KB sudah di handler.

## 4. Cara memasang ke service inti (saat `PAYMENT_MODE=gateway`)

```go
pcfg, err := payments.ConfigFromEnv(os.Getenv)
if err != nil { return err }
if pcfg.Mode == payments.ModeGateway {
	provs, err := pcfg.Providers()
	if err != nil { return err }
	paySvc, err := payments.NewService(pool, payments.SQLBookings{}, pcfg.Service, provs...)
	if err != nil { return err }
	payments.NewHandlers(paySvc).Register(mux)
	// jobs (advisory lock seperti job inti): lihat tabel di bawah
}
```

| Titik di inti | Yang dilakukan |
|---|---|
| `payBooking` | Saat `gateway`: aksi ini **tidak** lagi menandai lunas. Web memanggil `POST /payments/intents` (sewa, lalu deposit kalau jaminannya deposit uang) dan menampilkan QR. `booking.payment.status = "lunas"` diset inti ketika intent `rent` (dan `deposit` bila ada) `paid`: paling sederhana lewat job/hook setelah `HandleWebhook` yang membaca `payment_intents`. Status `lunas` jangan diset dari klien |
| `agreeCheckout` → `ACTIVE` | Setelah commit: `paySvc.ReleaseEscrow(ctx, bookingID)` (idempoten) |
| `confirmCollateral` / job 48 jam → `COMPLETED` | Setelah commit: `paySvc.SettleCompleted(ctx, bookingID)`. `ErrAlreadySettled` = sukses |
| `resolveDispute` → `RESOLVED` | Setelah commit: `paySvc.Settle(ctx, bookingID, payments.Outcome{Case, Claim})`. `Case` dari `dispute.case_type` (kode sama dengan `domain.CaseTypeFor`) atau pilihan admin; `Claim` dari `recommendation.amount`. `resolveDispute` perlu input terstruktur (case + nominal), bukan hanya teks `outcome` |
| `rejectAtCheckout` (barang tidak sesuai) | `Settle(… Outcome{Case: "tidak-sesuai"})` |
| `cancelBooking` oleh penyedia setelah bayar | `Settle(… Outcome{Case: "penyedia-batal"})` |
| Penyewa no-show (penyedia membatalkan) | `Settle(… Outcome{Case: "penyewa-tidak-datang"})` |

Semua fungsi `Settle*`/`ReleaseEscrow` berjalan di transaksi sendiri dan **idempoten** (id transaksi ledger =
`release:<booking>`, `settle:<booking>`). Pola yang disarankan: panggil setelah transaksi inti commit, lalu job
penyapu (mis. tiap 5 menit) memanggil ulang untuk booking `ACTIVE`/`COMPLETED`/`RESOLVED` yang belum punya
transaksi ledger-nya, supaya crash di antara dua commit tidak menghilangkan langkah uang.

Job berkala:

| Job | Interval | Fungsi |
|---|---|---|
| Kedaluwarsa intent | 1 menit | `ExpireIntents(ctx)` |
| Kirim refund | 1–5 menit | `ExecuteRefunds(ctx, 50)` |
| Kirim payout | harian / per jam | `ExecutePayouts(ctx, resolver, 50)`. `resolver(userID)` mengembalikan rekening/e-wallet penyedia (tabel rekening belum ada, lihat §8) |
| Snapshot saldo | harian | `SnapshotBalances(ctx, pool)` |
| Rekonsiliasi | harian | `Reconcile(ctx, kemarin, hari ini)` → kirim ke alarm (ops) kalau tidak kosong |

## 5. Keputusan yang diambil di paket ini

1. **Sewa dan deposit = dua intent QRIS terpisah.** Xendit hanya mendukung refund QRIS **penuh** (tanpa refund
   parsial, maksimal 30 hari). Dengan intent deposit sendiri, deposit yang kembali utuh bisa di-refund penuh ke
   metode bayar asal. Deposit yang kembali sebagian (setelah potongan sengketa) atau lewat 30 hari dikirim lewat
   **payout** ke rekening/e-wallet penyewa (`queueBack`). UX-nya dua kali scan; bisa digabung nanti kalau
   xenPlatform/split memberi jalan lain.
2. **Komisi ditagih saat sewa lunas**, dari snapshot kesepakatan (FR-K5) dan hanya dari nilai sewa (FR-K4).
   Bagian penyewa ditambahkan ke tagihan QR sewa, bagian penyedia dipotong dari escrow (FR-K3). Kalau snapshot
   sudah berisi `charged/renterPays/providerPays` yang konsisten, angka itu yang dipakai persis (yang dilihat dan
   disetujui kedua pihak); hitung ulang integer `ComputeCommission` bisa beda Rp1 pada pembagian split karena
   snapshot dihitung dengan float.
3. **MDR ditanggung platform** (PRD §14.3): `platform:fees` negatif. Apakah MDR dikembalikan saat refund QRIS
   belum diketahui → tidak dimodelkan (lihat §6).
4. **Pembayaran terlambat tetap diterima** (uang nyata): intent `expired`/`failed` yang menerima capture menjadi
   `paid` + `paid_late`. Kalau intent lain untuk booking+jenis yang sama sudah `paid`, itu **kelebihan bayar**:
   uang masuk ke `renter:<id>` dan refund penuh diantrekan.
5. **Webhook**: disimpan dulu (`webhook_events`, unik `provider+event_id`) lalu diproses di transaksi yang sama.
   Gagal proses = rollback (event tidak tersimpan) → 5xx → Xendit mengulang (hingga 6×). Id event Xendit dibentuk
   dari `payment.capture:<capture_id>`, `refund.*:<refund_id>` (dokumentasi menyebut id ini unik). Capture dengan
   nominal berbeda dari intent tidak dibukukan (hasil `amount_mismatch`, muncul di rekonsiliasi).
6. **Ledger append-only** dengan trigger; koreksi = transaksi pembalik. Keseimbangan dicek di Go (`Txn.Validate`)
   dan di DB (constraint trigger `DEFERRABLE INITIALLY DEFERRED` saat commit).
7. **Uang keluar dibukukan saat sukses**, bukan saat dikirim: `renter:`/`provider:` tetap berisi selama refund/payout
   belum `succeeded`, jadi saldo akun itu = utang yang belum dibayar.
8. **Penyewa no-show = refund penuh** (PRD hanya menyebut strike). Kebijakan pembatalan listing yang memotong uang
   belum dimodelkan; kasus batal hanya mendukung refund penuh.
9. eKYC: tidak pernah menolak otomatis. Semua lulus → `approved`; ada yang gagal → `pending` (antrean admin, dengan
   hasil cek); vendor error → error (inti jatuh ke antrean manual). Urutan cek dipilih supaya pengajuan yang pasti
   gagal tidak membayar cek Dukcapil (Rp4.000): OCR → face match → liveness → Dukcapil.

## 6. Wajib diverifikasi di sandbox Xendit

Fixture `internal/payments/testdata/*.json` ditandai **PERLU VERIFIKASI SANDBOX**: bentuknya diambil dari
dokumentasi, tetapi contoh QRIS Indonesia tidak ada di halaman publik.

| # | Hal | Status |
|---|---|---|
| 1 | `channel_code` QRIS untuk `/v3/payment_requests` (`QRIS`?) dan `channel_properties` yang diterima (`expires_at`? batas minimal/maksimal kedaluwarsa) | PERLU VERIFIKASI. Contoh publik hanya `QRPROMPTPAY` (TH) |
| 2 | Respons create berisi `actions[].descriptor = "QR_STRING"` untuk QRIS | PERLU VERIFIKASI |
| 3 | Endpoint status: `GET /v3/payment_requests/{id}` (yang terverifikasi hanya `GET /v3/payments/{payment_id}`) | PERLU VERIFIKASI (`XENDIT_STATUS_PATH`) |
| 4 | Apakah ada webhook kedaluwarsa untuk payment request (event yang terdokumentasi hanya `payment.capture`, `payment.authorization`, `payment.failure`) | Diasumsikan tidak ada; kedaluwarsa ditangani job lokal + rekonsiliasi |
| 5 | `POST /refunds`: perlu header `api-version`? Field respons id (`id` atau `refund_id`)? Header idempotensi? | Adapter mengirim `idempotency-key` dan membaca keduanya. PERLU VERIFIKASI |
| 6 | Refund QRIS: hanya penuh, ≤ 30 hari | Dari tabel fitur QR Xendit. Konfirmasi di sandbox + apakah MDR ikut kembali |
| 7 | Payout (Payouts API: path, body, `channel_code` bank/e-wallet, event webhook `payout.*`) | Belum diverifikasi sama sekali. `XENDIT_PAYOUT_PATH` kosong = mati |
| 8 | Nominal di webhook selalu bilangan bulat untuk IDR | Adapter menolak pecahan |
| 9 | Basic auth: secret key sebagai username, password kosong | Pola standar Xendit; konfirmasi |
| 10 | Uji ulang webhook dobel/terlambat dari dashboard ("resend") → `duplicate` | Tes lokal lulus; ulangi di sandbox |
| 11 | xenPlatform: `for-user-id`, `with-split-rule` di create payment request; apakah refund membalik split | Dokumentasi: fee split **tidak** dikembalikan otomatis saat refund |

## 7. xenPlatform dan eKYC vendor (rancangan, menunggu akun)

**xenPlatform (FR-J8, PRD §12.4).** Supaya Sewain tidak menampung dana di rekening sendiri:

- Opsi A (paling sederhana, dipakai kode sekarang): semua pembayaran masuk akun master Sewain di Xendit (dana
  ditahan Xendit, bukan rekening bank Sewain), lalu payout ke penyedia/penyewa. Perlu dicek ke konsultan apakah ini
  cukup untuk syarat "tidak menampung dana".
- Opsi B: penyedia = sub-account (Owned/Managed). `ChargeRequest.SubAccountID` → header `for-user-id`,
  `ChargeRequest.SplitRuleID` → header `with-split-rule` (sudah dikirim adapter bila diisi). Split rule
  (`POST /split_rules`, `routes[]` berisi `flat_amount`/`percent_amount`, `currency`, `destination_account_id`,
  `reference_id`) mengirim komisi ke akun master. Deposit tetap di akun master sampai settlement, lalu
  `POST /transfers` (`reference`, `amount`, `source_user_id`, `destination_user_id`; hanya kunci akun master) ke
  sub-account penyedia. Interface `payments.Splitter` sudah ada; implementasinya menunggu akun. Ledger tidak
  berubah: `gateway:` dipecah per akun bila perlu.

**eKYC vendor.** `ekyc.Provider{Vendor, Costs: ekyc.XenditCosts, OnCost}` dipasang sebagai `kyc.KYCProvider`.
Sebaiknya dijalankan **asinkron setelah commit** (catatan auth.md: `Submit` memegang transaksi selama `Verify`).

| Langkah | Biaya Xendit (PRD §15.2) |
|---|---|
| OCR KTP | Rp500 |
| Face match | Rp50 |
| Liveness | Rp2.000 |
| Autentikasi data Dukcapil | Rp4.000 |
| **Total lulus semua** | **Rp6.550** (± Rp12.000 dengan autentikasi foto biometrik) |

Verihubs, VIDA, Privy, ASLI RI: harga berdasarkan penawaran. Endpoint vendor eKYC tidak publik tanpa akun
merchant, jadi `XenditEKYC` dan `Verihubs` sekarang berperilaku seperti `Manual` (semua ke antrean admin). Saat
kontrak ada: implementasikan empat metode `Vendor` dari dokumentasi vendor, tes `httptest` + `kyctest.ProviderContract`.

## 8. Prasyarat bisnis dan hal yang masih terbuka

Prasyarat (ROADMAP §9 #12–13): PT (bukan perorangan, 3 pendiri), NIB lewat OSS, pendaftaran PSE lingkup privat,
akun Xendit + **KYB** perusahaan, aktivasi QRIS, aktivasi **xenPlatform** (kalau opsi B), kontrak eKYC vendor,
S&K yang menjelaskan alur dana dan sengketa (PRD §12.4).

Terbuka:

- Tabel rekening payout pengguna (bank/e-wallet + verifikasi nama pemilik) belum ada; `DestinationResolver`
  diisi inti. Migrasinya bisa masuk rentang `00201+`.
- Kebijakan pembatalan listing (potongan saat penyewa batal) belum dimodelkan di settlement.
- `resolveDispute` di inti perlu input terstruktur (case + nominal) untuk `Outcome`.
- Refund/payout `failed` belum punya endpoint admin "kirim ulang" (sekarang: ubah status ke `pending` lewat SQL).
- Biaya payout dan apakah MDR kembali saat refund belum diketahui (PRD §14.3), jadi belum dibukukan.
- Event realtime/notifikasi "pembayaran diterima" dipancarkan inti setelah `HandleWebhook` (paket ini tidak
  bergantung pada `notify`).

## 9. Tes

```bash
export TEST_DATABASE_URL=$(scripts/test-db.sh up)
scripts/go.sh test -count=1 -race ./internal/payments/ ./internal/ekyc/
```

Tes DB memakai schema sekali pakai dengan fixture `users` + `bookings` minimal (bukan migrasi inti) dan hanya
migrasi rentang 00200–00249 (diuji up → down → up). Cakupan: ledger seimbang (property test 5.000 siklus acak),
semua kasus §11.3, komisi persen/flat/min/maks/split + kecocokan dengan `domain.ComputeCommission`, webhook
dobel/terlambat/terbalik, token salah, kelebihan bayar, intent kedaluwarsa, 20 webhook paralel, 10 create intent
paralel, rekonsiliasi menemukan selisih status dan nominal, laporan komisi + "seandainya aktif", trigger
append-only/seimbang, adapter Xendit dengan `httptest`, kontrak `KYCProvider` untuk eKYC.

## 10. Sumber (dibaca 16 Nov 2026)

- Create payment request (v3): https://docs.xendit.co/apidocs/create-payment-request
- Get payment status: https://docs.xendit.co/apidocs/get-payment
- Payment webhook: https://docs.xendit.co/apidocs/payment-webhook-notification
- Handling webhooks (x-callback-token, retry 6×, urutan tidak dijamin): https://docs.xendit.co/docs/handling-webhooks
- Refund payment request: https://docs.xendit.co/docs/refund-payment-request
- Refund webhook: https://docs.xendit.co/apidocs/refund-webhook-notification
- Fitur refund QR (QRIS penuh saja, 30 hari): https://docs.xendit.co/qr-codes/payment-flows/refund/
- Split payments xenPlatform: https://docs.xendit.co/docs/split-payments
- Create split rule: https://docs.xendit.co/apidocs/create-split-rule
- Transfer antar akun: https://docs.xendit.co/docs/transfer-balances
- Migrasi ke stack pembayaran terbaru (`/qr_codes` lama → `/v3/payment_requests`): https://docs.xendit.co/docs/migrating-to-xendit-s-latest-payments-stack

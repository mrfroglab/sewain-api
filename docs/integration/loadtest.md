# Integrasi `loadtest` (F5.5)

Laporan lengkap: [`docs/loadtest/2026-11-report.md`](../loadtest/2026-11-report.md). Skrip: [`loadtest/`](../../loadtest/README.md).

## Perubahan kode

| Berkas | Perubahan |
|---|---|
| `internal/config` | Env `SNAPSHOT_USERS` (`all` default \| `referenced`), nilai lain = gagal start |
| `internal/service` | `Options.SnapshotUsers`. `buildSnapshot` memuat user **terakhir**; dengan `referenced` hanya id yang dirujuk snapshot (`referencedUsers`), `publicStats` difilter id yang sama. Admin/`full` tetap semua |
| `internal/db/queries/users.sql` | `ListUsersByIDs` (sqlc) |
| `internal/app` | Meneruskan `cfg.SnapshotUsers` |

## Keputusan

- **Default tetap `all`.** `referenced` mengubah isi kontrak `GET /api/v1/state` untuk web (`/u/[id]` untuk user yang
  tidak dirujuk). Default dan kebutuhan endpoint profil publik ditanyakan ke koordinator. Mengaktifkannya cukup
  lewat env, tanpa migrasi.
- **Tidak mengubah ETag/realtime.** ETag global (rasio 304 = 0%) dan `saveBooking` yang tidak menandai perubahan
  publik (`listingBusy`) dicatat sebagai temuan 7.2/7.3 di laporan untuk core-v2. Memperbaikinya mengubah semantik
  event realtime.
- **Ukuran pool tanpa kode baru:** pgx sudah membaca `pool_max_conns` dari `DATABASE_URL`.

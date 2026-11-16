package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// RunJobs runs the time-based transitions once (PRD §13 job berkala): EXPIRED after 24h (FR-R3), the overdue flag,
// auto-COMPLETED 48h after the collateral return, L1 → L2 after the 48h negotiation window, and the 7-day review
// release (FR-T2). A transaction-scoped advisory lock makes it safe with several API instances: when another
// instance is already running the jobs this returns immediately.
func (s *Service) RunJobs(ctx context.Context) (Change, error) {
	return s.run(ctx, func(t *txn) error {
		ok, err := db.TryXactLock(t.ctx, t.tx, db.LockJobs)
		if !t.check(err) || !ok {
			return nil
		}
		t.runJobs()
		return nil
	})
}

func (t *txn) runJobs() {
	now := t.now
	rows, err := t.q.ListBookingsDueForJobs(t.ctx, dbq.ListBookingsDueForJobsParams{Now: tsOf(now), Today: dateOf(domain.IsoDate(now)), NowMs: now})
	t.check(err)
	for _, r := range rows {
		b := bookingFromRow(r)
		switch {
		case b.Status == "REQUESTED" && now >= b.ExpiresAt:
			b.Status = "EXPIRED"
			t.saveBooking(b)
			t.notify(b.RenterID, "booking", "Request kadaluarsa", t.titleOf(b)+": penyedia tidak merespons dalam 24 jam. Coba barang lain di sekitarmu.", domain.BookingHref(b))
			t.notify(b.ProviderID, "booking", "Request kadaluarsa", "Request untuk "+t.titleOf(b)+" kadaluarsa karena tidak direspons 24 jam.", domain.BookingHref(b))
			t.sysMsg(b.ID, "Request kadaluarsa: tidak direspons dalam 24 jam.")
		case (b.Status == "ACTIVE" || b.Status == "DISPUTED") && !b.Overdue && b.ReturnedAt == nil && now > domain.EndOfDay(b.EndDate):
			b.Overdue = true
			t.saveBooking(b)
			t.notify(b.RenterID, "booking", "Kamu telat mengembalikan barang", t.titleOf(b)+" seharusnya kembali "+b.EndDate+". Denda "+domain.Rupiah(b.Agreement.LateFeePerDay)+"/hari berlaku.", domain.BookingHref(b))
			t.notify(b.ProviderID, "booking", "Booking telat dikembalikan", t.titleOf(b)+" sudah lewat tanggal kembali.", domain.BookingHref(b))
			t.sysMsg(b.ID, "Tanggal kembali sudah lewat. Booking ditandai TELAT. Denda per hari berlaku sesuai kesepakatan.")
		case b.Status == "RETURNED" && b.CollateralReturn != nil && b.CollateralReturn.AutoCompleteAt != nil && b.CollateralReturn.RenterConfirmedAt == nil && now >= *b.CollateralReturn.AutoCompleteAt:
			b.Status = "COMPLETED"
			b.CompletedAt = domain.Ptr(now)
			b.CompletedAuto = domain.Ptr(true)
			t.saveBooking(b)
			t.notify(b.RenterID, "booking", "Sewa selesai otomatis", t.titleOf(b)+": tidak ada konfirmasi jaminan dalam 48 jam, status otomatis selesai. Beri rating, yuk.", domain.BookingHref(b))
			t.notify(b.ProviderID, "booking", "Sewa selesai", t.titleOf(b)+" selesai otomatis setelah 48 jam.", domain.BookingHref(b))
			t.sysMsg(b.ID, "Penyewa tidak merespons dalam 48 jam. Status otomatis SELESAI.")
		}
	}
	disputes, err := t.q.ListDisputesDue(t.ctx, tsOf(now))
	t.check(err)
	for _, r := range disputes {
		d := disputeFromRow(r, nil, nil)
		d.Layer, d.Status = "L2", "mediasi"
		t.saveDispute(d)
		title := "Booking"
		if b := t.booking(d.BookingID); b != nil {
			title = t.titleOf(b)
			t.touch(b.RenterID, b.ProviderID)
			t.sysMsg(b.ID, "Batas negosiasi 48 jam habis. Sengketa otomatis naik ke mediasi admin (L2).")
		}
		t.notify(domain.AdminID, "sengketa", "Sengketa naik ke mediasi", title+": negosiasi 48 jam berakhir tanpa kesepakatan.", "/admin/sengketa/"+d.ID)
	}
	released, err := t.q.ReleaseDueReviews(t.ctx, tsOf(now))
	t.check(err)
	for _, r := range released {
		t.touch(r.FromID, r.ToID)
		t.touchPublic()
	}
}

// Housekeeping prunes idempotency keys older than 24 hours and old change-log rows. Not a state change.
func (s *Service) Housekeeping(ctx context.Context) error {
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.PruneIdempotencyKeys(ctx, s.opt.Now().Add(-24*time.Hour)); err != nil {
			return err
		}
		return q.PruneChangeLog(ctx, 10_000)
	})
}

// RunScheduler runs the jobs every interval until ctx ends (and housekeeping once an hour).
func (s *Service) RunScheduler(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	lastHousekeeping := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := s.RunJobs(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("jobs", "err", err)
			}
			if time.Since(lastHousekeeping) > time.Hour {
				lastHousekeeping = time.Now()
				if err := s.Housekeeping(ctx); err != nil && ctx.Err() == nil {
					s.log.Error("housekeeping", "err", err)
				}
			}
		}
	}
}

/* ------------------------------------------------------------- demo clock */

// MaxClockJump bounds advanceClock (30 days, like v1).
const MaxClockJump = 30 * domain.Day

// AdvanceClock moves the demo clock forward and runs the jobs at the new time, in one transaction.
func (s *Service) AdvanceClock(ctx context.Context, ms int64) (Change, error) {
	if ms <= 0 || ms > MaxClockJump {
		return Change{}, domain.Fail("Lompatan waktu tidak valid.")
	}
	return s.run(ctx, func(t *txn) error {
		c, err := getSetting[clockSetting](t.ctx, t.q, "clock")
		t.check(err)
		c.OffsetMs += ms
		t.check(putSetting(t.ctx, t.q, "clock", c))
		t.now += ms
		t.touchPublic()
		// Wait for a concurrently running scheduler instead of skipping: the jump must apply its transitions.
		_, err = t.tx.Exec(t.ctx, `SELECT pg_advisory_xact_lock($1)`, db.LockJobs)
		t.check(err)
		t.runJobs()
		return nil
	})
}

// ResetClock sets the demo clock back to real time.
func (s *Service) ResetClock(ctx context.Context) (Change, error) {
	return s.run(ctx, func(t *txn) error {
		t.check(putSetting(t.ctx, t.q, "clock", clockSetting{}))
		t.touchPublic()
		return nil
	})
}

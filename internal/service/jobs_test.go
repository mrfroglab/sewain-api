package service_test

import (
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// TestJobsWithFakeClock (F1.7): expiry, overdue, 48h auto-complete and L1 → L2, driven by the injected clock.
func TestJobsWithFakeClock(t *testing.T) {
	e := newEnv(t)
	// b-returned: provider marks the collateral as returned; the renter never confirms.
	e.must("u-dina", "returnCollateral", "u-dina", "b-returned")
	// A fresh L1 dispute on b-active.
	did := e.must("u-sinta", "openDispute", "u-sinta", "b-active", map[string]any{"caseType": "rusak-ringan", "description": "Resleting macet"}).(string)

	e.clock.Add(25 * time.Hour)
	ch, err := e.svc.RunJobs(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Version == 0 || !ch.Admins || len(ch.Users) == 0 {
		t.Fatalf("jobs must report a change: %+v", ch)
	}
	if b := e.booking("b-requested"); b.Status != "EXPIRED" {
		t.Fatalf("expiry: %s", b.Status)
	}
	if b := e.booking("b-returned"); b.Status != "RETURNED" {
		t.Fatalf("not yet auto-complete: %s", b.Status)
	}
	e.clock.Add(24 * time.Hour) // 49h after the collateral return
	if _, err := e.svc.RunJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	b := e.booking("b-returned")
	if b.Status != "COMPLETED" || b.CompletedAuto == nil || !*b.CompletedAuto {
		t.Fatalf("auto-complete: %+v", b.Status)
	}
	for _, d := range e.state().Disputes {
		if d.ID == did && (d.Layer != "L2" || d.Status != "mediasi") {
			t.Fatalf("L1 → L2 after 48h: %+v", d)
		}
	}
	if b := e.booking("b-active"); b.Overdue {
		t.Fatal("not overdue before the end date has passed")
	}
	e.clock.Add(3 * 24 * time.Hour) // past b-active's end date (seed: today + 2)
	if _, err := e.svc.RunJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if b := e.booking("b-active"); !b.Overdue {
		t.Fatal("disputed booking past its end date is overdue")
	}
	// Running again changes nothing.
	ch, err = e.svc.RunJobs(e.ctx)
	if err != nil || ch.Version != 0 {
		t.Fatalf("idle run must not bump the version: %+v %v", ch, err)
	}
}

// TestJobsAdvisoryLock: concurrent schedulers (several instances) do not process the same rows twice.
func TestJobsAdvisoryLock(t *testing.T) {
	e := newEnv(t)
	e.clock.Add(25 * time.Hour)
	other := service.New(e.svc.Pool(), service.Options{DemoMode: true, Now: e.clock.Now})
	done := make(chan error, 2)
	go func() { _, err := e.svc.RunJobs(e.ctx); done <- err }()
	go func() { _, err := other.RunJobs(e.ctx); done <- err }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for _, n := range e.state().Notifications {
		if n.Title == "Request kadaluarsa" && n.UserID == "u-raka" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expiry notified %d times", count)
	}
}

func TestDemoClockAndReset(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.AdvanceClock(e.ctx, 0); err == nil {
		t.Fatal("zero jump must be rejected")
	}
	if _, err := e.svc.AdvanceClock(e.ctx, 31*domain.Day); err == nil {
		t.Fatal("jump > 30 days must be rejected")
	}
	if _, err := e.svc.AdvanceClock(e.ctx, 2*domain.Hour); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); st.ClockOffset != 2*domain.Hour {
		t.Fatalf("offset %d", st.ClockOffset)
	}
	if _, err := e.svc.ResetClock(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.must("u-raka", "toggleFavorite", "u-raka", "l-headlamp")
	v1 := e.state().Version
	ch, err := e.svc.ResetDemo(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := e.state()
	if ch.Version <= v1 || st.ClockOffset != 0 || len(st.Favorites["u-raka"]) != 2 || len(st.Bookings) != 20 || st.BookingCounter != 60 {
		t.Fatalf("reset: version %d→%d offset %d favorites %v bookings %d counter %d", v1, ch.Version, st.ClockOffset, st.Favorites["u-raka"], len(st.Bookings), st.BookingCounter)
	}
}

package notify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func insertJob(t *testing.T, db DB, id, userID string, ch Channel, due time.Time) {
	t.Helper()
	_, err := db.Exec(context.Background(), `INSERT INTO notification_outbox (user_id, notification_id, channel, payload, next_attempt_at)
		VALUES ($1, $2, $3, '{"title":"Halo","body":"Tes","kind":"sistem","category":"sistem"}', $4)`, userID, id, string(ch), due)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRetriesThenSucceeds(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-a", "")
	clk := &clock{t: at(12, 0)}
	insertJob(t, db, "n-1", "u-a", ChannelEmail, clk.Now())

	var calls atomic.Int32
	failUntil := int32(2) // provider down for two attempts
	d := &Dispatcher{DB: db, Now: clk.Now, Senders: map[Channel]Sender{ChannelEmail: SenderFunc(func(ctx context.Context, j Job) error {
		if n := calls.Add(1); n <= failUntil {
			return errors.New("smtp: 451 coba lagi nanti")
		}
		if j.Payload.Title != "Halo" {
			return Permanent(fmt.Errorf("payload %+v", j.Payload))
		}
		return nil
	})}}
	step := func(wantClaimed int, wantStatus string, wantDue time.Time) {
		t.Helper()
		n, err := d.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != wantClaimed {
			t.Fatalf("claimed %d, want %d", n, wantClaimed)
		}
		r := outboxRows(t, db, "n-1")[ChannelEmail]
		if r.Status != wantStatus || (!wantDue.IsZero() && !r.Due.Equal(wantDue)) {
			t.Fatalf("row = %+v, want %s due %v", r, wantStatus, wantDue)
		}
	}
	step(1, "failed", at(12, 0).Add(30*time.Second))
	clk.Add(10 * time.Second)
	step(0, "failed", at(12, 0).Add(30*time.Second)) // not due yet
	clk.Set(at(12, 0).Add(30 * time.Second))
	step(1, "failed", at(12, 0).Add(30*time.Second+2*time.Minute))
	clk.Add(2 * time.Minute)
	step(1, "sent", time.Time{})
	r := outboxRows(t, db, "n-1")[ChannelEmail]
	if r.Attempts != 3 || r.LastErr != nil {
		t.Fatalf("final row = %+v", r)
	}
	clk.Add(24 * time.Hour)
	step(0, "sent", time.Time{}) // never resent
}

func TestDispatcherGivesUpAfterBackoffSchedule(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-a", "")
	clk := &clock{t: at(12, 0)}
	insertJob(t, db, "n-1", "u-a", ChannelWebPush, clk.Now())
	d := &Dispatcher{DB: db, Now: clk.Now, Senders: map[Channel]Sender{ChannelWebPush: SenderFunc(func(context.Context, Job) error {
		return errors.New("push service 503")
	})}}
	wantWaits := []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}
	for i, wait := range wantWaits {
		before := clk.Now()
		if n, err := d.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("attempt %d: n=%d err=%v", i+1, n, err)
		}
		r := outboxRows(t, db, "n-1")[ChannelWebPush]
		if r.Status != "failed" || !r.Due.Equal(before.Add(wait)) || r.Attempts != i+1 {
			t.Fatalf("attempt %d: %+v, want retry after %v", i+1, r, wait)
		}
		clk.Set(r.Due)
	}
	if n, _ := d.RunOnce(ctx); n != 1 {
		t.Fatal("6th attempt not claimed")
	}
	r := outboxRows(t, db, "n-1")[ChannelWebPush]
	if r.Status != "dead" || r.Attempts != 6 || r.LastErr == nil || *r.LastErr != "push service 503" {
		t.Fatalf("after last attempt: %+v", r)
	}
	clk.Add(48 * time.Hour)
	if n, _ := d.RunOnce(ctx); n != 0 {
		t.Fatal("dead job claimed again")
	}
}

func TestDispatcherPermanentAndUnknownChannel(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-a", "")
	clk := &clock{t: at(12, 0)}
	insertJob(t, db, "n-perm", "u-a", ChannelEmail, clk.Now())
	insertJob(t, db, "n-wa", "u-a", ChannelWhatsApp, clk.Now())
	d := &Dispatcher{DB: db, Now: clk.Now, Senders: map[Channel]Sender{
		ChannelEmail:    SenderFunc(func(context.Context, Job) error { return Permanent(errors.New("550 mailbox unavailable")) }),
		ChannelWhatsApp: WhatsAppStub{},
	}}
	if n, err := d.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, id := range []string{"n-perm", "n-wa"} {
		for _, r := range outboxRows(t, db, id) {
			if r.Status != "dead" || r.Attempts != 1 {
				t.Errorf("%s: %+v, want dead after one attempt", id, r)
			}
		}
	}
}

func TestParallelDispatchersNeverSendTwice(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-a", "")
	const jobs = 120
	now := time.Now()
	for i := range jobs {
		insertJob(t, db, fmt.Sprintf("n-%03d", i), "u-a", ChannelEmail, now.Add(-time.Minute))
	}
	var mu sync.Mutex
	sent := map[string]int{}
	sender := SenderFunc(func(_ context.Context, j Job) error {
		time.Sleep(2 * time.Millisecond) // widen the race window
		mu.Lock()
		sent[j.NotificationID]++
		mu.Unlock()
		return nil
	})
	var wg sync.WaitGroup
	for range 3 {
		d := &Dispatcher{DB: db, BatchSize: 7, Concurrency: 3, Senders: map[Channel]Sender{ChannelEmail: sender}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := d.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(sent) != jobs {
		t.Fatalf("sent %d distinct jobs, want %d", len(sent), jobs)
	}
	for id, n := range sent {
		if n != 1 {
			t.Errorf("%s sent %d times", id, n)
		}
	}
	var pending int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE status <> 'sent'`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d rows not sent", pending)
	}
}

func TestDispatcherRunStopsWithContext(t *testing.T) {
	db := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	addUser(t, db, "u-a", "")
	insertJob(t, db, "n-1", "u-a", ChannelEmail, time.Now().Add(-time.Second))
	done := make(chan struct{})
	d := &Dispatcher{DB: db, Interval: 20 * time.Millisecond, Senders: map[Channel]Sender{ChannelEmail: SenderFunc(func(context.Context, Job) error {
		close(done)
		return nil
	})}}
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not send")
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

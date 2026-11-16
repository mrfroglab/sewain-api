package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Job is one claimed outbox row.
type Job struct {
	ID             string
	UserID         string
	NotificationID string
	Channel        Channel
	Payload        Payload
	Attempts       int // including the current attempt
}

// Sender delivers jobs of one channel. Return Permanent(err) when retrying cannot help.
type Sender interface {
	Send(ctx context.Context, job Job) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, job Job) error

func (f SenderFunc) Send(ctx context.Context, job Job) error { return f(ctx, job) }

// DefaultBackoff is the wait after the 1st, 2nd, ... failed attempt. After the last one fails the job is dead.
var DefaultBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

// Dispatcher sends due outbox rows. Several dispatchers (goroutines or instances) may share a database: a row is
// claimed with FOR UPDATE SKIP LOCKED and leased, so no two dispatchers send it at the same time.
// Delivery is at-least-once: a crash after a provider accepted a message but before the row is marked sent
// resends it once the lease expires.
type Dispatcher struct {
	DB      DB
	Senders map[Channel]Sender
	// Backoff defaults to DefaultBackoff; max attempts = len(Backoff) + 1.
	Backoff []time.Duration
	// BatchSize rows claimed per round (default 20), sent with Concurrency workers (default 4).
	BatchSize   int
	Concurrency int
	// SendTimeout bounds one send (default 30s). Lease is how long a claimed row stays invisible to other
	// dispatchers (default 5m); it must exceed the time a batch can take.
	SendTimeout time.Duration
	Lease       time.Duration
	// Interval between polls when the queue is empty (default 2s).
	Interval time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func orDefault[T int | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// Run polls until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) error {
	interval := orDefault(d.Interval, 2*time.Second)
	for {
		n, err := d.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			d.log().Error("notify: dispatch", "err", err)
		}
		if n > 0 && err == nil {
			continue // more may be due
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// RunOnce claims one batch of due rows, sends them and records the outcome. It returns the number claimed.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	jobs, err := d.claim(ctx)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	sem := make(chan struct{}, orDefault(d.Concurrency, 4))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := d.process(ctx, j); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return len(jobs), firstErr
}

func (d *Dispatcher) claim(ctx context.Context) ([]Job, error) {
	now := d.now()
	rows, err := d.DB.Query(ctx, `
		UPDATE notification_outbox o
		SET attempts = o.attempts + 1, next_attempt_at = $2, updated_at = $1
		FROM (
			SELECT id FROM notification_outbox
			WHERE status IN ('pending', 'failed') AND next_attempt_at <= $1
			ORDER BY next_attempt_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		) due
		WHERE o.id = due.id
		RETURNING o.id, o.user_id, o.notification_id, o.channel, o.payload, o.attempts`,
		now, now.Add(orDefault(d.Lease, 5*time.Minute)), orDefault(d.BatchSize, 20))
	if err != nil {
		return nil, fmt.Errorf("notify: claim: %w", err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		var ch string
		var payload []byte
		if err := rows.Scan(&j.ID, &j.UserID, &j.NotificationID, &ch, &payload, &j.Attempts); err != nil {
			return nil, err
		}
		j.Channel = Channel(ch)
		if err := json.Unmarshal(payload, &j.Payload); err != nil {
			return nil, fmt.Errorf("notify: payload of %s: %w", j.ID, err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

var errNoSender = errors.New("channel not configured")

func (d *Dispatcher) process(ctx context.Context, j Job) error {
	var sendErr error
	if s := d.Senders[j.Channel]; s == nil {
		sendErr = Permanent(errNoSender)
	} else {
		sctx, cancel := context.WithTimeout(ctx, orDefault(d.SendTimeout, 30*time.Second))
		sendErr = s.Send(sctx, j)
		cancel()
	}
	// Record the outcome even when shutdown cancelled ctx mid-send, so a delivered message is not resent.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := d.now()
	if sendErr == nil {
		_, err := d.DB.Exec(ctx, `UPDATE notification_outbox
			SET status = 'sent', sent_at = $2, updated_at = $2, last_error = NULL WHERE id = $1`, j.ID, now)
		return err
	}
	backoff := d.Backoff
	if len(backoff) == 0 {
		backoff = DefaultBackoff
	}
	msg := sendErr.Error()
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	if IsPermanent(sendErr) || j.Attempts > len(backoff) {
		d.log().Warn("notify: job dead", "id", j.ID, "channel", j.Channel, "attempts", j.Attempts, "err", msg)
		_, err := d.DB.Exec(ctx, `UPDATE notification_outbox
			SET status = 'dead', last_error = $2, updated_at = $3 WHERE id = $1`, j.ID, msg, now)
		return err
	}
	next := now.Add(backoff[j.Attempts-1])
	d.log().Info("notify: job failed, retrying", "id", j.ID, "channel", j.Channel, "attempts", j.Attempts, "next", next, "err", msg)
	_, err := d.DB.Exec(ctx, `UPDATE notification_outbox
		SET status = 'failed', last_error = $2, next_attempt_at = $3, updated_at = $4 WHERE id = $1`, j.ID, msg, next, now)
	return err
}

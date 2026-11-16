// Package service runs every Sewain action as one Postgres transaction: rules from internal/domain, rows locked with
// SELECT … FOR UPDATE, side effects (in-app notifications, system chat messages, audit log) written in the same
// transaction, and one state version per committed change. It replaces v1's in-memory engine.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Payment modes (PAYMENT_MODE, ROADMAP D12).
const (
	PaymentSimulated = "simulated" // demo: "lunas" via a simulated QRIS/VA/e-wallet (default with DEMO_MODE)
	PaymentOffline   = "offline"   // pilot: the renter pays the provider directly and marks it ("langsung")
	PaymentGateway   = "gateway"   // reserved for Fase 8 (Xendit)
)

// Snapshot scopes (SNAPSHOT_SCOPE).
const (
	ScopeScoped = "scoped" // per viewer (ROADMAP §4), the default and the only scope allowed in production
	ScopeFull   = "full"   // everything, like v1 (demo fallback only)
)

type Options struct {
	DemoMode      bool
	SnapshotScope string
	// SnapshotUsers: SnapshotUsersReferenced (default, empty) or SnapshotUsersAll (config.Config.SnapshotUsers). With
	// referenced, DEMO_MODE snapshots also carry every demo persona.
	SnapshotUsers string
	// PaymentMode: simulated | offline | gateway. Empty = simulated with DEMO_MODE, offline otherwise.
	PaymentMode string
	// Now is the real clock (the demo clock adds the stored offset). Tests inject a fake one.
	Now func() time.Time
	Log *slog.Logger
}

// Viewer is who makes a request. nil = guest. Auth modules resolve a request to a Viewer.
type Viewer struct {
	UserID string
	Role   string // user | admin
}

func (v *Viewer) IsAdmin() bool { return v != nil && v.Role == "admin" }

// Change describes one committed transaction for realtime fan-out.
//   - Version: the new state version (snapshot "version").
//   - Users: users whose private data changed (booking parties, notification recipients, the actor).
//   - Admins: admins should refresh (always true: admins see everything).
//   - Public: public data changed (listings, public profiles, released reviews, configuration, wanted board).
type Change struct {
	Version int64    `json:"version"`
	Users   []string `json:"users"`
	Admins  bool     `json:"admins"`
	Public  bool     `json:"public"`
}

type Service struct {
	pool *pgxpool.Pool
	q    *dbq.Queries
	opt  Options
	log  *slog.Logger

	mu          sync.RWMutex
	listeners   []func(Change)
	txListeners []func(context.Context, pgx.Tx, Change) error
	notifiers   []func(context.Context, pgx.Tx, *domain.Notification) error
	kycDecider  KYCDecider
}

func New(pool *pgxpool.Pool, opt Options) *Service {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.SnapshotScope == "" {
		opt.SnapshotScope = ScopeScoped
	}
	if opt.PaymentMode == "" {
		opt.PaymentMode = PaymentOffline
		if opt.DemoMode {
			opt.PaymentMode = PaymentSimulated
		}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Service{pool: pool, q: dbq.New(pool), opt: opt, log: opt.Log}
}

func (s *Service) Pool() *pgxpool.Pool { return s.pool }
func (s *Service) DemoMode() bool      { return s.opt.DemoMode }

// PaymentMode is the effective PAYMENT_MODE.
func (s *Service) PaymentMode() string { return s.opt.PaymentMode }

// OnChange registers a listener called after every committed change (realtime hub, outbox dispatcher, …).
func (s *Service) OnChange(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

// OnTxChange registers a hook for changes recorded inside a transaction owned by another module (auth, kyc); it
// runs in that transaction before it commits (see PublishTx).
func (s *Service) OnTxChange(fn func(ctx context.Context, tx pgx.Tx, ch Change) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txListeners = append(s.txListeners, fn)
}

// OnNotification registers a hook that runs inside the transaction that creates an in-app notification
// (e.g. the notify module enqueues outbox rows for push/email). Returning an error aborts the action.
func (s *Service) OnNotification(fn func(ctx context.Context, tx pgx.Tx, n *domain.Notification) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifiers = append(s.notifiers, fn)
}

func (s *Service) emit(ch Change) {
	s.mu.RLock()
	ls := append([]func(Change){}, s.listeners...)
	s.mu.RUnlock()
	for _, fn := range ls {
		fn(ch)
	}
}

// clockSetting is app_settings["clock"]: the demo clock offset (PRD §13 DEMO_MODE "timer dipercepat").
type clockSetting struct {
	OffsetMs int64 `json:"offsetMs"`
}

// seedSetting is app_settings["seed"].
type seedSetting struct {
	Seeded   bool  `json:"seeded"`
	SeedBase int64 `json:"seedBase"`
}

func (s *Service) realNow() int64 { return s.opt.Now().UnixMilli() }

func getSetting[T any](ctx context.Context, q *dbq.Queries, key string) (T, error) {
	var v T
	raw, err := q.GetSetting(ctx, key)
	if db.IsNotFound(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(raw, &v)
}

func putSetting(ctx context.Context, q *dbq.Queries, key string, v any) error {
	raw, _ := json.Marshal(v)
	return q.PutSetting(ctx, dbq.PutSettingParams{Key: key, Value: raw})
}

// Now is the demo clock: real time + stored offset (ms).
func (s *Service) Now(ctx context.Context) (int64, error) {
	c, err := getSetting[clockSetting](ctx, s.q, "clock")
	return s.realNow() + c.OffsetMs, err
}

// Version is the current state version.
func (s *Service) Version(ctx context.Context) (int64, error) { return s.q.CurrentVersion(ctx) }

// errAbort carries a panic out of a transaction body.
type errAbort struct{ v any }

func (e errAbort) Error() string { return fmt.Sprintf("panic in transaction: %v", e.v) }

// run executes fn in one transaction (retried on serialization failure/deadlock). A domain.UserError rolls back.
// When fn changed something, a change_log row assigns the new version and listeners are told after commit.
func (s *Service) run(ctx context.Context, fn func(t *txn) error) (Change, error) {
	var ch Change
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) (err error) {
		t := newTxn(ctx, s, tx)
		defer func() {
			if r := recover(); r != nil {
				if t.err != nil { // a nil from a failed lookup after the transaction already broke
					err = t.err
					return
				}
				err = errAbort{r}
			}
		}()
		if err := t.loadClock(); err != nil {
			return err
		}
		ferr := fn(t)
		if t.err != nil {
			return t.err
		}
		if ferr != nil {
			return ferr
		}
		ch, err = t.finish()
		return err
	})
	if err != nil {
		return Change{}, err
	}
	if ch.Version > 0 {
		s.emit(ch)
	}
	return ch, nil
}

// IsUserError reports a rule violation (message for the user).
func IsUserError(err error) (string, bool) {
	var ue domain.UserError
	if errors.As(err, &ue) {
		return ue.Error(), true
	}
	return "", false
}

// Package app wires the Sewain modules into one process: core service, auth + KYC, media, realtime fan-out,
// notification outbox, platform middleware, and the background runners (time jobs, retention, outbox dispatcher,
// LISTEN/NOTIFY listener). cmd/api and the end-to-end tests build the API through it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/crypto"
	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/httpapi"
	"github.com/mrfroglab/sewain-api/internal/kyc"
	"github.com/mrfroglab/sewain-api/internal/media"
	"github.com/mrfroglab/sewain-api/internal/notify"
	"github.com/mrfroglab/sewain-api/internal/platform/health"
	"github.com/mrfroglab/sewain-api/internal/platform/metrics"
	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
	"github.com/mrfroglab/sewain-api/internal/platform/ratelimit"
	"github.com/mrfroglab/sewain-api/internal/realtime"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// Options override parts of the wiring (tests).
type Options struct {
	// Storage replaces media.FromEnv (MEDIA_BACKEND); RetentionStorage replaces media.RetentionStorageFromEnv.
	Storage, RetentionStorage media.Storage
	// Now is the real clock (the demo clock adds its offset).
	Now func() time.Time
	// RateProfiles replaces the per-route rate limits from the configuration (Config.Rates); unset profiles keep
	// the configured rate.
	RateProfiles httpapi.RateProfiles
	// PushHosts overrides the allowed push services (tests: "*").
	PushHosts []string
	// Metrics, when nil, a fresh registry is created.
	Metrics *metrics.Metrics
	// Signer replaces the MEDIA_SIGNING_KEY signer.
	Signer *media.Signer
	// OnListen runs whenever the LISTEN connection is (re)established.
	OnListen func()
}

// App is a wired API process.
type App struct {
	Cfg     config.Config
	Pool    *pgxpool.Pool
	Log     *slog.Logger
	Svc     *service.Service
	Auth    *auth.Service
	KYC     *kyc.Service
	Storage media.Storage
	Signer  *media.Signer
	Hub     *realtime.Hub
	Metrics *metrics.Metrics
	Limiter *ratelimit.Memory

	retentionStorage media.Storage
	publisher        *realtime.Publisher
	listener         *realtime.Listener
	dispatcher       *notify.Dispatcher
	handler          http.Handler
	now              func() time.Time
}

// New builds the process around an open, migrated pool.
func New(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, opt Options) (*App, error) {
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	a := &App{Cfg: cfg, Pool: pool, Log: log, now: now, Metrics: opt.Metrics}
	if a.Metrics == nil {
		a.Metrics = metrics.New()
		if err := a.Metrics.RegisterPool(pool); err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
	}
	a.Svc = service.New(pool, service.Options{DemoMode: cfg.DemoMode, SnapshotScope: cfg.SnapshotScope, PaymentMode: cfg.PaymentMode,
		Now: now, Log: log})

	// Media: storage (write-once), signer (MEDIA_SIGNING_KEY, random in development/demo when unset).
	var err error
	if a.Storage = opt.Storage; a.Storage == nil {
		if a.Storage, err = media.FromEnv(); err != nil {
			return nil, fmt.Errorf("media storage: %w", err)
		}
	}
	if a.retentionStorage = opt.RetentionStorage; a.retentionStorage == nil {
		if opt.Storage != nil || cfg.MediaBackend == "local" {
			a.retentionStorage = a.Storage
		} else if a.retentionStorage, err = media.RetentionStorageFromEnv(); err != nil {
			return nil, fmt.Errorf("media retention storage: %w", err)
		}
	}
	if opt.Signer != nil {
		a.Signer = opt.Signer
	} else if cfg.MediaSigningKey {
		a.Signer, err = media.SignerFromEnv()
	} else if cfg.EphemeralKeysOK() {
		log.Warn("MEDIA_SIGNING_KEY not set: using a random key, signed photo URLs break on restart")
		var key []byte
		if key, err = crypto.RandomBytes(32); err == nil {
			a.Signer, err = media.NewSigner(key)
		}
	} else {
		err = errors.New("MEDIA_SIGNING_KEY is required")
	}
	if err != nil {
		return nil, fmt.Errorf("media signer: %w", err)
	}
	a.Signer.SetClock(now)

	// Auth + KYC on top of the core user repository.
	sender, err := auth.NewSender(cfg.Auth)
	if err != nil {
		return nil, err
	}
	if a.Auth, err = auth.NewService(pool, userDirectory{svc: a.Svc}, sender, cfg.Auth); err != nil {
		return nil, err
	}
	if a.KYC, err = kyc.NewService(pool, a.Storage, auditor{pool: pool}, cfg.KYC); err != nil {
		return nil, err
	}
	a.Auth.Now, a.KYC.Now = now, now
	a.Auth.Log = log
	a.Auth.OnDailyCapReached = func(ctx context.Context, day string, limit int) error {
		return a.Svc.NotifyAdmins(ctx, "keamanan", "Batas harian OTP tercapai",
			fmt.Sprintf("Sudah %d kode OTP terkirim hari ini (%s). Permintaan kode baru ditolak sampai 00.00 WIB. "+
				"Cek kemungkinan SMS pumping, atau naikkan OTP_MAX_SENDS_PER_DAY kalau lalu lintasnya wajar.", limit, day), "")
	}
	a.KYC.Hooks = kyc.Hooks{
		OnSubmitted: func(ctx context.Context, tx pgx.Tx, s kyc.Submission) error {
			return asAuthError(a.Svc.KYCSubmitted(ctx, tx, kycEvent(s)))
		},
		OnDecided: func(ctx context.Context, tx pgx.Tx, s kyc.Submission) error {
			return asAuthError(a.Svc.KYCDecided(ctx, tx, kycEvent(s)))
		},
	}
	a.Svc.SetKYCDecider(func(ctx context.Context, adminID, submissionID string, approve bool, reason string) error {
		decision := "reject"
		if approve {
			decision = "approve"
		}
		_, err := a.KYC.Decide(ctx, auth.Viewer{UserID: adminID, Role: auth.RoleAdmin}, submissionID, decision, reason)
		var ae *auth.Error
		if errors.As(err, &ae) && ae.Status < 500 {
			return domain.Fail(ae.Msg)
		}
		return err
	})

	// Realtime: per-user hub, fan-out through LISTEN/NOTIFY.
	instance := realtime.NewInstanceID()
	a.Hub = realtime.NewHub(realtime.Options{
		Resolve: func(r *http.Request) (string, string) {
			v := auth.ViewerFrom(r.Context())
			return v.UserID, v.Role
		},
		AllowedOrigins: cfg.CORSOrigins,
		CurrentVersion: a.Svc.Version,
		Logger:         log,
	})
	a.publisher = &realtime.Publisher{DB: pool, Hub: a.Hub, Origin: instance}
	a.listener = &realtime.Listener{ConnString: cfg.DatabaseURL, Hub: a.Hub, Origin: instance, Logger: log, OnListen: opt.OnListen}
	a.Svc.OnChange(func(ch service.Change) {
		// Called after commit on the request goroutine: local hub right away, other instances via NOTIFY.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.publisher.Publish(ctx, event(ch)); err != nil {
			log.Warn("realtime publish", "err", err)
		}
	})
	a.Svc.OnTxChange(func(ctx context.Context, tx pgx.Tx, ch service.Change) error {
		return a.publisher.PublishTx(ctx, tx, event(ch))
	})

	// Notifications: every in-app notification is queued for push/email in the same transaction.
	enqueuer := &notify.Enqueuer{Channels: cfg.Notify.Channels(), PublicURL: cfg.Notify.PublicURL}
	a.Svc.OnNotification(func(ctx context.Context, tx pgx.Tx, n *domain.Notification) error {
		_, err := enqueuer.Enqueue(ctx, tx, notify.Notification{ID: n.ID, UserID: n.UserID, Kind: n.Kind, Title: n.Title, Body: n.Body,
			Href: domain.Deref(n.Href, "")})
		return err
	})
	a.dispatcher = &notify.Dispatcher{DB: pool, Senders: cfg.Notify.Senders(pool, log), Logger: log}

	// HTTP.
	a.Limiter = ratelimit.NewMemory(ratelimit.MemoryOptions{})
	ipRes, err := middleware.NewIPResolver(cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
	}
	notifyAPI := &notify.API{DB: pool, Viewer: func(r *http.Request) string { return auth.ViewerFrom(r.Context()).UserID },
		VAPIDPublicKey: cfg.Notify.VAPID.PublicKey, Available: cfg.Notify.Channels(), PushHosts: opt.PushHosts}
	if !cfg.Notify.VAPID.Enabled() {
		notifyAPI.VAPIDPublicKey = ""
	}
	srv := httpapi.New(cfg, a.Svc, log, httpapi.Deps{
		Auth: a.Auth, KYC: a.KYC, Storage: a.Storage, Signer: a.Signer, Hub: a.Hub, Notify: notifyAPI, Limiter: a.Limiter,
		Metrics: a.Metrics, Profiles: rateProfiles(cfg, opt.RateProfiles), IPResolver: ipRes,
		Ready: health.Readiness{Ping: pool.Ping, MigrationVersion: func(ctx context.Context) (int64, error) { return db.SchemaVersion(ctx, pool) },
			MinMigration: db.LatestMigration()},
	})
	a.handler = srv.Routes()
	return a, nil
}

// rateProfiles fills the profiles not overridden by the caller from the configured rates.
func rateProfiles(cfg config.Config, override httpapi.RateProfiles) httpapi.RateProfiles {
	p := httpapi.ProfilesFromRates(cfg.Rates)
	for _, o := range []struct{ dst, src *ratelimit.Profile }{
		{&p.Public, &override.Public}, {&p.Write, &override.Write}, {&p.Upload, &override.Upload},
		{&p.Auth, &override.Auth}, {&p.Demo, &override.Demo},
	} {
		if o.src.Name != "" {
			*o.dst = *o.src
		}
	}
	return p
}

// Handler is the whole HTTP API.
func (a *App) Handler() http.Handler { return a.handler }

// Run starts the background runners and blocks until ctx ends: time-based jobs (15 s), retention (RETENTION_INTERVAL),
// outbox dispatcher, LISTEN/NOTIFY listener and the rate-limit janitor. Each runner is safe with several instances.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
			a.Log.Info("runner stopped", "runner", name)
		}()
	}
	a.Limiter.StartJanitor(ctx, time.Minute)
	start("jobs", func() { a.Svc.RunScheduler(ctx, 15*time.Second) })
	start("listener", func() { _ = a.listener.Run(ctx) })
	start("dispatcher", func() { _ = a.dispatcher.Run(ctx) })
	start("retention", func() {
		tick := time.NewTicker(a.Cfg.RetentionEvery)
		defer tick.Stop()
		for {
			if err := a.Metrics.TimeJob("retention", func() error { return a.RunRetention(ctx) }); err != nil && ctx.Err() == nil {
				a.Log.Error("retention", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	})
	start("outbox-gauge", func() { a.outboxGauge(ctx) })
	wg.Wait()
}

// Listen runs only the LISTEN/NOTIFY listener until ctx ends (tests; Run includes it).
func (a *App) Listen(ctx context.Context) error { return a.listener.Run(ctx) }

// DispatchOnce sends one batch of due outbox rows (tests; Run includes the dispatcher).
func (a *App) DispatchOnce(ctx context.Context) (int, error) { return a.dispatcher.RunOnce(ctx) }

// RunRetention runs every retention task once, on one instance at a time (session advisory lock): evidence photos
// 90 days after the booking finished (F3.6), KYC files 30 days after the decision, identity data of deleted accounts,
// expired sessions and OTP challenges, idempotency keys older than 24 hours (and the change log), read
// notifications older than 180 days and delivered/dead outbox rows older than 30 days. It also alerts the admins
// about KYC submissions waiting longer than 48 hours.
func (a *App) RunRetention(ctx context.Context) error {
	_, err := db.WithSessionLock(ctx, a.Pool, db.LockRetention, func(ctx context.Context) error {
		var errs []error
		step := func(name string, fn func() (int64, error)) {
			n, err := fn()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
			if n > 0 {
				a.Log.Info("retention", "task", name, "removed", n)
			}
		}
		demoNow, err := a.Svc.DemoNow(ctx)
		if err != nil {
			return err
		}
		realNow := a.now()
		step("evidence-photos", func() (int64, error) {
			n, err := a.Svc.PurgeEvidencePhotos(ctx, a.retentionStorage, demoNow)
			return int64(n), err
		})
		step("kyc-files", func() (int64, error) {
			n, err := a.KYC.PurgeFiles(ctx, realNow)
			return int64(n), err
		})
		step("kyc-deleted-accounts", func() (int64, error) {
			ids, err := a.Svc.DeletedAccountsWithKYC(ctx)
			var n int64
			for _, id := range ids {
				k, eerr := a.KYC.EraseUser(ctx, id)
				n += int64(k)
				err = errors.Join(err, eerr)
			}
			return n, err
		})
		step("kyc-stale-alert", func() (int64, error) { return a.Svc.AlertStaleKYC(ctx, realNow) })
		step("outbox", func() (int64, error) { return a.Svc.PurgeOutbox(ctx, realNow) })
		step("sessions", func() (int64, error) { return a.Auth.PurgeExpiredSessions(ctx, realNow.Add(-7*24*time.Hour)) })
		step("otp", func() (int64, error) { return a.Auth.PurgeOTPChallenges(ctx, realNow.Add(-24*time.Hour)) })
		step("idempotency", func() (int64, error) { return 0, a.Svc.Housekeeping(ctx) })
		step("notifications", func() (int64, error) { return a.Svc.PurgeReadNotifications(ctx, demoNow) })
		return errors.Join(errs...)
	})
	return err
}

func (a *App) outboxGauge(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		rows, err := a.Pool.Query(ctx, `SELECT channel, count(*) FROM notification_outbox WHERE status IN ('pending', 'failed') GROUP BY channel`)
		if err == nil {
			seen := map[string]bool{}
			for rows.Next() {
				var ch string
				var n int
				if rows.Scan(&ch, &n) == nil {
					a.Metrics.SetOutboxPending(ch, n)
					seen[ch] = true
				}
			}
			rows.Close()
			for _, ch := range notify.QueuedChannels {
				if !seen[string(ch)] {
					a.Metrics.SetOutboxPending(string(ch), 0)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Close releases what New opened that the pool does not own (the hub's connections).
func (a *App) Close() { a.Hub.Close() }

func event(ch service.Change) realtime.Event {
	return realtime.Event{Version: ch.Version, Users: ch.Users, Admins: ch.Admins, Public: ch.Public}
}

func kycEvent(s kyc.Submission) service.KYCEvent {
	return service.KYCEvent{SubmissionID: s.ID, UserID: s.UserID, NIKMasked: s.NIKMasked, Status: s.Status, Reason: s.Reason,
		DecidedAt: s.DecidedAt, DecidedBy: s.DecidedBy}
}

// asAuthError turns a core rule violation inside a KYC hook into a 409 the KYC handlers can show.
func asAuthError(err error) error {
	if msg, ok := service.IsUserError(err); ok {
		return &auth.Error{Status: http.StatusConflict, Msg: msg}
	}
	return err
}

// userDirectory implements auth.UserDirectory on the core users table. Phones arrive as E.164 (+62…); the core
// stores them like v1 (digits, leading 0), service.NormalizePhone converts.
type userDirectory struct{ svc *service.Service }

// NewUserName is the placeholder name of an account created by OTP; the web asks for the real name next.
const NewUserName = "Pengguna Baru"

func (d userDirectory) FindUserByPhone(ctx context.Context, phoneE164 string) (string, bool, error) {
	u, err := d.svc.FindUserByPhone(ctx, phoneE164)
	if err != nil || u == nil {
		return "", false, err
	}
	return u.ID, true, nil
}

func (d userDirectory) CreateUser(ctx context.Context, tx pgx.Tx, phoneE164 string) (string, error) {
	u, ch, err := d.svc.CreateUser(ctx, tx, phoneE164, NewUserName)
	if err != nil {
		return "", asAuthError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET phone_verified_at = now() WHERE id = $1`, u.ID); err != nil {
		return "", err
	}
	return u.ID, d.svc.PublishTx(ctx, tx, ch)
}

// GetUserAuth: unknown or deleted accounts are reported as blacklisted so their sessions resolve to a guest.
func (d userDirectory) GetUserAuth(ctx context.Context, userID string) (string, string, error) {
	u, err := d.svc.GetUserAuth(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if u == nil {
		return auth.RoleUser, auth.StatusBlacklist, nil
	}
	return u.Role, u.Status, nil
}

// auditor writes KYC audit entries to the core audit log (append-only), with the request id when there is one.
type auditor struct{ pool *pgxpool.Pool }

func (a auditor) Audit(ctx context.Context, actorID, action, target string, oldValue, newValue *string) error {
	var reqID *string
	if id := middleware.RequestIDFrom(ctx); id != "" {
		reqID = &id
	}
	var ip *string
	if v := middleware.ClientIPFrom(ctx); v != "" {
		ip = &v
	}
	_, err := a.pool.Exec(ctx, `INSERT INTO audit_log (id, actor_id, action, target, old_value, new_value, at, request_id, ip)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8)`, domain.NewID("a"), actorID, action, target, oldValue, newValue, reqID, ip)
	return err
}

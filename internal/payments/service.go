package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Booking is what the payments package needs from the core booking.
type Booking struct {
	ID         string
	RenterID   string
	ProviderID string
	Status     string
	Rent       int64 // rent value (rent_total)
	Deposit    int64 // cash deposit agreed (0 for identity/item collateral)
	Commission domain.CommissionSnapshot
	LateFeeCap int64
}

// BookingReader loads a booking inside the caller's transaction.
type BookingReader interface {
	Booking(ctx context.Context, q Querier, id string) (Booking, error)
}

// SQLBookings reads the core bookings table (00001_core_schema.sql): rent_total, the payment JSON (deposit)
// and the agreement snapshot (commission, late fee cap).
type SQLBookings struct{}

// Booking implements BookingReader.
func (SQLBookings) Booking(ctx context.Context, q Querier, id string) (Booking, error) {
	var b Booking
	var commission []byte
	err := q.QueryRow(ctx, `SELECT id, renter_id, provider_id, status, rent_total,
			coalesce((payment->>'deposit')::bigint, 0), coalesce(agreement->'commission', '{}'::jsonb),
			coalesce((agreement->>'lateFeeCap')::bigint, 0)
		FROM bookings WHERE id = $1`, id).
		Scan(&b.ID, &b.RenterID, &b.ProviderID, &b.Status, &b.Rent, &b.Deposit, &commission, &b.LateFeeCap)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, errBookingNotFound
	}
	if err != nil {
		return Booking{}, err
	}
	if err := json.Unmarshal(commission, &b.Commission); err != nil {
		return Booking{}, fmt.Errorf("payments: booking %s commission snapshot: %w", id, err)
	}
	return b, nil
}

// Config configures the service (see ConfigFromEnv).
type Config struct {
	Provider         string        // default provider name for new intents
	IntentTTL        time.Duration // QR validity, default 30 min
	MDRBasisPoints   int64         // gateway fee in basis points, QRIS 0.7% = 70
	CommissionActive bool          // global commission switch (FR-K6); false = charge 0, still report Amount
}

// Service is the payments module.
type Service struct {
	db        DB
	bookings  BookingReader
	cfg       Config
	providers map[string]Provider
	// Now is the clock (tests replace it).
	Now func() time.Time
}

// NewService builds the service. The default provider must be among providers.
func NewService(db DB, bookings BookingReader, cfg Config, providers ...Provider) (*Service, error) {
	s := &Service{db: db, bookings: bookings, cfg: cfg, providers: map[string]Provider{}, Now: time.Now}
	for _, p := range providers {
		s.providers[p.Name()] = p
	}
	if _, ok := s.providers[cfg.Provider]; !ok {
		return nil, fmt.Errorf("payments: provider %q not configured", cfg.Provider)
	}
	if s.cfg.IntentTTL <= 0 {
		s.cfg.IntentTTL = 30 * time.Minute
	}
	if s.cfg.MDRBasisPoints < 0 {
		return nil, errors.New("payments: negative MDR")
	}
	return s, nil
}

// User-facing errors (Indonesian, v1 style).
func userErr(status int, msg string) *auth.Error { return &auth.Error{Status: status, Msg: msg} }

var (
	errBookingNotFound = userErr(http.StatusNotFound, "Booking tidak ditemukan.")
	errIntentNotFound  = userErr(http.StatusNotFound, "Tagihan tidak ditemukan.")
	errOnlyRenter      = userErr(http.StatusForbidden, "Hanya penyewa yang bisa membayar booking ini.")
	errNotAccepted     = userErr(http.StatusConflict, "Pembayaran tersedia setelah request disetujui.")
	errAlreadyPaid     = userErr(http.StatusConflict, "Sudah lunas.")
	errNoDeposit       = userErr(http.StatusBadRequest, "Booking ini tidak memakai deposit uang.")
	errBadKind         = userErr(http.StatusBadRequest, "Jenis pembayaran tidak dikenal.")
	errGateway         = userErr(http.StatusBadGateway, "Gateway pembayaran sedang bermasalah. Coba lagi sebentar lagi.")
	errUnknownProvider = userErr(http.StatusNotFound, "Provider pembayaran tidak dikenal.")
	errBadWebhook      = userErr(http.StatusUnauthorized, "Tanda tangan webhook tidak valid.")
)

// ErrAlreadySettled is returned when a booking's settlement was already posted (treat as success).
var ErrAlreadySettled = errors.New("payments: booking already settled")

func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// CreateIntent creates (or returns the live) QRIS intent for a booking. Only the renter of an ACCEPTED
// booking may pay (FR rent before handover); kind is rent or deposit.
func (s *Service) CreateIntent(ctx context.Context, viewer auth.Viewer, bookingID, kind string) (Intent, error) {
	if viewer.IsGuest() {
		return Intent{}, userErr(http.StatusUnauthorized, auth.MsgLoginRequired)
	}
	if kind == "" {
		kind = KindRent
	}
	if kind != KindRent && kind != KindDeposit {
		return Intent{}, errBadKind
	}
	var b Booking
	err := inTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		b, err = s.bookings.Booking(ctx, tx, bookingID)
		return err
	})
	if err != nil {
		return Intent{}, err
	}
	if b.RenterID != viewer.UserID {
		return Intent{}, errOnlyRenter
	}
	if b.Status != "ACCEPTED" {
		return Intent{}, errNotAccepted
	}
	in := Intent{BookingID: b.ID, PayerID: b.RenterID, Kind: kind}
	switch kind {
	case KindRent:
		c := CommissionForBooking(b.Rent, b.Commission, s.cfg.CommissionActive)
		in.RentAmount, in.CommissionRenter, in.CommissionProvider = b.Rent, c.RenterPays, c.ProviderPays
		in.Amount = b.Rent + c.RenterPays
	case KindDeposit:
		if b.Deposit <= 0 {
			return Intent{}, errNoDeposit
		}
		in.Amount = b.Deposit
	}
	return s.createIntent(ctx, in)
}

// createIntent inserts the intent (unique per live booking+kind), then asks the provider for a QR.
func (s *Service) createIntent(ctx context.Context, in Intent) (Intent, error) {
	now := s.Now()
	in.Provider = s.cfg.Provider
	in.ExpiresAt = now.Add(s.cfg.IntentTTL)
	var existing *Intent
	err := inTx(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status = 'expired', updated_at = $3
			WHERE booking_id = $1 AND kind = $2 AND status = 'pending' AND expires_at <= $3`, in.BookingID, in.Kind, now); err != nil {
			return err
		}
		live, err := queryIntents(ctx, tx, `booking_id = $1 AND kind = $2 AND status IN ('pending', 'paid')`, in.BookingID, in.Kind)
		if err != nil {
			return err
		}
		if len(live) > 0 {
			existing = &live[0]
			return nil
		}
		in.ID = newID("pi")
		_, err = tx.Exec(ctx, `INSERT INTO payment_intents (id, booking_id, payer_id, kind, amount, rent_amount,
				commission_renter, commission_provider, provider, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
			in.ID, in.BookingID, in.PayerID, in.Kind, in.Amount, in.RentAmount, in.CommissionRenter,
			in.CommissionProvider, in.Provider, in.ExpiresAt, now)
		return err
	})
	if isUniqueViolation(err) { // a concurrent request won the race: return its intent
		live, qerr := queryIntents(ctx, s.db, `booking_id = $1 AND kind = $2 AND status IN ('pending', 'paid')`, in.BookingID, in.Kind)
		if qerr == nil && len(live) > 0 {
			existing, err = &live[0], nil
		}
	}
	if err != nil {
		return Intent{}, err
	}
	if existing != nil {
		if existing.Status == StatusPaid {
			return *existing, errAlreadyPaid
		}
		if existing.QRString != nil {
			return *existing, nil
		}
		in = *existing // created by a request whose provider call failed or is still running
	}
	ch, err := s.providers[in.Provider].CreateQRIS(ctx, ChargeRequest{IntentID: in.ID, Amount: in.Amount,
		ExpiresAt: in.ExpiresAt, Description: fmt.Sprintf("Sewain %s %s", in.Kind, in.BookingID)})
	if err != nil {
		_, _ = s.db.Exec(ctx, `UPDATE payment_intents SET status = 'failed', updated_at = now()
			WHERE id = $1 AND status = 'pending' AND qr_string IS NULL`, in.ID)
		log.Printf("payments: create QRIS for intent %s: %v", in.ID, err)
		return Intent{}, &auth.Error{Status: errGateway.Status, Msg: errGateway.Msg, Code: "gateway_error"}
	}
	if err := s.saveCharge(ctx, in.ID, ch); err != nil {
		return Intent{}, err
	}
	return getIntent(ctx, s.db, `id = $1`, in.ID)
}

// GetIntent returns an intent to its payer or an admin. A pending intent past its expiry is shown (and
// stored) as expired.
func (s *Service) GetIntent(ctx context.Context, viewer auth.Viewer, id string) (Intent, error) {
	in, err := getIntent(ctx, s.db, `id = $1`, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && in.PayerID != viewer.UserID && !viewer.IsAdmin()) {
		return Intent{}, errIntentNotFound
	}
	if err != nil {
		return Intent{}, err
	}
	if in.Status == StatusPending && !s.Now().Before(in.ExpiresAt) {
		if _, err := s.db.Exec(ctx, `UPDATE payment_intents SET status = 'expired', updated_at = now()
			WHERE id = $1 AND status = 'pending'`, id); err != nil {
			return Intent{}, err
		}
		in.Status = StatusExpired
	}
	return in, nil
}

// ExpireIntents marks pending intents past expires_at as expired (periodic job). A payment that still
// arrives later is accepted and flagged paid_late (the money is real).
func (s *Service) ExpireIntents(ctx context.Context) (int, error) {
	tag, err := s.db.Exec(ctx, `UPDATE payment_intents SET status = 'expired', updated_at = $1
		WHERE status = 'pending' AND expires_at <= $1`, s.Now())
	return int(tag.RowsAffected()), err
}

// saveCharge stores the provider's charge (QR payload, reference) on the intent.
func (s *Service) saveCharge(ctx context.Context, id string, ch Charge) error {
	_, err := s.db.Exec(ctx, `UPDATE payment_intents SET provider_ref = $2, qr_string = nullif($3, ''),
		va_number = nullif($4, ''), updated_at = now() WHERE id = $1`, id, ch.ProviderRef, ch.QRString, ch.VANumber)
	return err
}

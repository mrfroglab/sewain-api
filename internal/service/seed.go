package service

import (
	"context"
	"sort"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/seed"
)

// coreTables are emptied by a demo reset (sessions, idempotency keys, change log and sequences are kept, so tokens
// stay valid and versions keep increasing).
var coreTables = []string{
	"users", "stores", "verification_requests", "listings", "listing_photos", "collateral_options", "bookings", "photos",
	"messages", "chat_reads", "favorites", "reviews", "reports", "disputes", "dispute_proposals", "dispute_photos", "strikes",
	"wanted_posts", "wanted_offers", "notifications", "audit_log", "commission_configs", "collateral_rule_sets",
	"banned_keywords", "app_settings",
}

// IsEmpty reports whether the database has never been seeded or used.
func (s *Service) IsEmpty(ctx context.Context) (bool, error) {
	var used bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM commission_configs)`).Scan(&used)
	return !used, err
}

// ResetDemo replaces all data with the demo seed anchored at the real "now", in one transaction (truncate + seed).
// The demo clock goes back to real time. Photo and audit rows are evidence tables, so the reset opts in to deleting
// them (sewain.retention). Live sessions of users that exist again after the reseed (the demo personas) are kept,
// so web tabs stay signed in across a reset like in v1.
func (s *Service) ResetDemo(ctx context.Context) (Change, error) {
	d, err := seed.Build(s.realNow())
	if err != nil {
		return Change{}, err
	}
	return s.run(ctx, func(t *txn) error {
		_, err := t.tx.Exec(t.ctx, `SET LOCAL sewain.retention = 'on'`)
		t.check(err)
		_, err = t.tx.Exec(t.ctx, `CREATE TEMP TABLE keep_sessions ON COMMIT DROP AS
			SELECT * FROM auth_sessions WHERE revoked_at IS NULL AND expires_at > now()`)
		t.check(err)
		_, err = t.tx.Exec(t.ctx, `TRUNCATE `+strings.Join(coreTables, ", ")+` CASCADE`)
		t.check(err)
		t.now = s.realNow()
		t.loadData(d)
		_, err = t.tx.Exec(t.ctx, `INSERT INTO auth_sessions SELECT k.* FROM keep_sessions k WHERE EXISTS (SELECT 1 FROM users u WHERE u.id = k.user_id)`)
		t.check(err)
		t.touchPublic()
		return nil
	})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SeedIfEmpty seeds the demo data on first boot (DEMO_MODE only). Returns whether it seeded.
func (s *Service) SeedIfEmpty(ctx context.Context) (bool, error) {
	if !s.opt.DemoMode {
		return false, nil
	}
	empty, err := s.IsEmpty(ctx)
	if err != nil || !empty {
		return false, err
	}
	_, err = s.ResetDemo(ctx)
	return err == nil, err
}

// loadData inserts a whole dataset. Collections the web shows newest-first are inserted in reverse so that
// "ORDER BY ord DESC" reproduces the dataset order exactly; append-order collections are inserted as they are.
func (t *txn) loadData(d *domain.Data) {
	domain.Normalize(d)
	q, ctx := t.q, t.ctx
	for _, u := range d.Users {
		t.check(q.InsertUser(ctx, insertUserParams(u)))
		if u.Store != nil {
			t.saveStore(u)
		}
	}
	for i := len(d.Verifications) - 1; i >= 0; i-- {
		v := d.Verifications[i]
		t.check(q.InsertVerification(ctx, dbq.InsertVerificationParams{ID: v.ID, UserID: v.UserID, Method: v.Method, NameOnKtp: v.NameOnKtp,
			NikMasked: v.NikMasked, KtpPhotoID: v.KtpPhotoID, SelfiePhotoID: v.SelfiePhotoID, Status: v.Status, Auto: v.Auto,
			SubmittedAt: tsOf(v.SubmittedAt), ReviewedAt: tsPtr(v.ReviewedAt), ReviewedBy: v.ReviewedBy, Reason: v.Reason}))
	}
	for i := len(d.Listings) - 1; i >= 0; i-- {
		l := d.Listings[i]
		t.check(q.InsertListing(ctx, insertListingParams(l)))
		t.writeListingChildren(l)
	}
	for i := len(d.Bookings) - 1; i >= 0; i-- {
		t.check(q.InsertBooking(ctx, insertBookingParams(d.Bookings[i])))
	}
	for _, p := range d.Photos {
		t.check(q.InsertPhoto(ctx, dbq.InsertPhotoParams{ID: p.ID, BookingID: p.BookingID, Stage: p.Stage, UploaderID: p.UploaderID, Src: p.Src,
			Sha256: p.Sha256, ServerTs: tsOf(p.ServerTimestamp), Source: p.Source, Angle: p.Angle, Width: p.Width, Height: p.Height, Bytes: p.Bytes}))
	}
	for _, m := range d.Messages {
		var sender *string
		if m.SenderID != SystemSender {
			sender = domain.Ptr(m.SenderID)
		}
		t.check(q.InsertMessage(ctx, dbq.InsertMessageParams{ID: m.ID, BookingID: m.BookingID, SenderID: sender, Text: m.Text, PhotoID: m.PhotoID, At: tsOf(m.At)}))
	}
	reviewed := map[string]bool{}
	for _, r := range d.Reviews {
		reviewed[r.BookingID+"|"+r.FromID] = true
	}
	for _, r := range d.Reviews {
		visible := domain.ReviewVisibleAt(r.CreatedAt, reviewed[r.BookingID+"|"+r.ToID])
		t.check(q.InsertReview(ctx, dbq.InsertReviewParams{ID: r.ID, BookingID: r.BookingID, ListingID: r.ListingID, FromID: r.FromID, ToID: r.ToID,
			FromRole: r.FromRole, Rating: r.Rating, Comment: r.Comment, CreatedAt: tsOf(r.CreatedAt), VisibleAt: tsOf(visible), Released: visible <= t.now}))
	}
	for i := len(d.Reports) - 1; i >= 0; i-- {
		r := d.Reports[i]
		t.check(q.InsertReport(ctx, dbq.InsertReportParams{ID: r.ID, ReporterID: r.ReporterID, TargetType: r.TargetType, TargetID: r.TargetID,
			Reason: r.Reason, Detail: r.Detail, Status: r.Status, Resolution: r.Resolution, ResolvedAt: tsPtr(r.ResolvedAt), ResolvedBy: r.ResolvedBy,
			CreatedAt: tsOf(r.CreatedAt)}))
	}
	for i := len(d.Disputes) - 1; i >= 0; i-- {
		x := d.Disputes[i]
		t.check(q.InsertDispute(ctx, dbq.InsertDisputeParams{ID: x.ID, BookingID: x.BookingID, OpenedBy: x.OpenedBy, CaseType: x.CaseType,
			Description: x.Description, Layer: x.Layer, Status: x.Status, L1Deadline: tsOf(x.L1Deadline), Recommendation: x.Recommendation,
			Outcome: x.Outcome, ResolvedAt: tsPtr(x.ResolvedAt), CreatedAt: tsOf(x.CreatedAt)}))
		for _, p := range x.Proposals {
			t.check(q.InsertDisputeProposal(ctx, dbq.InsertDisputeProposalParams{ID: p.ID, DisputeID: x.ID, ByID: p.ByID, Type: p.Type,
				Amount: p.Amount, Note: p.Note, At: tsOf(p.At), Status: p.Status}))
		}
		if len(x.PhotoIDs) > 0 {
			t.check(q.InsertDisputePhotos(ctx, dbq.InsertDisputePhotosParams{DisputeID: x.ID, PhotoIds: x.PhotoIDs}))
		}
	}
	for i := len(d.Strikes) - 1; i >= 0; i-- {
		s := d.Strikes[i]
		t.check(q.InsertStrike(ctx, dbq.InsertStrikeParams{ID: s.ID, UserID: s.UserID, Reason: s.Reason, Severity: s.Severity, BookingID: s.BookingID,
			At: tsOf(s.At), ByAdminID: s.ByAdminID}))
	}
	for i := len(d.Audit) - 1; i >= 0; i-- {
		a := d.Audit[i]
		t.check(q.InsertAudit(ctx, dbq.InsertAuditParams{ID: a.ID, ActorID: a.ActorID, Action: a.Action, Target: a.Target, OldValue: a.OldValue,
			NewValue: a.NewValue, At: tsOf(a.At)}))
	}
	for i := len(d.Wanted) - 1; i >= 0; i-- {
		w := d.Wanted[i]
		t.check(q.InsertWanted(ctx, dbq.InsertWantedParams{ID: w.ID, RequesterID: w.RequesterID, Title: w.Title, Description: w.Description,
			Category: w.Category, AreaID: w.AreaID, Lat: w.Location.Lat, Lng: w.Location.Lng, RadiusKm: w.RadiusKm, NeededFrom: datePtr(w.NeededFrom),
			NeededUntil: datePtr(w.NeededUntil), BudgetPerDay: w.BudgetPerDay, Status: w.Status, CreatedAt: tsOf(w.CreatedAt)}))
		for _, o := range w.Offers {
			t.check(q.InsertWantedOffer(ctx, dbq.InsertWantedOfferParams{ID: o.ID, WantedID: w.ID, ProviderID: o.ProviderID, ListingID: o.ListingID,
				Message: o.Message, At: tsOf(o.At)}))
		}
	}
	for i := len(d.Notifications) - 1; i >= 0; i-- {
		n := d.Notifications[i]
		var readAt *int64
		if n.Read {
			readAt = domain.Ptr(n.At)
		}
		t.check(q.InsertNotification(ctx, dbq.InsertNotificationParams{ID: n.ID, UserID: n.UserID, Kind: n.Kind, Title: n.Title, Body: n.Body,
			Href: n.Href, At: tsOf(n.At), ReadAt: tsPtr(readAt)}))
	}
	// Favorites: v1 serializes the map with sorted keys and keeps each list in insertion order.
	for _, uid := range sortedKeys(d.Favorites) {
		for _, lid := range d.Favorites[uid] {
			t.check(q.InsertFavorite(ctx, dbq.InsertFavoriteParams{UserID: uid, ListingID: lid}))
		}
	}
	for key, at := range d.ChatReads {
		if uid, bid, ok := strings.Cut(key, ":"); ok {
			t.check(q.UpsertChatRead(ctx, dbq.UpsertChatReadParams{UserID: uid, BookingID: bid, ReadAt: tsOf(at)}))
		}
	}
	c := d.Commission
	t.check(q.InsertCommission(ctx, dbq.InsertCommissionParams{Active: c.Active, Mode: c.Mode, Percent: c.Percent, Flat: c.Flat, Min: c.Min, Max: c.Max,
		Bearer: c.Bearer, SplitRenterPercent: c.SplitRenterPercent, EffectiveFrom: tsOf(c.UpdatedAt), CreatedBy: c.UpdatedBy}))
	r := d.CollateralRules
	t.check(q.InsertCollateralRules(ctx, dbq.InsertCollateralRulesParams{LowMax: r.LowMax, MidMax: r.MidMax, MidDepositPercent: r.MidDepositPercent,
		HighDepositPercent: r.HighDepositPercent, VehicleDepositPercent: r.VehicleDepositPercent, HighValueBadge: r.HighValueBadge,
		EffectiveFrom: tsOf(r.UpdatedAt), CreatedBy: r.UpdatedBy}))
	t.check(q.InsertBannedKeywords(ctx, d.BannedKeywords))
	t.check(q.SetBookingCounter(ctx, int64(d.BookingCounter)))
	t.check(putSetting(ctx, q, "clock", clockSetting{OffsetMs: d.ClockOffset}))
	t.check(putSetting(ctx, q, "seed", seedSetting{Seeded: d.Seeded, SeedBase: d.SeedBase}))
	t.rules, t.comm, t.banned = nil, nil, nil
}

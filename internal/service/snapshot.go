package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Snapshot is GET /api/v1/state: the web app's DataState plus the server clock.
type Snapshot struct {
	*domain.Data
	ServerNow int64 `json:"serverNow"`
}

// SnapshotScope returns the effective scope (full is only honored in DEMO_MODE).
func (s *Service) SnapshotScope() string {
	if s.opt.SnapshotScope == ScopeFull && s.opt.DemoMode {
		return ScopeFull
	}
	return ScopeScoped
}

// ETag identifies a snapshot without building it: the state version, the clock offset and who is looking.
func (s *Service) ETag(ctx context.Context, viewer *Viewer) (string, int64, error) {
	var version int64
	var clock json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT coalesce((SELECT max(version) FROM change_log), 0),
		coalesce((SELECT value FROM app_settings WHERE key = 'clock'), '{}'::jsonb)`).Scan(&version, &clock)
	if err != nil {
		return "", 0, err
	}
	var c clockSetting
	_ = json.Unmarshal(clock, &c)
	return s.ETagFor(viewer, version, c.OffsetMs), version, nil
}

// ETagFor is the ETag of a snapshot with this version and clock offset for this viewer.
func (s *Service) ETagFor(viewer *Viewer, version, clockOffset int64) string {
	who := "guest"
	if viewer != nil {
		who = viewer.UserID + "/" + viewer.Role
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%d", version, s.SnapshotScope(), who, clockOffset)))
	return `W/"` + hex.EncodeToString(sum[:12]) + `"`
}

// Snapshot builds the dataset for one viewer in a single read-only REPEATABLE READ transaction, so the version and
// the rows are consistent. Visibility follows ROADMAP §4:
//   - guest: active listings (location rounded ±250 m), public profiles, released reviews, configuration, wanted posts;
//   - user:  the same + own profile, own listings (precise), own bookings and everything attached to them, own
//     notifications/favorites/chat reads/reports/strikes/verifications, own reviews;
//   - admin and scope "full": everything (v1 behavior).
func (s *Service) Snapshot(ctx context.Context, viewer *Viewer) (*Snapshot, error) {
	var out *Snapshot
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		out, err = s.buildSnapshot(ctx, dbq.New(tx), viewer)
		return err
	})
	return out, err
}

type snapReader struct {
	ctx context.Context
	q   *dbq.Queries
	err error
}

func (r *snapReader) ok(err error) bool {
	if err != nil && r.err == nil {
		r.err = err
	}
	return r.err == nil
}

func (s *Service) buildSnapshot(ctx context.Context, q *dbq.Queries, viewer *Viewer) (*Snapshot, error) {
	r := &snapReader{ctx: ctx, q: q}
	all := s.SnapshotScope() == ScopeFull || viewer.IsAdmin()
	me := ""
	if viewer != nil {
		me = viewer.UserID
	}
	d := &domain.Data{Favorites: map[domain.ID][]domain.ID{}, ChatReads: map[string]int64{}}

	version, err := q.CurrentVersion(ctx)
	r.ok(err)
	d.Version = version
	clock, err := getSetting[clockSetting](ctx, q, "clock")
	r.ok(err)
	seedInfo, err := getSetting[seedSetting](ctx, q, "seed")
	r.ok(err)
	d.ClockOffset, d.Seeded, d.SeedBase = clock.OffsetMs, seedInfo.Seeded, seedInfo.SeedBase
	counter, err := q.BookingCounter(ctx)
	r.ok(err)
	d.BookingCounter = int(counter)

	// Configuration is public (the web computes prices, deposits and the keyword filter client-side).
	if c, err := q.CurrentCommission(ctx); err == nil {
		d.Commission = commissionFromRow(c)
	} else if err != pgx.ErrNoRows {
		r.ok(err)
	}
	if c, err := q.CurrentCollateralRules(ctx); err == nil {
		d.CollateralRules = rulesFromRow(c)
	} else if err != pgx.ErrNoRows {
		r.ok(err)
	}
	words, err := q.ListBannedKeywords(ctx)
	r.ok(err)
	d.BannedKeywords = words

	// Users: everyone's public profile, the viewer's own profile in full.
	stores, err := q.ListStores(ctx)
	r.ok(err)
	storeOf := map[string]*dbq.Store{}
	for i := range stores {
		storeOf[stores[i].UserID] = &stores[i]
	}
	users, err := q.ListUsers(ctx)
	r.ok(err)
	for _, u := range users {
		du := userFromRow(u, storeOf[u.ID])
		if !all && u.ID != me {
			du = publicProfile(du)
		}
		d.Users = append(d.Users, du)
	}

	// Listings: photos and collateral options are loaded once and grouped.
	photosOf := map[string][]string{}
	lp, err := q.ListListingPhotos(ctx)
	r.ok(err)
	for _, p := range lp {
		photosOf[p.ListingID] = append(photosOf[p.ListingID], p.Src)
	}
	optionsOf := map[string][]domain.CollateralOption{}
	co, err := q.ListCollateralOptions(ctx)
	r.ok(err)
	for _, o := range co {
		optionsOf[o.ListingID] = append(optionsOf[o.ListingID], collateralFromRow(o))
	}
	var listings []dbq.ListingRow
	if all {
		listings, err = q.ListListings(ctx)
	} else {
		listings, err = q.ListListingsVisibleTo(ctx, me)
	}
	r.ok(err)
	for _, l := range listings {
		dl := listingFromRow(l, photosOf[l.ID], optionsOf[l.ID])
		if !all && l.OwnerID != me {
			dl.Location.Lat, dl.Location.Lng = domain.PublicLocation(dl.Location.Lat, dl.Location.Lng)
		}
		d.Listings = append(d.Listings, dl)
	}

	// Reviews (FR-T2): released ones, plus the viewer's own.
	var reviews []dbq.Review
	if all {
		reviews, err = q.ListReviews(ctx)
	} else {
		var viewerID *string
		if me != "" {
			viewerID = &me
		}
		reviews, err = q.ListReviewsVisibleTo(ctx, viewerID)
	}
	r.ok(err)
	for _, x := range reviews {
		d.Reviews = append(d.Reviews, reviewFromRow(x))
	}

	// Wanted board: posts are public; offers only for the requester, the offering provider and admins.
	wanted, err := q.ListWanted(ctx)
	r.ok(err)
	offers, err := q.ListWantedOffers(ctx)
	r.ok(err)
	offersOf := map[string][]domain.WantedOffer{}
	for _, o := range offers {
		offersOf[o.WantedID] = append(offersOf[o.WantedID], offerFromRow(o))
	}
	for _, w := range wanted {
		var visible []domain.WantedOffer
		for _, o := range offersOf[w.ID] {
			if all || w.RequesterID == me || o.ProviderID == me {
				visible = append(visible, o)
			}
		}
		d.Wanted = append(d.Wanted, wantedFromRow(wantedRow(w), visible))
	}

	if all {
		s.loadPrivateAll(r, d)
	} else if me != "" {
		s.loadPrivateOf(r, d, me)
	}
	if r.err != nil {
		return nil, r.err
	}
	domain.Normalize(d)
	return &Snapshot{Data: d, ServerNow: s.realNow()}, nil
}

// publicProfile keeps what anyone may see (ROADMAP §4): no phone, e-mail, NIK, strikes or verification details.
func publicProfile(u *domain.User) *domain.User {
	p := &domain.User{
		ID: u.ID, Name: domain.FirstName(u.Name), Nickname: u.Nickname, Role: u.Role, AccountType: u.AccountType,
		Verification: domain.Verification{Status: u.Verification.Status}, AreaID: u.AreaID, Campus: u.Campus,
		StudentVerified: u.StudentVerified, EarlyLender: u.EarlyLender, Bio: u.Bio, JoinedAt: u.JoinedAt, Status: u.Status,
		BaseRating: u.BaseRating, BaseRatingCount: u.BaseRatingCount, Demo: u.Demo,
	}
	if u.Store != nil {
		p.Store = &domain.StoreInfo{Name: u.Store.Name, AreaID: u.Store.AreaID, Hours: u.Store.Hours}
	}
	return p
}

func (s *Service) loadPrivateAll(r *snapReader, d *domain.Data) {
	q, ctx := r.q, r.ctx
	vs, err := q.ListVerifications(ctx)
	r.ok(err)
	for _, v := range vs {
		d.Verifications = append(d.Verifications, verificationFromRow(v))
	}
	bs, err := q.ListBookings(ctx)
	r.ok(err)
	for _, b := range bs {
		d.Bookings = append(d.Bookings, bookingFromRow(b))
	}
	ps, err := q.ListPhotos(ctx)
	r.ok(err)
	for _, p := range ps {
		d.Photos = append(d.Photos, photoFromRow(p))
	}
	ms, err := q.ListMessages(ctx)
	r.ok(err)
	for _, m := range ms {
		d.Messages = append(d.Messages, messageFromRow(m))
	}
	rs, err := q.ListReports(ctx)
	r.ok(err)
	for _, x := range rs {
		d.Reports = append(d.Reports, reportFromRow(x))
	}
	ds, err := q.ListDisputes(ctx)
	r.ok(err)
	d.Disputes = s.attachDisputes(r, ds)
	ss, err := q.ListStrikes(ctx)
	r.ok(err)
	for _, x := range ss {
		d.Strikes = append(d.Strikes, strikeFromRow(x))
	}
	as, err := q.ListAudit(ctx)
	r.ok(err)
	for _, a := range as {
		d.Audit = append(d.Audit, auditFromRow(a))
	}
	ns, err := q.ListNotifications(ctx)
	r.ok(err)
	for _, n := range ns {
		d.Notifications = append(d.Notifications, notificationFromRow(n))
	}
	fs, err := q.ListFavorites(ctx)
	r.ok(err)
	for _, f := range fs {
		d.Favorites[f.UserID] = append(d.Favorites[f.UserID], f.ListingID)
	}
	cr, err := q.ListChatReads(ctx)
	r.ok(err)
	for _, c := range cr {
		d.ChatReads[c.UserID+":"+c.BookingID] = msOf(c.ReadAt)
	}
}

func (s *Service) loadPrivateOf(r *snapReader, d *domain.Data, me string) {
	q, ctx := r.q, r.ctx
	vs, err := q.ListVerificationsByUser(ctx, me)
	r.ok(err)
	for _, v := range vs {
		d.Verifications = append(d.Verifications, verificationFromRow(v))
	}
	bs, err := q.ListBookingsOfUser(ctx, me)
	r.ok(err)
	for _, b := range bs {
		d.Bookings = append(d.Bookings, bookingFromRow(b))
	}
	ps, err := q.ListPhotosVisibleTo(ctx, me)
	r.ok(err)
	for _, p := range ps {
		d.Photos = append(d.Photos, photoFromRow(p))
	}
	ms, err := q.ListMessagesOfUser(ctx, me)
	r.ok(err)
	for _, m := range ms {
		d.Messages = append(d.Messages, messageFromRow(m))
	}
	rs, err := q.ListReportsByReporter(ctx, me)
	r.ok(err)
	for _, x := range rs {
		d.Reports = append(d.Reports, reportFromRow(x))
	}
	ds, err := q.ListDisputesOfUser(ctx, me)
	r.ok(err)
	d.Disputes = s.attachDisputes(r, ds)
	ss, err := q.ListStrikesOfUser(ctx, me)
	r.ok(err)
	for _, x := range ss {
		d.Strikes = append(d.Strikes, strikeFromRow(x))
	}
	ns, err := q.ListNotificationsOfUser(ctx, me)
	r.ok(err)
	for _, n := range ns {
		d.Notifications = append(d.Notifications, notificationFromRow(n))
	}
	fs, err := q.ListFavoritesOfUser(ctx, me)
	r.ok(err)
	for _, f := range fs {
		d.Favorites[f.UserID] = append(d.Favorites[f.UserID], f.ListingID)
	}
	cr, err := q.ListChatReadsOfUser(ctx, me)
	r.ok(err)
	for _, c := range cr {
		d.ChatReads[c.UserID+":"+c.BookingID] = msOf(c.ReadAt)
	}
}

func (s *Service) attachDisputes(r *snapReader, rows []dbq.Dispute) []*domain.Dispute {
	if len(rows) == 0 {
		return nil
	}
	ps, err := r.q.ListDisputeProposals(r.ctx)
	r.ok(err)
	proposals := map[string][]domain.DisputeProposal{}
	for _, p := range ps {
		proposals[p.DisputeID] = append(proposals[p.DisputeID], proposalFromRow(p))
	}
	phs, err := r.q.ListDisputePhotos(r.ctx)
	r.ok(err)
	photos := map[string][]domain.ID{}
	for _, p := range phs {
		photos[p.DisputeID] = append(photos[p.DisputeID], p.PhotoID)
	}
	out := make([]*domain.Dispute, 0, len(rows))
	for _, x := range rows {
		out = append(out, disputeFromRow(x, proposals[x.ID], photos[x.ID]))
	}
	return out
}

// SnapshotJSON marshals the snapshot exactly like v1 (field order of domain.Data, then serverNow).
func SnapshotJSON(snap *Snapshot) ([]byte, error) { return json.Marshal(snap) }

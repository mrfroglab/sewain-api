package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/geo"
)

// Snapshot is GET /api/v1/state: the web app's DataState plus the server clock.
type Snapshot struct {
	*domain.Data
	ServerNow int64 `json:"serverNow"`
	// ViewerVersion is the latest change relevant to this viewer (see ETag); not sent.
	ViewerVersion int64 `json:"-"`
}

// SnapshotScope returns the effective scope (full is only honored in DEMO_MODE).
func (s *Service) SnapshotScope() string {
	if s.opt.SnapshotScope == ScopeFull && s.opt.DemoMode {
		return ScopeFull
	}
	return ScopeScoped
}

// viewerVersionSQL is the latest change_log version relevant to a viewer: every change for admins and scope "full"
// ($1), otherwise public changes plus the ones naming the user ($2, ” for guests). Every write that changes what a
// viewer sees touches that viewer or marks the change public (txn.touch/touchPublic), which is what the realtime
// feed relies on too. When the viewer's rows were pruned (Housekeeping) the oldest kept version stands in: it only
// grows, so an ETag never comes back to an older value after something changed.
const viewerVersionSQL = `coalesce(
	(SELECT max(version) FROM change_log WHERE $1::boolean OR public OR ($2::text <> '' AND $2::text = ANY(users))),
	(SELECT min(version) FROM change_log), 0)`

func (s *Service) seesEverything(viewer *Viewer) bool {
	return viewer.IsAdmin() || s.SnapshotScope() == ScopeFull
}

func viewerKey(viewer *Viewer) string {
	if viewer == nil {
		return ""
	}
	return viewer.UserID
}

// ETag identifies a snapshot without building it: the latest change relevant to the viewer, the clock offset and
// who is looking. A change that only concerns other users (their bookings, chats, notifications) keeps the ETag, so
// a refetch gets 304 (load test finding 7.2: the global version gave 0% 304s).
func (s *Service) ETag(ctx context.Context, viewer *Viewer) (string, int64, error) {
	var version int64
	var clock json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT `+viewerVersionSQL+`,
		coalesce((SELECT value FROM app_settings WHERE key = 'clock'), '{}'::jsonb)`, s.seesEverything(viewer), viewerKey(viewer)).Scan(&version, &clock)
	if err != nil {
		return "", 0, err
	}
	var c clockSetting
	_ = json.Unmarshal(clock, &c)
	return s.ETagFor(viewer, version, c.OffsetMs), version, nil
}

// ETagFor is the ETag of a snapshot with this viewer version (Snapshot.ViewerVersion) and clock offset for this viewer.
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
//   - guest: active listings (location rounded ±250 m, no meeting point), public profiles, released reviews, configuration, wanted posts;
//   - user:  the same + own profile, own listings (precise), own bookings and everything attached to them, own
//     notifications/favorites/chat reads/reports/strikes/verifications, own reviews;
//   - admin and scope "full": everything (v1 behavior).
func (s *Service) Snapshot(ctx context.Context, viewer *Viewer) (*Snapshot, error) {
	return s.snapshot(ctx, viewer, s.SnapshotScope())
}

func (s *Service) snapshot(ctx context.Context, viewer *Viewer, scope string) (*Snapshot, error) {
	var out *Snapshot
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		out, err = s.buildSnapshot(ctx, tx, viewer, scope)
		return err
	})
	return out, err
}

type snapReader struct {
	ctx context.Context
	tx  pgx.Tx
	q   *dbq.Queries
	err error
}

func (r *snapReader) ok(err error) bool {
	if err != nil && r.err == nil {
		r.err = err
	}
	return r.err == nil
}

func (s *Service) buildSnapshot(ctx context.Context, tx pgx.Tx, viewer *Viewer, scope string) (*Snapshot, error) {
	q := dbq.New(tx)
	r := &snapReader{ctx: ctx, tx: tx, q: q}
	all := scope == ScopeFull || viewer.IsAdmin()
	me := ""
	d := &domain.Data{Favorites: map[domain.ID][]domain.ID{}, ChatReads: map[string]int64{}, Scope: scope}
	if viewer != nil {
		me = viewer.UserID
		d.ViewerID = domain.Ptr(me)
	}

	version, err := q.CurrentVersion(ctx)
	r.ok(err)
	d.Version = version
	var viewerVersion int64
	r.ok(tx.QueryRow(ctx, `SELECT `+viewerVersionSQL, s.seesEverything(viewer), viewerKey(viewer)).Scan(&viewerVersion))
	clock, err := getSetting[clockSetting](ctx, q, "clock")
	r.ok(err)
	seedInfo, err := getSetting[seedSetting](ctx, q, "seed")
	r.ok(err)
	d.ClockOffset, d.Seeded, d.SeedBase = clock.OffsetMs, seedInfo.Seeded, seedInfo.SeedBase
	now := s.realNow() + clock.OffsetMs // demo clock
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
			dl.Location.Lat, dl.Location.Lng = geo.FuzzPoint(l.ID, dl.Location.Lat, dl.Location.Lng)
			// FR-S6: the meeting point opens only after ACCEPTED, on the booking (meetingPointDetail).
			dl.MeetingPoint = nil
		}
		d.Listings = append(d.Listings, dl)
	}
	d.ListingBusy = r.listingBusy(d.Listings, now)

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

	// Wanted board: open posts are public (location fuzzed), the requester sees all their own posts; offers only for
	// the requester, the offering provider and admins.
	wanted, err := q.ListWanted(ctx)
	r.ok(err)
	offers, err := q.ListWantedOffers(ctx)
	r.ok(err)
	offersOf := map[string][]domain.WantedOffer{}
	for _, o := range offers {
		offersOf[o.WantedID] = append(offersOf[o.WantedID], offerFromRow(o))
	}
	for _, w := range wanted {
		own := w.RequesterID == me
		if !all && !own && w.Status != "terbuka" {
			continue
		}
		if !all && !own {
			w.Lat, w.Lng = geo.FuzzPoint(w.ID, w.Lat, w.Lng)
		}
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

	// Users last: with SNAPSHOT_USERS=referenced a scoped snapshot only carries the profiles the rest of it points
	// at, which keeps its size and cost independent of the number of accounts.
	var ids []string
	if !all && s.opt.SnapshotUsers != SnapshotUsersAll {
		ids = referencedUsers(d, me)
		if s.opt.DemoMode {
			// The persona picker and the Demo panel list every demo account, referenced or not.
			rows, err := tx.Query(ctx, `SELECT id FROM users WHERE demo IS NOT NULL AND deleted_at IS NULL`)
			if r.ok(err) {
				demo, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if r.ok(err) {
					ids = mergeIDs(ids, demo)
				}
			}
		}
	}
	s.loadUsers(r, d, all, me, now, ids)
	if r.err != nil {
		return nil, r.err
	}
	domain.Normalize(d)
	return &Snapshot{Data: d, ServerNow: s.realNow(), ViewerVersion: viewerVersion}, nil
}

// publicProfile keeps what anyone may see (ROADMAP §4): no phone, e-mail, NIK, strikes or verification details.
// Snapshot user scopes (Options.SnapshotUsers).
const (
	SnapshotUsersAll        = "all"
	SnapshotUsersReferenced = "referenced"
)

// loadUsers adds public profiles (the viewer's own profile in full, everything for admins/full scope) with their
// public reputation. ids == nil loads every user; otherwise only those ids.
func (s *Service) loadUsers(r *snapReader, d *domain.Data, all bool, me string, now int64, ids []string) {
	q, ctx := r.q, r.ctx
	stores, err := q.ListStores(ctx)
	r.ok(err)
	storeOf := map[string]*dbq.Store{}
	for i := range stores {
		storeOf[stores[i].UserID] = &stores[i]
	}
	var users []dbq.User
	if ids == nil {
		users, err = q.ListUsers(ctx)
	} else {
		users, err = q.ListUsersByIDs(ctx, ids)
	}
	r.ok(err)
	stats := r.publicStats(now, ids)
	for _, u := range users {
		du := userFromRow(u, storeOf[u.ID])
		if !all && u.ID != me {
			du = publicProfile(du)
		}
		du.PublicStats = stats[u.ID]
		d.Users = append(d.Users, du)
	}
}

// referencedUsers lists the users a scoped snapshot refers to: the viewer, the moderation account, listing owners,
// review authors and targets, the wanted board, and everyone on the viewer's bookings, messages, disputes,
// reports, strikes and verifications. Sorted, without duplicates.
func referencedUsers(d *domain.Data, me string) []string {
	set := map[string]bool{domain.AdminID: true}
	add := func(ids ...domain.ID) {
		for _, id := range ids {
			if id != "" {
				set[id] = true
			}
		}
	}
	add(me)
	for _, l := range d.Listings {
		add(l.OwnerID)
	}
	for _, x := range d.Reviews {
		add(x.FromID, x.ToID)
	}
	for _, w := range d.Wanted {
		add(w.RequesterID)
		for _, o := range w.Offers {
			add(o.ProviderID)
		}
	}
	for _, b := range d.Bookings {
		add(b.RenterID, b.ProviderID)
	}
	for _, m := range d.Messages {
		add(m.SenderID)
	}
	for _, x := range d.Disputes {
		add(x.OpenedBy)
	}
	for _, x := range d.Reports {
		add(x.ReporterID)
		if x.TargetType == "user" {
			add(x.TargetID)
		}
	}
	for _, x := range d.Strikes {
		add(x.UserID, x.ByAdminID)
	}
	for _, v := range d.Verifications {
		add(v.UserID)
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// mergeIDs returns the sorted union of two id lists.
func mergeIDs(a, b []string) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, id := range append(append([]string{}, a...), b...) {
		set[id] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

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
	// Read markers of both parties of the viewer's bookings ("Dibaca" in the chat).
	rows, err := r.tx.Query(ctx, `SELECT c.user_id, c.booking_id, c.read_at FROM chat_reads c JOIN bookings b ON b.id = c.booking_id
		WHERE b.renter_id = $1 OR b.provider_id = $1 ORDER BY c.booking_id, c.user_id`, me)
	if r.ok(err) {
		defer rows.Close()
		for rows.Next() {
			var uid, bid string
			var at time.Time
			if !r.ok(rows.Scan(&uid, &bid, &at)) {
				return
			}
			d.ChatReads[uid+":"+bid] = msOf(at)
		}
		r.ok(rows.Err())
	}
}

// publicStats computes the public reputation of every user (web trustScore, moved to the server): completed
// rentals (+ seed base count), disputes on their bookings, strikes, and the trust score over visible reviews.
// ids == nil computes every user; otherwise only those ids.
func (r *snapReader) publicStats(now int64, ids []string) map[string]*domain.PublicStats {
	out := map[string]*domain.PublicStats{}
	rows, err := r.tx.Query(r.ctx, `
		SELECT u.id, u.verification_status = 'terverifikasi', coalesce(u.base_rating, 0), coalesce(u.base_rating_count, 0), u.strikes,
		       coalesce(c.n, 0)::int, coalesce(d.n, 0)::int, coalesce(rv.ratings, '{}')
		FROM users u
		LEFT JOIN (SELECT uid, count(*) AS n FROM (
		             SELECT renter_id AS uid FROM bookings WHERE status = 'COMPLETED'
		             UNION ALL SELECT provider_id FROM bookings WHERE status = 'COMPLETED') x GROUP BY uid) c ON c.uid = u.id
		LEFT JOIN (SELECT uid, count(*) AS n FROM (
		             SELECT b.renter_id AS uid FROM disputes x JOIN bookings b ON b.id = x.booking_id
		             UNION ALL SELECT b.provider_id FROM disputes x JOIN bookings b ON b.id = x.booking_id) y GROUP BY uid) d ON d.uid = u.id
		LEFT JOIN (SELECT to_id, array_agg(rating ORDER BY ord) AS ratings FROM reviews
		           WHERE released OR visible_at <= $1 GROUP BY to_id) rv ON rv.to_id = u.id
		WHERE $2::text[] IS NULL OR u.id = ANY($2)`, tsOf(now), ids)
	if !r.ok(err) {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var verified bool
		var base float64
		var baseCount, strikes, completed, disputes int
		var ratings []float64
		if !r.ok(rows.Scan(&id, &verified, &base, &baseCount, &strikes, &completed, &disputes, &ratings)) {
			return out
		}
		completed += baseCount
		rating := domain.CombineRating(base, baseCount, ratings)
		out[id] = &domain.PublicStats{CompletedRentals: completed, Disputes: disputes, Strikes: strikes,
			TrustScore: domain.TrustScore(verified, rating, completed, disputes, strikes)}
	}
	r.ok(rows.Err())
	return out
}

// listingBusy returns, per listing in the snapshot, the merged date ranges booked by ACCEPTED/ACTIVE/DISPUTED
// bookings that end today or later (WIB), with the number of units in use on each day. No renter identity.
func (r *snapReader) listingBusy(listings []*domain.Listing, now int64) map[domain.ID][]domain.BusyRange {
	out := map[domain.ID][]domain.BusyRange{}
	if len(listings) == 0 {
		return out
	}
	ids := make([]string, 0, len(listings))
	for _, l := range listings {
		ids = append(ids, l.ID)
	}
	rows, err := r.tx.Query(r.ctx, `SELECT listing_id, start_date, end_date FROM bookings
		WHERE listing_id = ANY($1) AND status IN ('ACCEPTED', 'ACTIVE', 'DISPUTED') AND end_date >= $2`,
		ids, dateOf(domain.IsoDate(now)))
	if !r.ok(err) {
		return out
	}
	defer rows.Close()
	days := map[string]map[string]int{} // listing → YYYY-MM-DD → units
	for rows.Next() {
		var lid string
		var start, end time.Time
		if !r.ok(rows.Scan(&lid, &start, &end)) {
			return out
		}
		if days[lid] == nil {
			days[lid] = map[string]int{}
		}
		for t := start; !t.After(end); t = t.AddDate(0, 0, 1) {
			days[lid][isoOf(t)]++
		}
	}
	if !r.ok(rows.Err()) {
		return out
	}
	for lid, perDay := range days {
		out[lid] = MergeBusyDays(perDay)
	}
	return out
}

// MergeBusyDays turns per-day unit counts into runs of consecutive days with the same count, sorted by date.
func MergeBusyDays(perDay map[string]int) []domain.BusyRange {
	dates := make([]string, 0, len(perDay))
	for d := range perDay {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	var out []domain.BusyRange
	for _, d := range dates {
		n := perDay[d]
		if k := len(out) - 1; k >= 0 && out[k].Count == n && nextDay(out[k].End) == d {
			out[k].End = d
			continue
		}
		out = append(out, domain.BusyRange{Start: d, End: d, Count: n})
	}
	return out
}

func nextDay(iso string) string { return isoOf(dateOf(iso).AddDate(0, 0, 1)) }

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

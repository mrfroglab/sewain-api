package service

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// txn is the unit of work of one action. Lookups return nil when a row is missing; a database error is recorded
// once (t.err) and fails the whole transaction, so action code reads like the v1 rules it was ported from.
type txn struct {
	ctx context.Context
	s   *Service
	tx  pgx.Tx
	q   *dbq.Queries
	now int64 // demo clock (ms), fixed for the whole transaction
	err error

	changed  bool
	finished *Change // set once the change has been recorded (finish)
	users    map[string]bool
	public   bool
	titles   map[domain.ID]string
	rules    *domain.CollateralRules
	comm     *domain.CommissionConfig
	banned   []string
}

func newTxn(ctx context.Context, s *Service, tx pgx.Tx) *txn {
	return &txn{ctx: ctx, s: s, tx: tx, q: dbq.New(tx), users: map[string]bool{}, titles: map[domain.ID]string{}}
}

func (t *txn) loadClock() error {
	c, err := getSetting[clockSetting](t.ctx, t.q, "clock")
	t.now = t.s.realNow() + c.OffsetMs
	return err
}

// check records the first database error.
func (t *txn) check(err error) bool {
	if err != nil && t.err == nil {
		t.err = err
	}
	return err == nil
}

// found turns ErrNoRows into "missing" and records any other error.
func (t *txn) found(err error) bool {
	if err == nil {
		return true
	}
	if !db.IsNotFound(err) {
		t.check(err)
	}
	return false
}

func (t *txn) touch(users ...domain.ID) {
	t.changed = true
	for _, u := range users {
		if u != "" {
			t.users[u] = true
		}
	}
}

func (t *txn) touchPublic() { t.changed, t.public = true, true }

// finish records the change (new version) once; later calls return the same change.
func (t *txn) finish() (Change, error) {
	if t.finished != nil {
		return *t.finished, nil
	}
	ch, err := t.commitChange()
	if err == nil {
		t.finished = &ch
	}
	return ch, err
}

func (t *txn) commitChange() (Change, error) {
	if !t.changed {
		return Change{}, nil
	}
	users := make([]string, 0, len(t.users))
	for u := range t.users {
		users = append(users, u)
	}
	sort.Strings(users)
	v, err := t.q.InsertChange(t.ctx, dbq.InsertChangeParams{Users: users, Admins: true, Public: t.public})
	return Change{Version: v, Users: users, Admins: true, Public: t.public}, err
}

/* ---------------------------------------------------------------- users */

func (t *txn) loadUser(r dbq.User, err error) *domain.User {
	if !t.found(err) {
		return nil
	}
	var store *dbq.Store
	st, err := t.q.GetStore(t.ctx, r.ID)
	if t.found(err) {
		store = &st
	}
	return userFromRow(r, store)
}

func (t *txn) user(id domain.ID) *domain.User {
	if id == "" {
		return nil
	}
	return t.loadUser(t.q.GetUser(t.ctx, id))
}

func (t *txn) userForUpdate(id domain.ID) *domain.User {
	if id == "" {
		return nil
	}
	return t.loadUser(t.q.GetUserForUpdate(t.ctx, id))
}

func (t *txn) isAdmin(id domain.ID) bool {
	r, err := t.q.GetUserAuth(t.ctx, id)
	return t.found(err) && r.Role == "admin"
}

func (t *txn) saveUser(u *domain.User) {
	t.check(t.q.UpdateUser(t.ctx, updateUserParams(u)))
	t.saveStore(u)
	t.touch(u.ID)
	t.touchPublic()
}

func (t *txn) saveStore(u *domain.User) {
	if u.Store == nil {
		t.check(t.q.DeleteStore(t.ctx, u.ID))
		return
	}
	t.check(t.q.UpsertStore(t.ctx, dbq.UpsertStoreParams{UserID: u.ID, Name: u.Store.Name, AreaID: u.Store.AreaID, Hours: u.Store.Hours, Nib: u.Store.NIB}))
}

/* ------------------------------------------------------------- listings */

func (t *txn) loadListing(r dbq.ListingRow, err error) *domain.Listing {
	if !t.found(err) {
		return nil
	}
	photos := []string{}
	rows, err := t.q.ListPhotosOfListing(t.ctx, r.ID)
	t.check(err)
	for _, p := range rows {
		photos = append(photos, p.Src)
	}
	options := []domain.CollateralOption{}
	opts, err := t.q.ListCollateralOptionsOfListing(t.ctx, r.ID)
	t.check(err)
	for _, o := range opts {
		options = append(options, collateralFromRow(o))
	}
	l := listingFromRow(r, photos, options)
	t.titles[l.ID] = l.Title
	return l
}

func (t *txn) listing(id domain.ID) *domain.Listing {
	return t.loadListing(t.q.GetListing(t.ctx, id))
}

func (t *txn) listingForUpdate(id domain.ID) *domain.Listing {
	return t.loadListing(t.q.GetListingForUpdate(t.ctx, id))
}

func (t *txn) writeListingChildren(l *domain.Listing) {
	t.check(t.q.DeleteListingPhotos(t.ctx, l.ID))
	t.check(t.q.InsertListingPhotos(t.ctx, dbq.InsertListingPhotosParams{ListingID: l.ID, Srcs: l.Photos}))
	t.check(t.q.DeleteCollateralOptions(t.ctx, l.ID))
	seen := map[string]bool{}
	for i, o := range l.CollateralOptions {
		if o.ID == "" || seen[o.ID] {
			o.ID = domain.NewID("co")
			l.CollateralOptions[i].ID = o.ID
		}
		seen[o.ID] = true
		t.check(t.q.InsertCollateralOption(t.ctx, dbq.InsertCollateralOptionParams{
			ListingID: l.ID, ID: o.ID, Position: i, Kind: o.Kind, Cards: o.Cards, DepositMode: o.DepositMode, DepositAmount: o.DepositAmount,
			DepositPercent: o.DepositPercent, WithIdentity: o.WithIdentity, Description: o.Description, EstimatedValue: o.EstimatedValue,
		}))
	}
}

func (t *txn) insertListing(l *domain.Listing) {
	t.check(t.q.InsertListing(t.ctx, insertListingParams(l)))
	t.writeListingChildren(l)
	t.titles[l.ID] = l.Title
	t.touch(l.OwnerID)
	t.touchPublic()
}

func (t *txn) saveListing(l *domain.Listing, children bool) {
	t.check(t.q.UpdateListing(t.ctx, updateListingParams(l)))
	if children {
		t.writeListingChildren(l)
	}
	t.titles[l.ID] = l.Title
	t.touch(l.OwnerID)
	t.touchPublic()
}

// titleOf is the listing title used in notification texts.
func (t *txn) titleOf(b *domain.Booking) string {
	if title, ok := t.titles[b.ListingID]; ok {
		return title
	}
	if l := t.listing(b.ListingID); l != nil {
		return l.Title
	}
	return "barang"
}

/* ------------------------------------------------------------- bookings */

func (t *txn) booking(id domain.ID) *domain.Booking {
	if id == "" {
		return nil
	}
	r, err := t.q.GetBooking(t.ctx, id)
	if !t.found(err) {
		return nil
	}
	return bookingFromRow(r)
}

func (t *txn) bookingForUpdate(id domain.ID) *domain.Booking {
	if id == "" {
		return nil
	}
	r, err := t.q.GetBookingForUpdate(t.ctx, id)
	if !t.found(err) {
		return nil
	}
	return bookingFromRow(r)
}

func (t *txn) insertBooking(b *domain.Booking) {
	t.check(t.q.InsertBooking(t.ctx, insertBookingParams(b)))
	t.touch(b.RenterID, b.ProviderID)
}

func (t *txn) saveBooking(b *domain.Booking) {
	t.check(t.q.UpdateBooking(t.ctx, updateBookingParams(b)))
	t.touch(b.RenterID, b.ProviderID)
}

// partyBooking locks a booking the actor is a party of (role: renter | provider | any).
func (t *txn) partyBooking(actor, id domain.ID, role string) (*domain.Booking, error) {
	b := t.bookingForUpdate(id)
	if b == nil || !domain.IsParty(b, actor, role) {
		return nil, domain.Fail("Booking tidak ditemukan.")
	}
	return b, nil
}

/* ------------------------------------------------------------- disputes */

func (t *txn) loadDispute(r dbq.Dispute, err error) *domain.Dispute {
	if !t.found(err) {
		return nil
	}
	var proposals []domain.DisputeProposal
	ps, err := t.q.ListProposalsOfDispute(t.ctx, r.ID)
	t.check(err)
	for _, p := range ps {
		proposals = append(proposals, proposalFromRow(p))
	}
	var photos []domain.ID
	phs, err := t.q.ListPhotosOfDispute(t.ctx, r.ID)
	t.check(err)
	for _, p := range phs {
		photos = append(photos, p.PhotoID)
	}
	return disputeFromRow(r, proposals, photos)
}

func (t *txn) dispute(id domain.ID) *domain.Dispute { return t.loadDispute(t.q.GetDispute(t.ctx, id)) }

func (t *txn) disputeForUpdate(id domain.ID) *domain.Dispute {
	return t.loadDispute(t.q.GetDisputeForUpdate(t.ctx, id))
}

func (t *txn) saveDispute(d *domain.Dispute) {
	t.check(t.q.UpdateDispute(t.ctx, dbq.UpdateDisputeParams{ID: d.ID, Layer: d.Layer, Status: d.Status, Recommendation: d.Recommendation,
		Outcome: d.Outcome, ResolvedAt: tsPtr(d.ResolvedAt)}))
	t.touch()
}

/* --------------------------------------------------------- configuration */

func (t *txn) collateralRules() domain.CollateralRules {
	if t.rules == nil {
		r, err := t.q.CurrentCollateralRules(t.ctx)
		var rules domain.CollateralRules
		if t.found(err) {
			rules = rulesFromRow(r)
		}
		t.rules = &rules
	}
	return *t.rules
}

func (t *txn) commission() domain.CommissionConfig {
	if t.comm == nil {
		r, err := t.q.CurrentCommission(t.ctx)
		var c domain.CommissionConfig
		if t.found(err) {
			c = commissionFromRow(r)
		}
		t.comm = &c
	}
	return *t.comm
}

func (t *txn) bannedKeywords() []string {
	if t.banned == nil {
		words, err := t.q.ListBannedKeywords(t.ctx)
		t.check(err)
		t.banned = append([]string{}, words...)
	}
	return t.banned
}

func (t *txn) photosExist(ids []domain.ID) bool {
	if len(ids) == 0 {
		return true
	}
	uniq := map[string]bool{}
	for _, id := range ids {
		uniq[id] = true
	}
	list := make([]string, 0, len(uniq))
	for id := range uniq {
		list = append(list, id)
	}
	n, err := t.q.CountExistingPhotos(t.ctx, list)
	return t.check(err) && n == len(list)
}

/* ---------------------------------------------------------- side effects */

// notify is the single place that creates in-app notifications (hooks registered with Service.OnNotification run
// here, in the same transaction, e.g. to enqueue push/email outbox rows).
func (t *txn) notify(userID domain.ID, kind, title, body, href string) {
	n := &domain.Notification{ID: domain.NewID("n"), UserID: userID, Kind: kind, Title: title, Body: body, At: t.now}
	if href != "" {
		n.Href = domain.Ptr(href)
	}
	if !t.check(t.q.InsertNotification(t.ctx, dbq.InsertNotificationParams{ID: n.ID, UserID: n.UserID, Kind: n.Kind, Title: n.Title,
		Body: n.Body, Href: n.Href, At: tsOf(n.At)})) {
		return
	}
	t.touch(userID)
	t.s.mu.RLock()
	hooks := append([]func(context.Context, pgx.Tx, *domain.Notification) error{}, t.s.notifiers...)
	t.s.mu.RUnlock()
	for _, h := range hooks {
		t.check(h(t.ctx, t.tx, n))
	}
}

// sysMsg posts a system message in the booking chat.
func (t *txn) sysMsg(bookingID domain.ID, text string) {
	t.check(t.q.InsertMessage(t.ctx, dbq.InsertMessageParams{ID: domain.NewID("m"), BookingID: bookingID, Text: domain.Ptr(text), At: tsOf(t.now)}))
	t.touch()
}

// audit appends to the audit log (FR-K7).
func (t *txn) audit(actor domain.ID, action, target string, oldV, newV *string) {
	t.check(t.q.InsertAudit(t.ctx, dbq.InsertAuditParams{ID: domain.NewID("a"), ActorID: actor, Action: action, Target: target,
		OldValue: oldV, NewValue: newV, At: tsOf(t.now)}))
	t.touch()
}

// pickID: client id when valid and unused (see domain.PickID).
func (t *txn) pickID(want, prefix string, exists func(context.Context, string) (bool, error)) domain.ID {
	return domain.PickID(want, prefix, func(id domain.ID) bool {
		ok, err := exists(t.ctx, id)
		t.check(err)
		return ok
	})
}

package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/geo"
)

// PublicUserPage is GET /api/v1/users/{id}: what anyone may see about one user, for the web's /u/[id] when the
// profile is not in the viewer's snapshot (SNAPSHOT_USERS=referenced).
type PublicUserPage struct {
	// User is exactly the users[] entry of a guest snapshot (publicProfile + publicStats, demo in DEMO_MODE).
	User *domain.User `json:"user"`
	// Listings are the user's active listings as a guest sees them (location fuzzed, no meeting point).
	Listings []*domain.Listing `json:"listings"`
	// Reviews are the reviews about the user that are shown (released, or past visibleAt).
	Reviews []*domain.Review `json:"reviews"`
}

// PublicUser builds the public profile page of a user, the same for every viewer (the response is publicly
// cacheable). nil when the user does not exist, was deleted or is blacklisted.
func (s *Service) PublicUser(ctx context.Context, id string) (*PublicUserPage, error) {
	var out *PublicUserPage
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		u, err := q.GetUser(ctx, id)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.DeletedAt != nil || u.Status == "blacklist" {
			return nil
		}
		var store *dbq.Store
		if st, err := q.GetStore(ctx, id); err == nil {
			store = &st
		} else if !isNoRows(err) {
			return err
		}
		clock, err := getSetting[clockSetting](ctx, q, "clock")
		if err != nil {
			return err
		}
		now := s.realNow() + clock.OffsetMs
		r := &snapReader{ctx: ctx, tx: tx, q: q}
		page := &PublicUserPage{User: publicProfile(userFromRow(u, store)), Listings: []*domain.Listing{}, Reviews: []*domain.Review{}}
		page.User.PublicStats = r.publicStats(now, []string{id})[id]

		if u.Status == "aktif" { // like the snapshot: listings of suspended owners are not public
			rows, err := q.ListActiveListingsOfOwner(ctx, id)
			if err != nil {
				return err
			}
			for _, l := range rows {
				photos := []string{}
				ps, err := q.ListPhotosOfListing(ctx, l.ID)
				if err != nil {
					return err
				}
				for _, p := range ps {
					photos = append(photos, p.Src)
				}
				options := []domain.CollateralOption{}
				opts, err := q.ListCollateralOptionsOfListing(ctx, l.ID)
				if err != nil {
					return err
				}
				for _, o := range opts {
					options = append(options, collateralFromRow(o))
				}
				dl := listingFromRow(l, photos, options)
				dl.Location.Lat, dl.Location.Lng = geo.FuzzPoint(l.ID, dl.Location.Lat, dl.Location.Lng)
				dl.MeetingPoint = nil
				domain.NormListing(dl)
				page.Listings = append(page.Listings, dl)
			}
		}
		reviews, err := q.ListVisibleReviewsAbout(ctx, dbq.ListVisibleReviewsAboutParams{ToID: id, Now: tsOf(now)})
		if err != nil {
			return err
		}
		for _, x := range reviews {
			page.Reviews = append(page.Reviews, reviewFromRow(x))
		}
		if r.err != nil {
			return r.err
		}
		out = page
		return nil
	})
	return out, err
}

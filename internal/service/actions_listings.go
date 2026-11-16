package service

import (
	"regexp"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// PhotoPath is the canonical (unsigned) URL of a stored photo; public listing photos are served there.
const PhotoPath = "/api/v1/photos/"

var (
	photoIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	demoImgPattern  = regexp.MustCompile(`^/img/[A-Za-z0-9_./-]+$`)
	errPhotoForeign = domain.Fail("Foto barang harus diunggah lewat Sewain, bukan tautan dari situs lain.")
	errPhotoNotOwn  = domain.Fail("Foto barang harus foto listing yang kamu unggah sendiri.")
)

// listingPhotos normalizes and checks the photos of a listing (review wave 2, L4). Each one is a photo uploaded as a
// listing photo (stage referensi) by one of uploaders, written "/api/v1/photos/<id>" or as the bare id, or a bundled
// image "/img/…" in DEMO_MODE. A photo the listing already has stays allowed (seed data and earlier imports) as long
// as it is served by Sewain. External URLs are refused: visitors' browsers would load them from someone else's server
// (tracking) and they never went through media.Inspect.
func (t *txn) listingPhotos(photos, current []string, uploaders ...domain.ID) ([]string, error) {
	kept := map[string]bool{}
	for _, src := range current {
		kept[src] = true
	}
	out := make([]string, 0, len(photos))
	var ids []string
	for _, src := range photos {
		src = strings.TrimSpace(src)
		if photoIDPattern.MatchString(src) {
			src = PhotoPath + src
		}
		switch {
		case strings.HasPrefix(src, PhotoPath) && photoIDPattern.MatchString(strings.TrimPrefix(src, PhotoPath)):
			if !kept[src] {
				ids = append(ids, strings.TrimPrefix(src, PhotoPath))
			}
		case demoImgPattern.MatchString(src) && !strings.Contains(src, ".."):
			if !t.s.opt.DemoMode && !kept[src] {
				return nil, errPhotoNotOwn
			}
		default:
			return nil, errPhotoForeign
		}
		out = append(out, src)
	}
	if len(ids) > 0 {
		var bad bool
		err := t.tx.QueryRow(t.ctx, `SELECT count(DISTINCT id) < cardinality(ARRAY(SELECT DISTINCT unnest($1::text[])))
			FROM photos WHERE id = ANY($1) AND stage = 'referensi' AND uploader_id = ANY($2)`, ids, uploaders).Scan(&bad)
		if !t.check(err) {
			return nil, nil
		}
		if bad {
			return nil, errPhotoNotOwn
		}
	}
	return out, nil
}

func (t *txn) createListing(ownerID domain.ID, in domain.ListingInput, wantID string) (domain.ID, error) {
	owner := t.user(ownerID)
	if err := domain.CanTransact(owner); err != nil {
		return "", err
	}
	id := t.pickID(wantID, "l", t.q.ListingExists)
	l := &domain.Listing{ID: id, Slug: id, OwnerID: ownerID, Status: "aktif", CreatedAt: t.now, Stock: 1, Approval: "manual", RadiusKm: 3}
	domain.ApplyListing(l, in)
	photos, err := t.listingPhotos(l.Photos, nil, ownerID)
	if err != nil {
		return "", err
	}
	l.Photos = photos
	if owner.AccountType != "toko" || l.Stock < 1 {
		l.Stock = 1
	}
	if domain.NeedsManualApproval(l, t.collateralRules()) {
		l.Approval = "manual"
	}
	if err := domain.ValidateListing(l, t.bannedKeywords()); err != nil {
		return "", err
	}
	if l.RadiusKm <= 0 {
		l.RadiusKm = 3
	}
	if l.Location.AreaID == "" {
		l.Location.AreaID = owner.AreaID
	}
	t.insertListing(l)
	// Wanted board: a new listing in a requested category notifies the requesters (demand → supply).
	posts, err := t.q.ListOpenWanted(t.ctx)
	t.check(err)
	for _, w := range posts {
		if w.Category != nil && *w.Category == l.Category && w.RequesterID != ownerID {
			t.notify(w.RequesterID, "papan", "Ada barang baru yang cocok", l.Title+" baru saja disewakan di dekatmu.", "/barang/"+id)
		}
	}
	return id, nil
}

func (t *txn) updateListing(actor, listingID domain.ID, in domain.ListingInput) error {
	l := t.listingForUpdate(listingID)
	if l == nil {
		return domain.Fail("Listing tidak ditemukan.")
	}
	if l.OwnerID != actor && !t.isAdmin(actor) {
		return domain.Fail("Bukan listing milikmu.")
	}
	next := *l
	domain.ApplyListing(&next, in)
	photos, err := t.listingPhotos(next.Photos, l.Photos, l.OwnerID, actor)
	if err != nil {
		return err
	}
	next.Photos = photos
	if err := domain.ValidateListing(&next, t.bannedKeywords()); err != nil {
		return err
	}
	// Same rules as createListing: stock > 1 only for shop accounts, manual approval for high-value items and
	// vehicles (PRD 11.4).
	if owner := t.user(l.OwnerID); owner == nil || owner.AccountType != "toko" || next.Stock < 1 {
		next.Stock = 1
	}
	if domain.NeedsManualApproval(&next, t.collateralRules()) {
		next.Approval = "manual"
	}
	if next.RadiusKm <= 0 {
		next.RadiusKm = l.RadiusKm
	}
	t.saveListing(&next, true)
	return nil
}

func (t *txn) setListingStatus(actor, listingID domain.ID, status string) error {
	l := t.listingForUpdate(listingID)
	if l == nil {
		return domain.Fail("Listing tidak ditemukan.")
	}
	admin := t.isAdmin(actor)
	if l.OwnerID != actor && !admin {
		return domain.Fail("Bukan listing milikmu.")
	}
	if status != "aktif" && status != "jeda" && status != "diturunkan" {
		return domain.Fail("Status tidak dikenal.")
	}
	if status == "diturunkan" && !admin {
		return domain.Fail("Hanya admin yang bisa menurunkan listing.")
	}
	old := l.Status
	t.check(t.q.SetListingStatus(t.ctx, dbq.SetListingStatusParams{ID: l.ID, Status: status}))
	t.touch(l.OwnerID)
	t.touchPublic()
	if admin {
		t.audit(actor, "listing."+status, l.Title, domain.Ptr(old), domain.Ptr(status))
	}
	return nil
}

func (t *txn) toggleFavorite(userID, listingID domain.ID) error {
	n, err := t.q.DeleteFavorite(t.ctx, dbq.DeleteFavoriteParams{UserID: userID, ListingID: listingID})
	t.check(err)
	if n == 0 {
		t.check(t.q.InsertFavorite(t.ctx, dbq.InsertFavoriteParams{UserID: userID, ListingID: listingID}))
	}
	t.touch(userID)
	return nil
}

package service

import (
	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

func (t *txn) createListing(ownerID domain.ID, in domain.ListingInput, wantID string) (domain.ID, error) {
	owner := t.user(ownerID)
	if err := domain.CanTransact(owner); err != nil {
		return "", err
	}
	id := t.pickID(wantID, "l", t.q.ListingExists)
	l := &domain.Listing{ID: id, Slug: id, OwnerID: ownerID, Status: "aktif", CreatedAt: t.now, Stock: 1, Approval: "manual", RadiusKm: 3}
	domain.ApplyListing(l, in)
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

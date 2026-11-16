package service

import (
	"fmt"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

/* ------------------------------------------------------------- disputes */

// openDispute starts L1: 48h self-negotiation (PRD 11.2).
func (t *txn) openDispute(actor, bookingID domain.ID, in domain.DisputeInput, wantID string) (domain.ID, error) {
	b, err := t.partyBooking(actor, bookingID, "any")
	if err != nil {
		return "", err
	}
	if b.Status != "ACTIVE" && b.Status != "RETURNED" {
		return "", domain.Fail("Sengketa bisa dibuka saat sedang disewa, telat, atau setelah pengembalian.")
	}
	if b.DisputeID != nil {
		if d := t.dispute(*b.DisputeID); d != nil && d.Status != "selesai" {
			return "", domain.Fail("Sudah ada sengketa berjalan.")
		}
	}
	if strings.TrimSpace(in.Description) == "" {
		return "", domain.Fail("Ceritakan masalahnya.")
	}
	if in.PhotoIDs == nil {
		in.PhotoIDs = []domain.ID{}
	}
	if !t.photosExist(in.PhotoIDs) {
		return "", domain.Fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	}
	if !t.evidenceOf(b, actor, in.PhotoIDs) {
		return "", domain.Fail("Foto bukti harus diunggah untuk booking ini.")
	}
	id := t.pickID(wantID, "d", t.q.DisputeExists)
	t.check(t.q.InsertDispute(t.ctx, dbq.InsertDisputeParams{ID: id, BookingID: bookingID, OpenedBy: actor, CaseType: in.CaseType,
		Description: strings.TrimSpace(in.Description), Layer: "L1", Status: "negosiasi", L1Deadline: tsOf(t.now + domain.NegotiationTTL), CreatedAt: tsOf(t.now)}))
	if len(in.PhotoIDs) > 0 {
		t.check(t.q.InsertDisputePhotos(t.ctx, dbq.InsertDisputePhotosParams{DisputeID: id, PhotoIds: in.PhotoIDs}))
	}
	b.Status, b.DisputeID = "DISPUTED", domain.Ptr(id)
	t.saveBooking(b)
	t.notify(domain.Counterpart(b, actor), "sengketa", "Ada laporan masalah", domain.PublicName(t.user(actor))+" melaporkan masalah pada "+t.titleOf(b)+". Respons dalam 48 jam.", domain.BookingHref(b)+"/sengketa")
	t.sysMsg(bookingID, "Laporan masalah dibuka. Tahap L1: negosiasi mandiri 48 jam. Ajukan solusi lewat halaman sengketa.")
	return id, nil
}

// disputeForParty locks the dispute and its booking for a party of that booking.
func (t *txn) disputeForParty(actor, id domain.ID) (*domain.Dispute, *domain.Booking, error) {
	d := t.dispute(id)
	if d == nil {
		return nil, nil, domain.Fail("Sengketa tidak ditemukan.")
	}
	b := t.bookingForUpdate(d.BookingID) // lock order: booking, then dispute
	if b == nil || !domain.IsParty(b, actor, "any") {
		return nil, nil, domain.Fail("Sengketa tidak ditemukan.")
	}
	return t.disputeForUpdate(id), b, nil
}

func (t *txn) proposeSolution(actor, id domain.ID, in domain.ProposalInput) error {
	d, b, err := t.disputeForParty(actor, id)
	if err != nil {
		return err
	}
	if d.Status != "negosiasi" {
		return domain.Fail("Negosiasi sudah ditutup.")
	}
	if domain.ProposalLabel[in.Type] == "" {
		return domain.Fail("Jenis solusi tidak dikenal.")
	}
	t.check(t.q.InsertDisputeProposal(t.ctx, dbq.InsertDisputeProposalParams{ID: domain.NewID("pr"), DisputeID: id, ByID: actor, Type: in.Type,
		Amount: in.Amount, Note: in.Note, At: tsOf(t.now), Status: "diajukan"}))
	t.touch(b.RenterID, b.ProviderID)
	t.notify(domain.Counterpart(b, actor), "sengketa", "Usulan solusi baru", domain.PublicName(t.user(actor))+" mengusulkan solusi untuk "+t.titleOf(b)+".", domain.BookingHref(b)+"/sengketa")
	return nil
}

func (t *txn) respondProposal(actor, id, proposalID domain.ID, accept bool) error {
	d, b, err := t.disputeForParty(actor, id)
	if err != nil {
		return err
	}
	var p *domain.DisputeProposal
	for i := range d.Proposals {
		if d.Proposals[i].ID == proposalID {
			p = &d.Proposals[i]
		}
	}
	if p == nil {
		return domain.Fail("Usulan tidak ditemukan.")
	}
	if p.ByID == actor {
		return domain.Fail("Tidak bisa merespons usulan sendiri.")
	}
	if p.Status != "diajukan" || d.Status != "negosiasi" {
		return domain.Fail("Usulan sudah direspons.")
	}
	t.touch(b.RenterID, b.ProviderID)
	if !accept {
		t.check(t.q.SetProposalStatus(t.ctx, dbq.SetProposalStatusParams{ID: p.ID, Status: "ditolak"}))
		t.notify(p.ByID, "sengketa", "Usulan ditolak", t.titleOf(b)+": usulanmu ditolak. Ajukan usulan lain atau naikkan ke admin.", domain.BookingHref(b)+"/sengketa")
		t.sysMsg(b.ID, "Usulan solusi ditolak.")
		return nil
	}
	t.check(t.q.SetProposalStatus(t.ctx, dbq.SetProposalStatusParams{ID: p.ID, Status: "diterima"}))
	outcome := "Disepakati di L1: " + domain.ProposalLabel[p.Type]
	if p.Amount != nil {
		outcome += " " + domain.Rupiah(*p.Amount)
	}
	outcome += "."
	d.Status, d.Outcome, d.ResolvedAt = "selesai", domain.Ptr(outcome), domain.Ptr(t.now)
	t.saveDispute(d)
	b.Status, b.ResolvedAt = "RESOLVED", domain.Ptr(t.now)
	t.saveBooking(b)
	t.notify(p.ByID, "sengketa", "Usulan diterima", t.titleOf(b)+": "+outcome, domain.BookingHref(b)+"/sengketa")
	t.sysMsg(b.ID, "Sengketa selesai lewat negosiasi. "+outcome)
	return nil
}

func (t *txn) escalateDispute(actor, id domain.ID) error {
	d, b, err := t.disputeForParty(actor, id)
	if err != nil {
		return err
	}
	if d.Status != "negosiasi" {
		return domain.Fail("Sengketa sudah di tahap mediasi.")
	}
	d.Layer, d.Status = "L2", "mediasi"
	t.saveDispute(d)
	t.touch(b.RenterID, b.ProviderID)
	t.notify(domain.AdminID, "sengketa", "Sengketa menunggu mediasi", t.titleOf(b)+": dinaikkan ke L2 oleh "+domain.PublicName(t.user(actor))+".", "/admin/sengketa/"+id)
	t.notify(domain.Counterpart(b, actor), "sengketa", "Sengketa naik ke mediasi admin", t.titleOf(b)+": admin akan memberi rekomendasi maksimal 5 hari kerja.", domain.BookingHref(b)+"/sengketa")
	t.sysMsg(b.ID, "Sengketa dinaikkan ke mediasi admin (L2). Admin menilai foto, chat, jaminan, dan kesepakatan.")
	return nil
}

// adminDispute locks a dispute (and its booking) for an admin action.
func (t *txn) adminDispute(id domain.ID) (*domain.Dispute, *domain.Booking) {
	d := t.dispute(id)
	if d == nil {
		return nil, nil
	}
	b := t.bookingForUpdate(d.BookingID)
	return t.disputeForUpdate(id), b
}

func (t *txn) adminRecommend(adminID, id domain.ID, text string, amount *int64) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	d, b := t.adminDispute(id)
	if d == nil {
		return domain.Fail("Sengketa tidak ditemukan.")
	}
	if strings.TrimSpace(text) == "" {
		return domain.Fail("Tulis rekomendasinya.")
	}
	d.Layer, d.Status = "L2", "mediasi"
	d.Recommendation = &domain.Recommendation{Text: strings.TrimSpace(text), Amount: amount, ByAdminID: adminID, At: t.now}
	t.saveDispute(d)
	if b != nil {
		t.touch(b.RenterID, b.ProviderID)
		for _, u := range []domain.ID{b.RenterID, b.ProviderID} {
			t.notify(u, "sengketa", "Rekomendasi admin keluar", t.titleOf(b)+": "+domain.Truncate(text, 90), domain.BookingHref(b)+"/sengketa")
		}
		msg := "Rekomendasi admin: " + text
		if amount != nil {
			msg += " (" + domain.Rupiah(*amount) + ")"
		}
		t.sysMsg(b.ID, msg)
		t.audit(adminID, "sengketa.rekomendasi", t.titleOf(b)+" ("+b.Code+")", nil, domain.Ptr(text))
	}
	return nil
}

func (t *txn) resolveDispute(adminID, id domain.ID, outcome string) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	d, b := t.adminDispute(id)
	if d == nil {
		return domain.Fail("Sengketa tidak ditemukan.")
	}
	if strings.TrimSpace(outcome) == "" {
		return domain.Fail("Tulis hasil akhirnya.")
	}
	d.Status, d.Outcome, d.ResolvedAt = "selesai", domain.Ptr(strings.TrimSpace(outcome)), domain.Ptr(t.now)
	t.saveDispute(d)
	if b != nil {
		b.Status, b.ResolvedAt = "RESOLVED", domain.Ptr(t.now)
		t.saveBooking(b)
		for _, u := range []domain.ID{b.RenterID, b.ProviderID} {
			t.notify(u, "sengketa", "Sengketa selesai", t.titleOf(b)+": "+domain.Truncate(outcome, 100), domain.BookingHref(b)+"/sengketa")
		}
		t.sysMsg(b.ID, "Sengketa ditutup admin. Hasil: "+outcome)
		t.audit(adminID, "sengketa.selesai", t.titleOf(b)+" ("+b.Code+")", nil, domain.Ptr(outcome))
	}
	return nil
}

/* ----------------------------------------------------------- moderation */

func (t *txn) createReport(reporterID domain.ID, in domain.ReportInput, wantID string) (domain.ID, error) {
	if in.TargetType != "listing" && in.TargetType != "user" {
		return "", domain.Fail("Target laporan tidak dikenal.")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return "", domain.Fail("Pilih alasan laporan.")
	}
	id := t.pickID(wantID, "rp", t.q.ReportExists)
	t.check(t.q.InsertReport(t.ctx, dbq.InsertReportParams{ID: id, ReporterID: reporterID, TargetType: in.TargetType, TargetID: in.TargetID,
		Reason: in.Reason, Detail: in.Detail, Status: "baru", CreatedAt: tsOf(t.now)}))
	t.touch(reporterID)
	kind := "Pengguna"
	if in.TargetType == "listing" {
		kind = "Listing"
	}
	t.notify(domain.AdminID, "sistem", "Laporan baru", kind+" dilaporkan: "+in.Reason, "/admin/laporan")
	return id, nil
}

func (t *txn) resolveReport(adminID, reportID domain.ID, status, resolution string) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	if status != "ditindak" && status != "ditolak" {
		return domain.Fail("Status laporan tidak dikenal.")
	}
	r, err := t.q.GetReportForUpdate(t.ctx, reportID)
	if !t.found(err) {
		return domain.Fail("Laporan tidak ditemukan.")
	}
	t.check(t.q.ResolveReport(t.ctx, dbq.ResolveReportParams{ID: r.ID, Status: status, Resolution: domain.Ptr(resolution), ResolvedAt: tsPtr(&t.now), ResolvedBy: domain.Ptr(adminID)}))
	t.touch(r.ReporterID)
	t.audit(adminID, "laporan."+status, r.Reason, nil, domain.Ptr(resolution))
	t.notify(r.ReporterID, "sistem", "Laporanmu sudah ditinjau", resolution, "")
	return nil
}

func (t *txn) addStrike(adminID, userID domain.ID, in domain.StrikeInput) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	u := t.userForUpdate(userID)
	if u == nil {
		return domain.Fail("Pengguna tidak ditemukan.")
	}
	if in.Severity != "berat" {
		in.Severity = "ringan"
	}
	add := 1
	if in.Severity == "berat" {
		add = 2
	}
	t.check(t.q.InsertStrike(t.ctx, dbq.InsertStrikeParams{ID: domain.NewID("s"), UserID: userID, Reason: in.Reason, Severity: in.Severity,
		BookingID: in.BookingID, At: tsOf(t.now), ByAdminID: adminID}))
	old := u.Strikes
	u.Strikes += add
	t.saveUser(u)
	t.notify(userID, "sistem", "Kamu mendapat strike", in.Reason+". Strike berulang bisa berujung suspend.", "/profil")
	t.audit(adminID, "user.strike", domain.PublicName(u), domain.Ptr(fmt.Sprint(old)), domain.Ptr(fmt.Sprintf("%d (%s: %s)", u.Strikes, in.Severity, in.Reason)))
	return nil
}

// setUserStatus: L3 consequences — suspend / blacklist (NIK-based) / restore. Listings of blocked users are paused.
func (t *txn) setUserStatus(adminID, userID domain.ID, status, reason string) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	u := t.userForUpdate(userID)
	if u == nil {
		return domain.Fail("Pengguna tidak ditemukan.")
	}
	if status != "aktif" && status != "suspend" && status != "blacklist" {
		return domain.Fail("Status tidak dikenal.")
	}
	old := u.Status
	u.Status = status
	t.saveUser(u)
	if status == "blacklist" {
		t.blacklistIdentity(adminID, userID, reason)
	}
	if status != "aktif" {
		ls, err := t.q.ListActiveListingsByOwnerForUpdate(t.ctx, userID)
		t.check(err)
		for _, l := range ls {
			t.check(t.q.SetListingStatus(t.ctx, dbq.SetListingStatusParams{ID: l.ID, Status: "jeda"}))
		}
	}
	title := map[string]string{"aktif": "Akun dipulihkan", "suspend": "Akun ditangguhkan", "blacklist": "Akun diblokir"}[status]
	t.notify(userID, "sistem", title, reason, "/profil")
	nv := status + " — " + reason
	if status == "blacklist" {
		nv += " (NIK " + domain.Deref(u.Verification.NikMasked, "-") + ")"
	}
	t.audit(adminID, "user."+status, domain.PublicName(u), domain.Ptr(old), domain.Ptr(nv))
	return nil
}

/* ------------------------------------------------------------- settings */

// updateCommission (FR-K1..K7): a new config version applies to NEW bookings only — agreements keep their snapshot
// (FR-K5). Audited.
func (t *txn) updateCommission(adminID domain.ID, p domain.CommissionPatch) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	old := t.commission()
	c, err := domain.ApplyCommissionPatch(old, p)
	if err != nil {
		return err
	}
	c.UpdatedAt, c.UpdatedBy = t.now, adminID
	t.check(t.q.InsertCommission(t.ctx, dbq.InsertCommissionParams{Active: c.Active, Mode: c.Mode, Percent: c.Percent, Flat: c.Flat, Min: c.Min,
		Max: c.Max, Bearer: c.Bearer, SplitRenterPercent: c.SplitRenterPercent, EffectiveFrom: tsOf(c.UpdatedAt), CreatedBy: adminID}))
	t.comm = &c
	t.touchPublic()
	t.audit(adminID, "komisi.ubah", "Pengaturan komisi", domain.Ptr(domain.FormatCommission(old)), domain.Ptr(domain.FormatCommission(c)))
	return nil
}

func (t *txn) updateCollateralRules(adminID domain.ID, p domain.CollateralRulesPatch) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	old := t.collateralRules()
	r, err := domain.ApplyCollateralRulesPatch(old, p)
	if err != nil {
		return err
	}
	r.UpdatedAt, r.UpdatedBy = t.now, adminID
	t.check(t.q.InsertCollateralRules(t.ctx, dbq.InsertCollateralRulesParams{LowMax: r.LowMax, MidMax: r.MidMax, MidDepositPercent: r.MidDepositPercent,
		HighDepositPercent: r.HighDepositPercent, VehicleDepositPercent: r.VehicleDepositPercent, HighValueBadge: r.HighValueBadge,
		EffectiveFrom: tsOf(r.UpdatedAt), CreatedBy: adminID}))
	t.rules = &r
	t.touchPublic()
	t.audit(adminID, "jaminan.ubah", "Saran jaminan", domain.Ptr(domain.FormatCollateralRules(old)), domain.Ptr(domain.FormatCollateralRules(r)))
	return nil
}

func (t *txn) setBannedKeywords(adminID domain.ID, words []string) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	clean := domain.CleanKeywords(words)
	old := len(t.bannedKeywords())
	t.check(t.q.DeleteBannedKeywords(t.ctx))
	t.check(t.q.InsertBannedKeywords(t.ctx, clean))
	t.banned = clean
	t.touchPublic()
	t.audit(adminID, "kata-terlarang.ubah", "Daftar kata kunci larangan", domain.Ptr(fmt.Sprintf("%d kata", old)), domain.Ptr(fmt.Sprintf("%d kata", len(clean))))
	return nil
}

/* ---------------------------------------------------------------- wanted */

// createWanted (P1 papan permintaan): demand becomes a supply signal — providers with active listings in that
// category are notified.
func (t *txn) createWanted(requesterID domain.ID, in domain.WantedInput, wantID string) (domain.ID, error) {
	if err := domain.CanTransact(t.user(requesterID)); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Title) == "" {
		return "", domain.Fail("Tulis barang yang kamu cari.")
	}
	if words := domain.BannedWordsIn(in.Title+" "+in.Description, t.bannedKeywords()); len(words) > 0 {
		return "", domain.Fail("Ada kata yang dilarang: " + strings.Join(words, ", ") + ".")
	}
	area, ok := domain.Areas[in.AreaID]
	if !ok {
		area = domain.Areas["ugm"]
	}
	if in.RadiusKm <= 0 {
		in.RadiusKm = 3
	}
	id := t.pickID(wantID, "w", t.q.WantedExists)
	title := strings.TrimSpace(in.Title)
	t.check(t.q.InsertWanted(t.ctx, dbq.InsertWantedParams{ID: id, RequesterID: requesterID, Title: title, Description: in.Description,
		Category: in.Category, AreaID: area.ID, Lat: area.Lat, Lng: area.Lng, RadiusKm: in.RadiusKm, NeededFrom: datePtr(in.NeededFrom),
		NeededUntil: datePtr(in.NeededUntil), BudgetPerDay: in.BudgetPerDay, Status: "terbuka", CreatedAt: tsOf(t.now)}))
	t.touch(requesterID)
	t.touchPublic()
	ls, err := t.q.ListActiveListingsByOthers(t.ctx, requesterID)
	t.check(err)
	notified := map[domain.ID]bool{}
	for _, l := range ls {
		if (in.Category == nil || l.Category == *in.Category) && !notified[l.OwnerID] {
			notified[l.OwnerID] = true
			t.notify(l.OwnerID, "papan", "Ada yang mencari barang", "\""+title+"\" di sekitar "+area.Name+". Punya? Tawarkan sekarang.", "/papan")
		}
	}
	return id, nil
}

func (t *txn) offerWanted(providerID, wantedID domain.ID, listingID *domain.ID, message string) error {
	w, err := t.q.GetWantedForUpdate(t.ctx, wantedID)
	if !t.found(err) || w.Status != "terbuka" {
		return domain.Fail("Permintaan sudah ditutup.")
	}
	if w.RequesterID == providerID {
		return domain.Fail("Ini permintaanmu sendiri.")
	}
	if err := domain.CanTransact(t.user(providerID)); err != nil {
		return err
	}
	t.check(t.q.InsertWantedOffer(t.ctx, dbq.InsertWantedOfferParams{ID: domain.NewID("wo"), WantedID: wantedID, ProviderID: providerID,
		ListingID: listingID, Message: message, At: tsOf(t.now)}))
	t.touch(providerID, w.RequesterID)
	href := "/papan"
	if listingID != nil {
		href = "/barang/" + *listingID
	}
	t.notify(w.RequesterID, "papan", "Ada yang menawarkan barang", domain.PublicName(t.user(providerID))+" merespons \""+w.Title+"\".", href)
	return nil
}

func (t *txn) closeWanted(requesterID, wantedID domain.ID, status string) error {
	w, err := t.q.GetWantedForUpdate(t.ctx, wantedID)
	if !t.found(err) || w.RequesterID != requesterID {
		return domain.Fail("Permintaan tidak ditemukan.")
	}
	if status != "terpenuhi" && status != "ditutup" {
		return domain.Fail("Status tidak dikenal.")
	}
	t.check(t.q.SetWantedStatus(t.ctx, dbq.SetWantedStatusParams{ID: wantedID, Status: status}))
	t.touch(requesterID)
	t.touchPublic()
	return nil
}

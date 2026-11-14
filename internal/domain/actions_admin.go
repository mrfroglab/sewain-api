package domain

import (
	"fmt"
	"sort"
	"strings"
)

/* ------------------------------------------------------------- disputes */

type DisputeInput struct {
	CaseType    string `json:"caseType"`
	Description string `json:"description"`
	PhotoIDs    []ID   `json:"photoIds"`
}

// OpenDispute starts L1: 48h self-negotiation (PRD 11.2).
func (e *Engine) OpenDispute(actor, bookingID ID, in DisputeInput, wantID string) (ID, error) {
	b, err := e.partyBooking(actor, bookingID, "any")
	if err != nil {
		return "", err
	}
	if b.Status != "ACTIVE" && b.Status != "RETURNED" {
		return "", fail("Sengketa bisa dibuka saat sedang disewa, telat, atau setelah pengembalian.")
	}
	if b.DisputeID != nil {
		if d := e.dispute(*b.DisputeID); d != nil && d.Status != "selesai" {
			return "", fail("Sudah ada sengketa berjalan.")
		}
	}
	if strings.TrimSpace(in.Description) == "" {
		return "", fail("Ceritakan masalahnya.")
	}
	if in.PhotoIDs == nil {
		in.PhotoIDs = []ID{}
	}
	t := e.Now()
	id := pickID(wantID, "d", func(x ID) bool { return e.dispute(x) != nil })
	d := &Dispute{ID: id, BookingID: bookingID, OpenedBy: actor, CaseType: in.CaseType, Description: strings.TrimSpace(in.Description), PhotoIDs: in.PhotoIDs,
		CreatedAt: t, Layer: "L1", Status: "negosiasi", L1Deadline: t + 48*Hour, Proposals: []DisputeProposal{}}
	e.d.Disputes = append([]*Dispute{d}, e.d.Disputes...)
	e.touch("disputes", id)
	b.Status, b.DisputeID = "DISPUTED", ptr(id)
	e.touch("bookings", bookingID)
	other := b.RenterID
	if actor == b.RenterID {
		other = b.ProviderID
	}
	e.notify(other, "sengketa", "Ada laporan masalah", PublicName(e.user(actor))+" melaporkan masalah pada "+e.titleOf(b)+". Respons dalam 48 jam.", bookingHref(b)+"/sengketa")
	e.sysMsg(bookingID, "Laporan masalah dibuka. Tahap L1: negosiasi mandiri 48 jam. Ajukan solusi lewat halaman sengketa.")
	return id, nil
}

func (e *Engine) disputeForParty(actor, id ID) (*Dispute, *Booking, error) {
	d := e.dispute(id)
	if d == nil {
		return nil, nil, fail("Sengketa tidak ditemukan.")
	}
	b := e.booking(d.BookingID)
	if b == nil || !isParty(b, actor, "any") {
		return nil, nil, fail("Sengketa tidak ditemukan.")
	}
	return d, b, nil
}

type ProposalInput struct {
	Type   string  `json:"type"`
	Amount *int64  `json:"amount,omitempty"`
	Note   *string `json:"note,omitempty"`
}

var proposalLabel = map[string]string{
	"potong-deposit": "potong deposit", "ganti-perbaikan": "ganti biaya perbaikan", "ganti-nilai": "ganti rugi nilai barang",
	"perpanjang": "perpanjang waktu sewa", "tanpa-kompensasi": "tanpa kompensasi (wajar pakai)",
}

func (e *Engine) ProposeSolution(actor, id ID, in ProposalInput) error {
	d, b, err := e.disputeForParty(actor, id)
	if err != nil {
		return err
	}
	if d.Status != "negosiasi" {
		return fail("Negosiasi sudah ditutup.")
	}
	if proposalLabel[in.Type] == "" {
		return fail("Jenis solusi tidak dikenal.")
	}
	d.Proposals = append(d.Proposals, DisputeProposal{ID: NewID("pr"), ByID: actor, Type: in.Type, Amount: in.Amount, Note: in.Note, At: e.Now(), Status: "diajukan"})
	e.touch("disputes", id)
	other := b.RenterID
	if actor == b.RenterID {
		other = b.ProviderID
	}
	e.notify(other, "sengketa", "Usulan solusi baru", PublicName(e.user(actor))+" mengusulkan solusi untuk "+e.titleOf(b)+".", bookingHref(b)+"/sengketa")
	return nil
}

func (e *Engine) RespondProposal(actor, id, proposalID ID, accept bool) error {
	d, b, err := e.disputeForParty(actor, id)
	if err != nil {
		return err
	}
	var p *DisputeProposal
	for i := range d.Proposals {
		if d.Proposals[i].ID == proposalID {
			p = &d.Proposals[i]
		}
	}
	if p == nil {
		return fail("Usulan tidak ditemukan.")
	}
	if p.ByID == actor {
		return fail("Tidak bisa merespons usulan sendiri.")
	}
	if p.Status != "diajukan" || d.Status != "negosiasi" {
		return fail("Usulan sudah direspons.")
	}
	t := e.Now()
	if !accept {
		p.Status = "ditolak"
		e.touch("disputes", id)
		e.notify(p.ByID, "sengketa", "Usulan ditolak", e.titleOf(b)+": usulanmu ditolak. Ajukan usulan lain atau naikkan ke admin.", bookingHref(b)+"/sengketa")
		e.sysMsg(b.ID, "Usulan solusi ditolak.")
		return nil
	}
	p.Status = "diterima"
	outcome := "Disepakati di L1: " + proposalLabel[p.Type]
	if p.Amount != nil {
		outcome += " " + Rupiah(*p.Amount)
	}
	outcome += "."
	d.Status, d.Outcome, d.ResolvedAt = "selesai", ptr(outcome), ptr(t)
	e.touch("disputes", id)
	b.Status, b.ResolvedAt = "RESOLVED", ptr(t)
	e.touch("bookings", b.ID)
	e.notify(p.ByID, "sengketa", "Usulan diterima", e.titleOf(b)+": "+outcome, bookingHref(b)+"/sengketa")
	e.sysMsg(b.ID, "Sengketa selesai lewat negosiasi. "+outcome)
	return nil
}

func (e *Engine) EscalateDispute(actor, id ID) error {
	d, b, err := e.disputeForParty(actor, id)
	if err != nil {
		return err
	}
	if d.Status != "negosiasi" {
		return fail("Sengketa sudah di tahap mediasi.")
	}
	d.Layer, d.Status = "L2", "mediasi"
	e.touch("disputes", id)
	other := b.RenterID
	if actor == b.RenterID {
		other = b.ProviderID
	}
	e.notify(AdminID, "sengketa", "Sengketa menunggu mediasi", e.titleOf(b)+": dinaikkan ke L2 oleh "+PublicName(e.user(actor))+".", "/admin/sengketa/"+id)
	e.notify(other, "sengketa", "Sengketa naik ke mediasi admin", e.titleOf(b)+": admin akan memberi rekomendasi maksimal 5 hari kerja.", bookingHref(b)+"/sengketa")
	e.sysMsg(b.ID, "Sengketa dinaikkan ke mediasi admin (L2). Admin menilai foto, chat, jaminan, dan kesepakatan.")
	return nil
}

func (e *Engine) AdminRecommend(adminID, id ID, text string, amount *int64) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	d := e.dispute(id)
	if d == nil {
		return fail("Sengketa tidak ditemukan.")
	}
	b := e.booking(d.BookingID)
	if strings.TrimSpace(text) == "" {
		return fail("Tulis rekomendasinya.")
	}
	d.Layer, d.Status = "L2", "mediasi"
	d.Recommendation = &Recommendation{Text: strings.TrimSpace(text), Amount: amount, ByAdminID: adminID, At: e.Now()}
	e.touch("disputes", id)
	if b != nil {
		for _, u := range []ID{b.RenterID, b.ProviderID} {
			e.notify(u, "sengketa", "Rekomendasi admin keluar", e.titleOf(b)+": "+truncate(text, 90), bookingHref(b)+"/sengketa")
		}
		msg := "Rekomendasi admin: " + text
		if amount != nil {
			msg += " (" + Rupiah(*amount) + ")"
		}
		e.sysMsg(b.ID, msg)
		e.audit(adminID, "sengketa.rekomendasi", e.titleOf(b)+" ("+b.Code+")", nil, ptr(text))
	}
	return nil
}

func (e *Engine) ResolveDispute(adminID, id ID, outcome string) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	d := e.dispute(id)
	if d == nil {
		return fail("Sengketa tidak ditemukan.")
	}
	if strings.TrimSpace(outcome) == "" {
		return fail("Tulis hasil akhirnya.")
	}
	t := e.Now()
	d.Status, d.Outcome, d.ResolvedAt = "selesai", ptr(strings.TrimSpace(outcome)), ptr(t)
	e.touch("disputes", id)
	if b := e.booking(d.BookingID); b != nil {
		b.Status, b.ResolvedAt = "RESOLVED", ptr(t)
		e.touch("bookings", b.ID)
		for _, u := range []ID{b.RenterID, b.ProviderID} {
			e.notify(u, "sengketa", "Sengketa selesai", e.titleOf(b)+": "+truncate(outcome, 100), bookingHref(b)+"/sengketa")
		}
		e.sysMsg(b.ID, "Sengketa ditutup admin. Hasil: "+outcome)
		e.audit(adminID, "sengketa.selesai", e.titleOf(b)+" ("+b.Code+")", nil, ptr(outcome))
	}
	return nil
}

/* ----------------------------------------------------------- moderation */

type ReportInput struct {
	TargetType string  `json:"targetType"`
	TargetID   ID      `json:"targetId"`
	Reason     string  `json:"reason"`
	Detail     *string `json:"detail,omitempty"`
}

func (e *Engine) CreateReport(reporterID ID, in ReportInput, wantID string) (ID, error) {
	if in.TargetType != "listing" && in.TargetType != "user" {
		return "", fail("Target laporan tidak dikenal.")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return "", fail("Pilih alasan laporan.")
	}
	id := pickID(wantID, "rp", func(x ID) bool {
		for _, r := range e.d.Reports {
			if r.ID == x {
				return true
			}
		}
		return false
	})
	r := &Report{ID: id, ReporterID: reporterID, TargetType: in.TargetType, TargetID: in.TargetID, Reason: in.Reason, Detail: in.Detail, CreatedAt: e.Now(), Status: "baru"}
	e.d.Reports = append([]*Report{r}, e.d.Reports...)
	e.touch("reports", id)
	kind := "Pengguna"
	if in.TargetType == "listing" {
		kind = "Listing"
	}
	e.notify(AdminID, "sistem", "Laporan baru", kind+" dilaporkan: "+in.Reason, "/admin/laporan")
	return id, nil
}

func (e *Engine) ResolveReport(adminID, reportID ID, status, resolution string) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	if status != "ditindak" && status != "ditolak" {
		return fail("Status laporan tidak dikenal.")
	}
	for _, r := range e.d.Reports {
		if r.ID == reportID {
			r.Status, r.Resolution, r.ResolvedAt, r.ResolvedBy = status, ptr(resolution), ptr(e.Now()), ptr(adminID)
			e.touch("reports", r.ID)
			e.audit(adminID, "laporan."+status, r.Reason, nil, ptr(resolution))
			e.notify(r.ReporterID, "sistem", "Laporanmu sudah ditinjau", resolution, "")
			return nil
		}
	}
	return fail("Laporan tidak ditemukan.")
}

type StrikeInput struct {
	Reason    string `json:"reason"`
	Severity  string `json:"severity"`
	BookingID *ID    `json:"bookingId,omitempty"`
}

func (e *Engine) AddStrike(adminID, userID ID, in StrikeInput) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	u := e.user(userID)
	if u == nil {
		return fail("Pengguna tidak ditemukan.")
	}
	if in.Severity != "berat" {
		in.Severity = "ringan"
	}
	add := 1
	if in.Severity == "berat" {
		add = 2
	}
	s := &Strike{ID: NewID("s"), UserID: userID, Reason: in.Reason, Severity: in.Severity, BookingID: in.BookingID, At: e.Now(), ByAdminID: adminID}
	e.d.Strikes = append([]*Strike{s}, e.d.Strikes...)
	e.touch("strikes", s.ID)
	old := u.Strikes
	u.Strikes += add
	e.touch("users", userID)
	e.notify(userID, "sistem", "Kamu mendapat strike", in.Reason+". Strike berulang bisa berujung suspend.", "/profil")
	e.audit(adminID, "user.strike", PublicName(u), ptr(fmt.Sprint(old)), ptr(fmt.Sprintf("%d (%s: %s)", u.Strikes, in.Severity, in.Reason)))
	return nil
}

// SetUserStatus: L3 consequences — suspend / blacklist (NIK-based) / restore. Listings of blocked users are paused.
func (e *Engine) SetUserStatus(adminID, userID ID, status, reason string) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	u := e.user(userID)
	if u == nil {
		return fail("Pengguna tidak ditemukan.")
	}
	if status != "aktif" && status != "suspend" && status != "blacklist" {
		return fail("Status tidak dikenal.")
	}
	old := u.Status
	u.Status = status
	e.touch("users", userID)
	if status != "aktif" {
		for _, l := range e.d.Listings {
			if l.OwnerID == userID && l.Status == "aktif" {
				l.Status = "jeda"
				e.touch("listings", l.ID)
			}
		}
	}
	title := map[string]string{"aktif": "Akun dipulihkan", "suspend": "Akun ditangguhkan", "blacklist": "Akun diblokir"}[status]
	e.notify(userID, "sistem", title, reason, "/profil")
	nv := status + " — " + reason
	if status == "blacklist" {
		nv += " (NIK " + deref(u.Verification.NikMasked, "-") + ")"
	}
	e.audit(adminID, "user."+status, PublicName(u), ptr(old), ptr(nv))
	return nil
}

/* ------------------------------------------------------------- settings */

type CommissionPatch struct {
	Active             *bool    `json:"active,omitempty"`
	Mode               *string  `json:"mode,omitempty"`
	Percent            *float64 `json:"percent,omitempty"`
	Flat               *int64   `json:"flat,omitempty"`
	Min                *int64   `json:"min"`
	Max                *int64   `json:"max"`
	Bearer             *string  `json:"bearer,omitempty"`
	SplitRenterPercent *float64 `json:"splitRenterPercent,omitempty"`
	HasMin             bool     `json:"-"`
	HasMax             bool     `json:"-"`
}

func fmtCommission(c CommissionConfig) string {
	state := "nonaktif"
	if c.Active {
		state = "aktif"
	}
	rate := Rupiah(c.Flat)
	if c.Mode == "persen" {
		rate = fmtPercent(c.Percent) + "%"
	}
	opt := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return Rupiah(*p)
	}
	s := fmt.Sprintf("%s, %s, min %s, maks %s, %s", state, rate, opt(c.Min), opt(c.Max), c.Bearer)
	if c.Bearer == "split" {
		s += fmt.Sprintf(" %s:%s", fmtPercent(c.SplitRenterPercent), fmtPercent(100-c.SplitRenterPercent))
	}
	return s
}

// UpdateCommission (FR-K1..K7): applies to NEW bookings only — agreements keep their snapshot (FR-K5). Audited.
func (e *Engine) UpdateCommission(adminID ID, p CommissionPatch) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	old := e.d.Commission
	c := old
	if p.Active != nil {
		c.Active = *p.Active
	}
	if p.Mode != nil && (*p.Mode == "persen" || *p.Mode == "flat") {
		c.Mode = *p.Mode
	}
	if p.Percent != nil {
		c.Percent = *p.Percent
	}
	if p.Flat != nil {
		c.Flat = *p.Flat
	}
	if p.HasMin {
		c.Min = p.Min
	}
	if p.HasMax {
		c.Max = p.Max
	}
	if p.Bearer != nil && contains([]string{"penyewa", "penyedia", "split"}, *p.Bearer) {
		c.Bearer = *p.Bearer
	}
	if p.SplitRenterPercent != nil {
		c.SplitRenterPercent = *p.SplitRenterPercent
	}
	if c.Percent < 0 || c.Percent > 100 || c.Flat < 0 || c.SplitRenterPercent < 0 || c.SplitRenterPercent > 100 {
		return fail("Nilai komisi tidak valid.")
	}
	c.UpdatedAt, c.UpdatedBy = e.Now(), adminID
	e.d.Commission = c
	e.settings = true
	e.audit(adminID, "komisi.ubah", "Pengaturan komisi", ptr(fmtCommission(old)), ptr(fmtCommission(c)))
	return nil
}

type CollateralRulesPatch struct {
	LowMax                *int64   `json:"lowMax,omitempty"`
	MidMax                *int64   `json:"midMax,omitempty"`
	MidDepositPercent     *float64 `json:"midDepositPercent,omitempty"`
	HighDepositPercent    *float64 `json:"highDepositPercent,omitempty"`
	VehicleDepositPercent *float64 `json:"vehicleDepositPercent,omitempty"`
	HighValueBadge        *int64   `json:"highValueBadge,omitempty"`
}

func (e *Engine) UpdateCollateralRules(adminID ID, p CollateralRulesPatch) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	old := e.d.CollateralRules
	r := old
	if p.LowMax != nil {
		r.LowMax = *p.LowMax
	}
	if p.MidMax != nil {
		r.MidMax = *p.MidMax
	}
	if p.MidDepositPercent != nil {
		r.MidDepositPercent = *p.MidDepositPercent
	}
	if p.HighDepositPercent != nil {
		r.HighDepositPercent = *p.HighDepositPercent
	}
	if p.VehicleDepositPercent != nil {
		r.VehicleDepositPercent = *p.VehicleDepositPercent
	}
	if p.HighValueBadge != nil {
		r.HighValueBadge = *p.HighValueBadge
	}
	if r.LowMax <= 0 || r.MidMax <= r.LowMax {
		return fail("Ambang nilai tidak valid: batas sedang harus di atas batas rendah.")
	}
	r.UpdatedAt, r.UpdatedBy = e.Now(), adminID
	e.d.CollateralRules = r
	e.settings = true
	f := func(x CollateralRules) string {
		return fmt.Sprintf("<%s identitas; ≤%s +%s%%; >%s %s%%; kendaraan %s%%", Rupiah(x.LowMax), Rupiah(x.MidMax), fmtPercent(x.MidDepositPercent), Rupiah(x.MidMax), fmtPercent(x.HighDepositPercent), fmtPercent(x.VehicleDepositPercent))
	}
	e.audit(adminID, "jaminan.ubah", "Saran jaminan", ptr(f(old)), ptr(f(r)))
	return nil
}

func (e *Engine) SetBannedKeywords(adminID ID, words []string) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if w != "" && !seen[w] {
			seen[w] = true
			clean = append(clean, w)
		}
	}
	sort.Strings(clean)
	old := len(e.d.BannedKeywords)
	e.d.BannedKeywords = clean
	e.settings = true
	e.audit(adminID, "kata-terlarang.ubah", "Daftar kata kunci larangan", ptr(fmt.Sprintf("%d kata", old)), ptr(fmt.Sprintf("%d kata", len(clean))))
	return nil
}

/* ---------------------------------------------------------------- wanted */

type WantedInput struct {
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	Category     *string `json:"category,omitempty"`
	AreaID       string  `json:"areaId"`
	RadiusKm     float64 `json:"radiusKm"`
	NeededFrom   *string `json:"neededFrom,omitempty"`
	NeededUntil  *string `json:"neededUntil,omitempty"`
	BudgetPerDay *int64  `json:"budgetPerDay,omitempty"`
}

// CreateWanted (P1 papan permintaan): demand becomes a supply signal — nearby providers in that category are notified.
func (e *Engine) CreateWanted(requesterID ID, in WantedInput, wantID string) (ID, error) {
	if err := e.canTransact(requesterID); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Title) == "" {
		return "", fail("Tulis barang yang kamu cari.")
	}
	if words := BannedWordsIn(in.Title+" "+in.Description, e.d.BannedKeywords); len(words) > 0 {
		return "", fail("Ada kata yang dilarang: " + strings.Join(words, ", ") + ".")
	}
	area, ok := Areas[in.AreaID]
	if !ok {
		area = Areas["ugm"]
	}
	if in.RadiusKm <= 0 {
		in.RadiusKm = 3
	}
	id := pickID(wantID, "w", func(x ID) bool {
		for _, w := range e.d.Wanted {
			if w.ID == x {
				return true
			}
		}
		return false
	})
	w := &WantedPost{ID: id, RequesterID: requesterID, Title: strings.TrimSpace(in.Title), Description: in.Description, Category: in.Category, AreaID: area.ID,
		Location: GeoPoint{Lat: area.Lat, Lng: area.Lng}, RadiusKm: in.RadiusKm, NeededFrom: in.NeededFrom, NeededUntil: in.NeededUntil, BudgetPerDay: in.BudgetPerDay,
		CreatedAt: e.Now(), Status: "terbuka", Offers: []WantedOffer{}}
	e.d.Wanted = append([]*WantedPost{w}, e.d.Wanted...)
	e.touch("wanted", id)
	notified := map[ID]bool{}
	for _, l := range e.d.Listings {
		if l.Status == "aktif" && l.OwnerID != requesterID && (in.Category == nil || l.Category == *in.Category) && !notified[l.OwnerID] {
			notified[l.OwnerID] = true
			e.notify(l.OwnerID, "papan", "Ada yang mencari barang", "\""+w.Title+"\" di sekitar "+area.Name+". Punya? Tawarkan sekarang.", "/papan")
		}
	}
	return id, nil
}

func (e *Engine) wantedPost(id ID) *WantedPost {
	for _, w := range e.d.Wanted {
		if w.ID == id {
			return w
		}
	}
	return nil
}

func (e *Engine) OfferWanted(providerID, wantedID ID, listingID *ID, message string) error {
	w := e.wantedPost(wantedID)
	if w == nil || w.Status != "terbuka" {
		return fail("Permintaan sudah ditutup.")
	}
	if w.RequesterID == providerID {
		return fail("Ini permintaanmu sendiri.")
	}
	if err := e.canTransact(providerID); err != nil {
		return err
	}
	w.Offers = append(w.Offers, WantedOffer{ID: NewID("wo"), ProviderID: providerID, ListingID: listingID, Message: message, At: e.Now()})
	e.touch("wanted", wantedID)
	href := "/papan"
	if listingID != nil {
		href = "/barang/" + *listingID
	}
	e.notify(w.RequesterID, "papan", "Ada yang menawarkan barang", PublicName(e.user(providerID))+" merespons \""+w.Title+"\".", href)
	return nil
}

func (e *Engine) CloseWanted(requesterID, wantedID ID, status string) error {
	w := e.wantedPost(wantedID)
	if w == nil || w.RequesterID != requesterID {
		return fail("Permintaan tidak ditemukan.")
	}
	if status != "terpenuhi" && status != "ditutup" {
		return fail("Status tidak dikenal.")
	}
	w.Status = status
	e.touch("wanted", wantedID)
	return nil
}

/* ---------------------------------------------------------------- photos */

// AddPhoto registers evidence created by the upload endpoint (server-side SHA-256 + timestamp, FR-P2/P3).
// Photos are append-only (FR-P4): there is no update or delete.
func (e *Engine) AddPhoto(p *PhotoRecord) {
	e.d.Photos = append(e.d.Photos, p)
	e.touch("photos", p.ID)
}

func (e *Engine) Photo(id ID) *PhotoRecord {
	for _, p := range e.d.Photos {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// CanSeePhoto: KTP/selfie only for the owner and admins; booking evidence for the parties and admins (FR-J5, PRD 12.4).
func (e *Engine) CanSeePhoto(p *PhotoRecord, viewer *User) bool {
	if p.Stage == "ktp" || p.Stage == "selfie" {
		return viewer != nil && (viewer.ID == p.UploaderID || viewer.Role == "admin")
	}
	if p.Stage == "jaminan" && p.BookingID != nil {
		b := e.booking(*p.BookingID)
		return viewer != nil && (viewer.Role == "admin" || (b != nil && isParty(b, viewer.ID, "any")))
	}
	return true
}

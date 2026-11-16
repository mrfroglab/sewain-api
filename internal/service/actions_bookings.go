package service

import (
	"fmt"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// committedOverlaps counts bookings holding stock on overlapping dates (FR-R4), including a DISPUTED one whose item
// is still out. The caller holds the listing lock, so concurrent requests/acceptances for the same listing are
// serialized and see each other's commits.
func (t *txn) committedOverlaps(listingID, excludeID domain.ID, start, end string) int {
	n, err := t.q.CountCommittedOverlaps(t.ctx, dbq.CountCommittedOverlapsParams{ListingID: listingID, ExcludeID: excludeID,
		StartDate: dateOf(start), EndDate: dateOf(end)})
	t.check(err)
	return n
}

// createBooking: request → agreement snapshot (hashed, FR-R2) → REQUESTED with a 24h expiry (FR-R3).
// Prevents schedule clashes with stock awareness (FR-R4). Auto-approval only for trusted renters and never
// for high-value items or vehicles (PRD 11.4).
func (t *txn) createBooking(renterID domain.ID, in domain.BookingInput, wantID string) (domain.ID, error) {
	renter := t.user(renterID)
	if err := domain.CanTransact(renter); err != nil {
		return "", err
	}
	l := t.listingForUpdate(in.ListingID)
	if l == nil || l.Status != "aktif" {
		return "", domain.Fail("Barang tidak tersedia.")
	}
	if l.OwnerID == renterID {
		return "", domain.Fail("Tidak bisa menyewa barang sendiri.")
	}
	owner := t.user(l.OwnerID)
	if _, err := domain.ParseIsoDate(in.StartDate); err != nil {
		return "", domain.Fail("Tanggal ambil tidak valid.")
	}
	if _, err := domain.ParseIsoDate(in.EndDate); err != nil {
		return "", domain.Fail("Tanggal kembali tidak valid.")
	}
	if in.EndDate < in.StartDate {
		return "", domain.Fail("Tanggal kembali harus setelah tanggal ambil.")
	}
	if in.StartDate < domain.IsoDate(t.now) {
		return "", domain.Fail("Tanggal ambil sudah lewat.")
	}
	if in.EndDate > domain.AddDays(domain.IsoDate(t.now), domain.MaxRentalDays) {
		return "", domain.Fail(fmt.Sprintf("Tanggal sewa maksimal %d hari dari sekarang.", domain.MaxRentalDays))
	}
	var collateral *domain.CollateralOption
	for i := range l.CollateralOptions {
		if l.CollateralOptions[i].ID == in.CollateralOptionID {
			collateral = &l.CollateralOptions[i]
		}
	}
	if collateral == nil {
		return "", domain.Fail("Pilih bentuk jaminan dari opsi penyedia.")
	}
	if !domain.Contains(l.Handover, in.Handover) {
		return "", domain.Fail("Metode serah terima tidak tersedia.")
	}
	if err := domain.CheckLenPtr("Catatan untuk penyedia", in.Note, domain.MaxNoteLen); err != nil {
		return "", err
	}
	quote := domain.QuoteRent(l, in.StartDate, in.EndDate)
	if l.MinDays != nil && quote.Days < *l.MinDays {
		return "", domain.Fail(fmt.Sprintf("Minimal sewa %d hari.", *l.MinDays))
	}
	if l.MaxDays != nil && quote.Days > *l.MaxDays {
		return "", domain.Fail(fmt.Sprintf("Maksimal sewa %d hari.", *l.MaxDays))
	}
	if t.committedOverlaps(l.ID, "", in.StartDate, in.EndDate) >= l.Stock {
		return "", domain.Fail("Barang sudah dipesan di tanggal itu. Coba tanggal lain.")
	}
	ag := domain.BuildAgreement(t.now, l, renter, owner, in, *collateral, t.commission())
	ag.RenterAgreedAt = domain.Ptr(t.now)
	n, err := t.q.NextBookingNumber(t.ctx)
	t.check(err)
	id := t.pickID(wantID, "b", t.q.BookingExists)
	b := &domain.Booking{
		ID: id, Code: domain.BookingCode(t.now, n), ListingID: l.ID, RenterID: renterID, ProviderID: l.OwnerID,
		StartDate: in.StartDate, EndDate: in.EndDate, Handover: in.Handover, Collateral: *collateral, Note: domain.CleanNote(in.Note),
		Status: "REQUESTED", CreatedAt: t.now, ExpiresAt: t.now + domain.RequestTTL, Agreement: ag, Payment: domain.Payment(ag),
	}
	t.insertBooking(b)
	t.notify(l.OwnerID, "booking", "Request sewa baru", fmt.Sprintf("%s ingin menyewa %s (%d hari).", domain.PublicName(renter), l.Title, quote.Days), domain.BookingHref(b))
	t.sysMsg(id, "Request sewa dikirim. Penyedia punya waktu 24 jam untuk merespons.")
	rating := domain.Deref(renter.BaseRating, 0)
	if l.Approval == "otomatis" && !domain.NeedsManualApproval(l, t.collateralRules()) && rating >= domain.Deref(l.AutoApproveMinRating, 4.5) {
		if err := t.acceptBooking(l.OwnerID, id, nil); err == nil {
			t.sysMsg(id, fmt.Sprintf("Disetujui otomatis: rating penyewa %.1f memenuhi ambang penyedia.", rating))
		}
	}
	return id, nil
}

func (t *txn) acceptBooking(providerID, id domain.ID, meeting *string) error {
	b, err := t.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Status != "REQUESTED" {
		return domain.Fail("Request ini sudah tidak menunggu persetujuan.")
	}
	if t.now >= b.ExpiresAt { // FR-R3 holds even before the periodic job marks it EXPIRED
		return domain.Fail("Request ini sudah kedaluwarsa.")
	}
	l := t.listingForUpdate(b.ListingID)
	stock := 1
	if l != nil {
		stock = l.Stock
	}
	if t.committedOverlaps(b.ListingID, b.ID, b.StartDate, b.EndDate) >= stock {
		return domain.Fail("Stok barang di tanggal itu sudah terpakai booking lain. Tolak request ini atau minta penyewa ganti tanggal.")
	}
	b.Status, b.AcceptedAt = "ACCEPTED", domain.Ptr(t.now)
	b.Agreement.ProviderAgreedAt = domain.Ptr(t.now)
	detail := "Atur titik temu lewat chat (disarankan tempat publik)."
	if meeting != nil && strings.TrimSpace(*meeting) != "" {
		detail = strings.TrimSpace(*meeting)
	} else if l != nil && l.MeetingPoint != nil {
		detail = *l.MeetingPoint
	}
	b.MeetingPointDetail = domain.Ptr(detail)
	t.saveBooking(b)
	t.notify(b.RenterID, "booking", "Request disetujui", domain.PublicName(t.user(providerID))+" menyetujui sewa "+t.titleOf(b)+". Atur serah terima lewat chat.", domain.BookingHref(b))
	t.sysMsg(id, "Request disetujui. Kesepakatan sewa sudah disetujui kedua pihak, dan detail titik temu sekarang terlihat.")
	return nil
}

func (t *txn) declineBooking(providerID, id domain.ID, reason string) error {
	b, err := t.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Status != "REQUESTED" {
		return domain.Fail("Request ini sudah tidak menunggu persetujuan.")
	}
	reason = strings.TrimSpace(reason)
	if err := domain.CheckLen("Alasan penolakan", reason, domain.MaxReasonLen); err != nil {
		return err
	}
	if reason == "" {
		reason = "Tidak ada alasan."
	}
	b.Status, b.DeclinedAt, b.DeclineReason = "DECLINED", domain.Ptr(t.now), domain.Ptr(reason)
	t.saveBooking(b)
	t.notify(b.RenterID, "booking", "Request ditolak", t.titleOf(b)+": "+reason, domain.BookingHref(b))
	t.sysMsg(id, "Request ditolak penyedia. Alasan: "+reason)
	return nil
}

func (t *txn) cancelBooking(actor, id domain.ID, reason string) error {
	b, err := t.partyBooking(actor, id, "any")
	if err != nil {
		return err
	}
	if b.Status != "REQUESTED" && b.Status != "ACCEPTED" {
		return domain.Fail("Booking hanya bisa dibatalkan sebelum serah terima.")
	}
	reason = strings.TrimSpace(reason)
	if err := domain.CheckLen("Alasan pembatalan", reason, domain.MaxReasonLen); err != nil {
		return err
	}
	if reason == "" {
		reason = "-"
	}
	other, who := b.RenterID, "penyedia"
	if actor == b.RenterID {
		other, who = b.ProviderID, "penyewa"
	}
	b.Status, b.CancelledAt, b.CancelledBy, b.CancelReason = "CANCELLED", domain.Ptr(t.now), domain.Ptr(actor), domain.Ptr(reason)
	t.saveBooking(b)
	t.notify(other, "booking", "Booking dibatalkan", domain.PublicName(t.user(actor))+" membatalkan "+t.titleOf(b)+". Alasan: "+reason, domain.BookingHref(b))
	t.sysMsg(id, "Booking dibatalkan oleh "+who+". Alasan: "+reason)
	return nil
}

func (t *txn) payBooking(renterID, id domain.ID, method string) error {
	b, err := t.partyBooking(renterID, id, "renter")
	if err != nil {
		return err
	}
	if b.Status != "ACCEPTED" && b.Status != "ACTIVE" {
		return domain.Fail("Pembayaran tersedia setelah request disetujui.")
	}
	if b.Payment.Status == "lunas" {
		return domain.Fail("Sudah lunas.")
	}
	switch t.s.opt.PaymentMode {
	case PaymentOffline:
		// Pilot (PRD §9.3): no money moves through Sewain. The renter only records paying the provider directly.
		if method != "langsung" {
			return domain.Fail("Pembayaran lewat Sewain belum tersedia. Bayar langsung ke penyedia, lalu tandai \"Sudah bayar langsung\".")
		}
		b.Payment.Status, b.Payment.Method, b.Payment.PaidAt = "lunas", domain.Ptr(method), domain.Ptr(t.now)
		t.saveBooking(b)
		t.notify(b.ProviderID, "booking", "Penyewa menandai sudah bayar", t.titleOf(b)+": penyewa menandai sudah bayar langsung ke kamu. Cek dulu, lalu konfirmasi.", domain.BookingHref(b))
		t.sysMsg(id, "Penyewa menandai sudah bayar langsung ke penyedia.")
		return nil
	case PaymentGateway:
		return domain.Fail("Pembayaran online belum tersedia.")
	}
	if !domain.Contains([]string{"qris", "va", "ewallet", "tunai"}, method) {
		method = "qris"
	}
	b.Payment.Status, b.Payment.Method, b.Payment.PaidAt = "lunas", domain.Ptr(method), domain.Ptr(t.now)
	t.saveBooking(b)
	t.notify(b.ProviderID, "booking", "Pembayaran diterima (simulasi)", t.titleOf(b)+": "+domain.Rupiah(b.Payment.Total)+" lunas via Sewain.", domain.BookingHref(b))
	t.sysMsg(id, "Pembayaran "+domain.Rupiah(b.Payment.Total)+" ditandai lunas (simulasi "+strings.ToUpper(method)+").")
	return nil
}

// confirmDirectPayment: the provider confirms receiving a direct payment the renter marked (PAYMENT_MODE=offline).
func (t *txn) confirmDirectPayment(providerID, id domain.ID) error {
	b, err := t.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Payment.Status != "lunas" || domain.Deref(b.Payment.Method, "") != "langsung" {
		return domain.Fail("Penyewa belum menandai sudah bayar langsung.")
	}
	if b.Payment.ConfirmedAt != nil {
		return domain.Fail("Pembayaran sudah dikonfirmasi.")
	}
	b.Payment.ConfirmedAt = domain.Ptr(t.now)
	t.saveBooking(b)
	t.notify(b.RenterID, "booking", "Pembayaran dikonfirmasi", t.titleOf(b)+": penyedia sudah menerima pembayaranmu.", domain.BookingHref(b))
	t.sysMsg(id, "Penyedia mengonfirmasi pembayaran langsung sudah diterima.")
	return nil
}

// submitCheckout: ≥3 condition photos + a collateral photo (FR-J5); vehicles also odometer + fuel (PRD 12.3).
func (t *txn) submitCheckout(actor, id domain.ID, in domain.CheckoutInput) error {
	b := t.bookingForUpdate(id)
	if b == nil || b.ProviderID != actor {
		return domain.Fail("Hanya penyedia yang memotret serah terima.")
	}
	if b.Status != "ACCEPTED" {
		return domain.Fail("Serah terima hanya untuk booking yang sudah disetujui.")
	}
	if len(in.PhotoIDs) < 3 {
		return domain.Fail("Minimal 3 foto kondisi barang.")
	}
	if len(in.CollateralPhotoIDs) < 1 {
		return domain.Fail("Foto jaminan wajib (FR-J5).")
	}
	if err := checkMeters(in.Odometer, in.Fuel); err != nil {
		return err
	}
	if all := append(append([]domain.ID{}, in.PhotoIDs...), in.CollateralPhotoIDs...); !t.photosExist(all) {
		return domain.Fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	} else if !t.evidenceOf(b, actor, all) {
		return domain.Fail("Foto bukti harus diunggah untuk booking ini.")
	}
	l := t.listing(b.ListingID)
	if l != nil && domain.IsVehicle(l.Category) && (domain.Deref(in.Odometer, "") == "" || domain.Deref(in.Fuel, "") == "") {
		return domain.Fail("Kendaraan: isi odometer dan indikator BBM.")
	}
	b.Checkout = &domain.Checkout{PhotoIDs: in.PhotoIDs, CollateralPhotoIDs: in.CollateralPhotoIDs, Odometer: in.Odometer, Fuel: in.Fuel, SubmittedAt: t.now, ProviderAgreedAt: domain.Ptr(t.now)}
	t.saveBooking(b)
	t.notify(b.RenterID, "booking", "Cek kondisi barang", domain.PublicName(t.user(actor))+" sudah memotret "+t.titleOf(b)+" + jaminan. Periksa lalu tekan \"Setuju kondisi\".", domain.BookingHref(b)+"/serah-terima")
	t.sysMsg(id, fmt.Sprintf("Foto check-out diunggah (%d foto barang + %d foto jaminan). Menunggu penyewa menyetujui kondisi.", len(in.PhotoIDs), len(in.CollateralPhotoIDs)))
	return nil
}

func (t *txn) addCheckoutPhotos(actor, id domain.ID, photoIDs []domain.ID) error {
	b, err := t.partyBooking(actor, id, "any")
	if err != nil || b.Checkout == nil {
		return domain.Fail("Foto check-out belum ada.")
	}
	if b.Status != "ACCEPTED" {
		return domain.Fail("Serah terima sudah selesai.")
	}
	if !t.photosExist(photoIDs) {
		return domain.Fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	}
	if !t.evidenceOf(b, actor, photoIDs) {
		return domain.Fail("Foto bukti harus diunggah untuk booking ini.")
	}
	b.Checkout.PhotoIDs = append(b.Checkout.PhotoIDs, photoIDs...)
	b.Checkout.RenterAgreedAt = nil
	t.saveBooking(b)
	who := "Penyedia"
	if actor == b.RenterID {
		who = "Penyewa"
	}
	t.sysMsg(id, fmt.Sprintf("%s menambah %d foto kondisi.", who, len(photoIDs)))
	return nil
}

// agreeCheckout: ACTIVE starts only after BOTH parties agree on the item + collateral condition.
func (t *txn) agreeCheckout(actor, id domain.ID) error {
	b, err := t.partyBooking(actor, id, "any")
	if err != nil || b.Checkout == nil {
		return domain.Fail("Foto check-out belum ada.")
	}
	if b.Status != "ACCEPTED" {
		return domain.Fail("Serah terima sudah selesai.")
	}
	if actor == b.RenterID {
		b.Checkout.RenterAgreedAt = domain.Ptr(t.now)
	} else {
		b.Checkout.ProviderAgreedAt = domain.Ptr(t.now)
	}
	started := b.Checkout.RenterAgreedAt != nil && b.Checkout.ProviderAgreedAt != nil
	if started {
		b.Status, b.ActiveAt = "ACTIVE", domain.Ptr(t.now)
	}
	t.saveBooking(b)
	if started {
		t.notify(b.RenterID, "booking", "Sewa dimulai", t.titleOf(b)+" resmi kamu sewa sampai "+b.EndDate+". Jaga baik-baik, ya!", domain.BookingHref(b))
		t.notify(b.ProviderID, "booking", "Sewa dimulai", t.titleOf(b)+" sudah diserahkan. Jaminan tercatat.", domain.BookingHref(b))
		t.sysMsg(id, "Serah terima selesai. Kedua pihak menyetujui kondisi barang dan jaminan. Status: Sedang disewa.")
	}
	return nil
}

func (t *txn) rejectAtCheckout(renterID, id domain.ID, note string) error {
	b, err := t.partyBooking(renterID, id, "renter")
	if err != nil {
		return err
	}
	if b.Status != "ACCEPTED" {
		return domain.Fail("Hanya saat serah terima.")
	}
	if err := domain.CheckLen("Catatan", note, domain.MaxNoteLen); err != nil {
		return err
	}
	t.check(t.q.InsertReport(t.ctx, dbq.InsertReportParams{ID: domain.NewID("rp"), ReporterID: renterID, TargetType: "listing", TargetID: b.ListingID,
		Reason: "Barang tidak sesuai deskripsi (saat serah terima)", Detail: domain.Ptr(note), Status: "baru", CreatedAt: tsOf(t.now)}))
	b.Status, b.CancelledAt, b.CancelledBy, b.CancelReason = "CANCELLED", domain.Ptr(t.now), domain.Ptr(renterID), domain.Ptr("Barang tidak sesuai deskripsi: "+note)
	t.saveBooking(b)
	t.notify(b.ProviderID, "booking", "Penyewa membatalkan saat serah terima", t.titleOf(b)+" dilaporkan tidak sesuai deskripsi. Admin akan meninjau.", domain.BookingHref(b))
	t.notify(domain.AdminID, "sistem", "Laporan: barang tidak sesuai", t.titleOf(b)+" dilaporkan saat serah terima.", "/admin/laporan")
	t.sysMsg(id, "Penyewa membatalkan tanpa penalti karena barang tidak sesuai deskripsi. Laporan diteruskan ke admin.")
	return nil
}

func (t *txn) submitCheckin(actor, id domain.ID, in domain.CheckinInput) error {
	b := t.bookingForUpdate(id)
	if b == nil || b.ProviderID != actor {
		return domain.Fail("Hanya penyedia yang memotret pengembalian.")
	}
	if b.Status != "ACTIVE" {
		return domain.Fail("Pengembalian hanya untuk booking yang sedang disewa.")
	}
	if len(in.PhotoIDs) < 3 {
		return domain.Fail("Minimal 3 foto kondisi saat kembali.")
	}
	if in.Result != "sesuai" && in.Result != "masalah" {
		return domain.Fail("Pilih: kondisi sesuai atau ada masalah.")
	}
	if in.Result == "masalah" && (domain.Deref(in.ProblemType, "") == "" || len(in.ProblemPhotoIDs) == 0) {
		return domain.Fail("Pilih jenis masalah dan lampirkan foto masalahnya.")
	}
	if err := domain.FirstErr(checkMeters(in.Odometer, in.Fuel), domain.CheckLenPtr("Jenis masalah", in.ProblemType, domain.MaxShortLen),
		domain.CheckLenPtr("Catatan masalah", in.ProblemNote, domain.MaxNoteLen)); err != nil {
		return err
	}
	all := append(append([]domain.ID{}, in.PhotoIDs...), in.ProblemPhotoIDs...)
	if !t.photosExist(all) {
		return domain.Fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	}
	if !t.evidenceOf(b, actor, all) {
		return domain.Fail("Foto bukti harus diunggah untuk booking ini.")
	}
	b.Status, b.ReturnedAt = "RETURNED", domain.Ptr(t.now)
	b.Checkin = &domain.Checkin{PhotoIDs: all, Result: in.Result, ProblemType: in.ProblemType, ProblemNote: in.ProblemNote, Odometer: in.Odometer, Fuel: in.Fuel, At: t.now}
	t.saveBooking(b)
	cond := "sesuai"
	if in.Result == "masalah" {
		cond = "ada masalah (" + domain.Deref(in.ProblemType, "") + ")"
	}
	t.notify(b.RenterID, "booking", "Pengembalian dicatat", t.titleOf(b)+" sudah diterima penyedia. Kondisi: "+cond+".", domain.BookingHref(b))
	t.sysMsg(id, fmt.Sprintf("Pengembalian dicatat (%d foto). Kondisi: %s.", len(in.PhotoIDs), strings.Replace(cond, "ada masalah (", "ada masalah: ", 1)))
	if in.Result == "masalah" {
		pt := domain.Deref(in.ProblemType, "")
		desc := pt
		if in.ProblemNote != nil && *in.ProblemNote != "" {
			desc += " — " + *in.ProblemNote
		}
		_, _ = t.openDispute(actor, id, domain.DisputeInput{CaseType: domain.CaseTypeFor(pt), Description: desc, PhotoIDs: in.ProblemPhotoIDs}, "")
	}
	return nil
}

func (t *txn) returnCollateral(providerID, id domain.ID) error {
	b, err := t.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Status != "RETURNED" && b.Status != "RESOLVED" {
		return domain.Fail("Jaminan dikembalikan setelah barang kembali.")
	}
	if b.CollateralReturn == nil {
		b.CollateralReturn = &domain.CollateralReturn{}
	}
	b.CollateralReturn.ProviderReturnedAt, b.CollateralReturn.AutoCompleteAt = domain.Ptr(t.now), domain.Ptr(t.now+domain.AutoCompleteTTL)
	t.saveBooking(b)
	t.notify(b.RenterID, "booking", "Jaminan dikembalikan", domain.PublicName(t.user(providerID))+" menandai jaminan ("+domain.DescribeCollateral(b.Collateral)+") sudah dikembalikan. Konfirmasi kalau sudah kamu terima.", domain.BookingHref(b))
	t.sysMsg(id, "Penyedia menandai jaminan sudah dikembalikan. Penyewa punya 48 jam untuk konfirmasi, setelah itu otomatis selesai.")
	return nil
}

// confirmCollateral: COMPLETED needs the provider's "returned" AND the renter's "received" (PRD 8.3).
func (t *txn) confirmCollateral(renterID, id domain.ID) error {
	b, err := t.partyBooking(renterID, id, "renter")
	if err != nil {
		return err
	}
	if b.CollateralReturn == nil || b.CollateralReturn.ProviderReturnedAt == nil {
		return domain.Fail("Penyedia belum menandai jaminan dikembalikan.")
	}
	if b.CollateralReturn.RenterConfirmedAt != nil {
		return domain.Fail("Jaminan sudah dikonfirmasi.")
	}
	completes := b.Status == "RETURNED"
	b.CollateralReturn.RenterConfirmedAt = domain.Ptr(t.now)
	if completes {
		b.Status, b.CompletedAt = "COMPLETED", domain.Ptr(t.now)
	}
	t.saveBooking(b)
	if completes { // a completed rental counts once (the auto-complete job counts the others)
		t.check(t.q.IncrementListingRentCount(t.ctx, b.ListingID))
		t.touchPublic()
	}
	title, body, msg := "Jaminan diterima penyewa", t.titleOf(b)+": penyewa mengonfirmasi jaminan sudah diterima.", "Penyewa mengonfirmasi jaminan diterima."
	if completes {
		title, body, msg = "Sewa selesai", body+" Beri rating, yuk.", "Penyewa mengonfirmasi jaminan diterima. Sewa SELESAI. Silakan saling memberi rating."
	}
	t.notify(b.ProviderID, "booking", title, body, domain.BookingHref(b))
	t.sysMsg(id, msg)
	return nil
}

func (t *txn) submitReview(fromID, id domain.ID, rating float64, comment string) error {
	b, err := t.partyBooking(fromID, id, "any")
	if err != nil {
		return err
	}
	if b.Status != "COMPLETED" && b.Status != "RESOLVED" {
		return domain.Fail("Rating diberikan setelah sewa selesai.")
	}
	dup, err := t.q.ReviewExists(t.ctx, dbq.ReviewExistsParams{BookingID: id, FromID: fromID})
	t.check(err)
	if dup {
		return domain.Fail("Kamu sudah memberi rating.")
	}
	if rating < 1 || rating > 5 {
		return domain.Fail("Rating 1–5.")
	}
	if err := domain.CheckLen("Ulasan", strings.TrimSpace(comment), domain.MaxMessageLen); err != nil {
		return err
	}
	role, to := "penyewa", b.ProviderID
	if fromID == b.ProviderID {
		role, to = "penyedia", b.RenterID
	}
	// FR-T2: both reviews become visible once the counterpart has reviewed too, otherwise after 7 days.
	counterpart, err := t.q.ReviewExists(t.ctx, dbq.ReviewExistsParams{BookingID: id, FromID: to})
	t.check(err)
	visible := domain.ReviewVisibleAt(t.now, counterpart)
	t.check(t.q.InsertReview(t.ctx, dbq.InsertReviewParams{ID: domain.NewID("r"), BookingID: id, ListingID: b.ListingID, FromID: fromID, ToID: to,
		FromRole: role, Rating: rating, Comment: strings.TrimSpace(comment), CreatedAt: tsOf(t.now), VisibleAt: tsOf(visible), Released: counterpart}))
	if counterpart {
		t.check(t.q.ReleaseReviewsOfBooking(t.ctx, dbq.ReleaseReviewsOfBookingParams{BookingID: id, Now: tsOf(t.now)}))
		t.touchPublic()
	}
	t.touch(fromID, to)
	t.notify(to, "booking", "Kamu dapat rating baru", "Rating dari "+domain.PublicName(t.user(fromID))+" tampil setelah kamu juga mengisi (atau setelah 7 hari).", domain.BookingHref(b))
	return nil
}

func (t *txn) sendMessage(senderID, id domain.ID, text *string, photoID *domain.ID) error {
	b, err := t.partyBooking(senderID, id, "any")
	if err != nil {
		return domain.Fail("Chat tidak ditemukan.")
	}
	clean := ""
	if text != nil {
		clean = strings.TrimSpace(*text)
	}
	if clean == "" && photoID == nil {
		return domain.Fail("Pesan kosong.")
	}
	if err := domain.CheckLen("Pesan", clean, domain.MaxMessageLen); err != nil {
		return err
	}
	if photoID != nil && !t.photosExist([]domain.ID{*photoID}) {
		return domain.Fail("Foto belum terunggah ke server. Coba lagi.")
	}
	if photoID != nil && !t.evidenceOf(b, senderID, []domain.ID{*photoID}) {
		return domain.Fail("Foto bukti harus diunggah untuk booking ini.")
	}
	var txt *string
	if clean != "" {
		txt = domain.Ptr(clean)
	}
	t.check(t.q.InsertMessage(t.ctx, dbq.InsertMessageParams{ID: domain.NewID("m"), BookingID: id, SenderID: domain.Ptr(senderID), Text: txt, PhotoID: photoID, At: tsOf(t.now)}))
	t.check(t.q.UpsertChatRead(t.ctx, dbq.UpsertChatReadParams{UserID: senderID, BookingID: id, ReadAt: tsOf(t.now)}))
	t.touch(b.RenterID, b.ProviderID)
	preview := domain.Truncate(clean, 80)
	if preview == "" {
		preview = "Mengirim foto"
	}
	t.notify(domain.Counterpart(b, senderID), "chat", "Pesan dari "+domain.PublicName(t.user(senderID)), preview, domain.BookingHref(b)+"/chat")
	return nil
}

// markThreadRead stores when the user last read a booking chat. Unknown bookings are ignored.
func (t *txn) markThreadRead(userID, bookingID domain.ID) error {
	b := t.booking(bookingID)
	if b == nil || (!domain.IsParty(b, userID, "any") && !t.isAdmin(userID)) {
		return nil
	}
	t.check(t.q.UpsertChatRead(t.ctx, dbq.UpsertChatReadParams{UserID: userID, BookingID: bookingID, ReadAt: tsOf(t.now)}))
	t.touch(b.RenterID, b.ProviderID) // both parties see both read markers ("Dibaca")
	return nil
}

func (t *txn) markNotificationsRead(userID domain.ID, ids []domain.ID) error {
	n, err := t.q.MarkNotificationsRead(t.ctx, dbq.MarkNotificationsReadParams{Now: domain.Ptr(tsOf(t.now)), UserID: userID, Ids: ids})
	t.check(err)
	if n > 0 {
		t.touch(userID)
	}
	return nil
}

// checkMeters bounds the vehicle readings typed at check-out/check-in.
func checkMeters(odometer, fuel *string) error {
	return domain.FirstErr(domain.CheckLenPtr("Odometer", odometer, domain.MaxMeterLen), domain.CheckLenPtr("Indikator BBM", fuel, domain.MaxMeterLen))
}

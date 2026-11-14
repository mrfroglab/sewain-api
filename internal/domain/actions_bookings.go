package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type BookingInput struct {
	ListingID          ID      `json:"listingId"`
	StartDate          string  `json:"startDate"`
	EndDate            string  `json:"endDate"`
	Handover           string  `json:"handover"`
	CollateralOptionID ID      `json:"collateralOptionId"`
	Note               *string `json:"note,omitempty"`
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// CreateBooking: request → agreement snapshot (hashed, FR-R2) → REQUESTED with a 24h expiry (FR-R3).
// Prevents schedule clashes with stock awareness (FR-R4). Auto-approval only for trusted renters and never
// for high-value items or vehicles (PRD 11.4).
func (e *Engine) CreateBooking(renterID ID, in BookingInput, wantID string) (ID, error) {
	if err := e.canTransact(renterID); err != nil {
		return "", err
	}
	l := e.listing(in.ListingID)
	if l == nil || l.Status != "aktif" {
		return "", fail("Barang tidak tersedia.")
	}
	if l.OwnerID == renterID {
		return "", fail("Tidak bisa menyewa barang sendiri.")
	}
	owner, renter := e.user(l.OwnerID), e.user(renterID)
	if _, err := ParseIsoDate(in.StartDate); err != nil {
		return "", fail("Tanggal ambil tidak valid.")
	}
	if _, err := ParseIsoDate(in.EndDate); err != nil {
		return "", fail("Tanggal kembali tidak valid.")
	}
	if in.EndDate < in.StartDate {
		return "", fail("Tanggal kembali harus setelah tanggal ambil.")
	}
	if in.StartDate < IsoDate(e.Now()) {
		return "", fail("Tanggal ambil sudah lewat.")
	}
	var collateral *CollateralOption
	for i := range l.CollateralOptions {
		if l.CollateralOptions[i].ID == in.CollateralOptionID {
			collateral = &l.CollateralOptions[i]
		}
	}
	if collateral == nil {
		return "", fail("Pilih bentuk jaminan dari opsi penyedia.")
	}
	if !contains(l.Handover, in.Handover) {
		return "", fail("Metode serah terima tidak tersedia.")
	}
	quote := QuoteRent(l, in.StartDate, in.EndDate)
	if l.MinDays != nil && quote.Days < *l.MinDays {
		return "", fail(fmt.Sprintf("Minimal sewa %d hari.", *l.MinDays))
	}
	if l.MaxDays != nil && quote.Days > *l.MaxDays {
		return "", fail(fmt.Sprintf("Maksimal sewa %d hari.", *l.MaxDays))
	}
	committed := 0
	for _, b := range e.d.Bookings {
		if b.ListingID == l.ID && (b.Status == "ACCEPTED" || b.Status == "ACTIVE") && b.StartDate <= in.EndDate && in.StartDate <= b.EndDate {
			committed++
		}
	}
	if committed >= l.Stock {
		return "", fail("Barang sudah dipesan di tanggal itu. Coba tanggal lain.")
	}
	t := e.Now()
	commission := ComputeCommission(quote.Total, e.d.Commission)
	ag := Agreement{
		CreatedAt: t, ListingID: l.ID, ListingTitle: l.Title, Category: l.Category, ItemValue: l.ItemValue,
		RenterName: PublicName(renter), ProviderName: PublicName(owner), StartDate: in.StartDate, EndDate: in.EndDate,
		Quote: quote, Collateral: *collateral, CollateralValue: CollateralValue(*collateral, l.ItemValue), Handover: in.Handover,
		LateFeePerDay: l.Rules.LateFeePerDay, LateFeeCap: int64(float64(l.ItemValue) * l.Rules.LateFeeCapPercent / 100),
		Cancellation: l.Rules.Cancellation, Special: l.Rules.Special, ConditionNotes: l.ConditionNotes, ReferencePhotos: append([]string{}, l.Photos...),
		Commission: commission,
	}
	body, _ := json.Marshal(ag)
	sum := sha256.Sum256(body)
	ag.Hash = hex.EncodeToString(sum[:])
	ag.RenterAgreedAt = ptr(t)
	e.d.BookingCounter++
	now := time.UnixMilli(t).In(WIB)
	id := pickID(wantID, "b", func(x ID) bool { return e.booking(x) != nil })
	deposit := DepositCash(*collateral, l.ItemValue)
	var note *string
	if in.Note != nil && strings.TrimSpace(*in.Note) != "" {
		note = ptr(strings.TrimSpace(*in.Note))
	}
	b := &Booking{
		ID: id, Code: fmt.Sprintf("SWN-%s-%04d", now.Format("0601"), e.d.BookingCounter), ListingID: l.ID, RenterID: renterID, ProviderID: l.OwnerID,
		StartDate: in.StartDate, EndDate: in.EndDate, Handover: in.Handover, Collateral: *collateral, Note: note, Status: "REQUESTED",
		CreatedAt: t, ExpiresAt: t + 24*Hour, Agreement: ag,
		Payment: PaymentInfo{Status: "belum", Rent: quote.Total, Deposit: deposit, Commission: commission.RenterPays, Total: quote.Total + deposit + commission.RenterPays},
	}
	e.d.Bookings = append([]*Booking{b}, e.d.Bookings...)
	e.settings = true // booking counter
	e.touch("bookings", id)
	e.notify(l.OwnerID, "booking", "Request sewa baru", fmt.Sprintf("%s ingin menyewa %s (%d hari).", PublicName(renter), l.Title, quote.Days), bookingHref(b))
	e.sysMsg(id, "Request sewa dikirim. Penyedia punya waktu 24 jam untuk merespons.")
	rating := deref(renter.BaseRating, 0)
	highValue := l.ItemValue > e.d.CollateralRules.HighValueBadge || IsVehicle(l.Category)
	if l.Approval == "otomatis" && !highValue && rating >= deref(l.AutoApproveMinRating, 4.5) {
		if err := e.AcceptBooking(l.OwnerID, id, nil); err == nil {
			e.sysMsg(id, fmt.Sprintf("Disetujui otomatis: rating penyewa %.1f memenuhi ambang penyedia.", rating))
		}
	}
	return id, nil
}

func (e *Engine) partyBooking(actor, id ID, role string) (*Booking, error) {
	b := e.booking(id)
	if b == nil || !isParty(b, actor, role) {
		return nil, fail("Booking tidak ditemukan.")
	}
	return b, nil
}

func (e *Engine) AcceptBooking(providerID, id ID, meeting *string) error {
	b, err := e.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Status != "REQUESTED" {
		return fail("Request ini sudah tidak menunggu persetujuan.")
	}
	l := e.listing(b.ListingID)
	t := e.Now()
	b.Status, b.AcceptedAt = "ACCEPTED", ptr(t)
	b.Agreement.ProviderAgreedAt = ptr(t)
	detail := "Atur titik temu lewat chat (disarankan tempat publik)."
	if meeting != nil && strings.TrimSpace(*meeting) != "" {
		detail = strings.TrimSpace(*meeting)
	} else if l != nil && l.MeetingPoint != nil {
		detail = *l.MeetingPoint
	}
	b.MeetingPointDetail = ptr(detail)
	e.touch("bookings", id)
	e.notify(b.RenterID, "booking", "Request disetujui", PublicName(e.user(providerID))+" menyetujui sewa "+e.titleOf(b)+". Atur serah terima lewat chat.", bookingHref(b))
	e.sysMsg(id, "Request disetujui. Kesepakatan sewa sudah disetujui kedua pihak, dan detail titik temu sekarang terlihat.")
	return nil
}

func (e *Engine) DeclineBooking(providerID, id ID, reason string) error {
	b, err := e.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Status != "REQUESTED" {
		return fail("Request ini sudah tidak menunggu persetujuan.")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Tidak ada alasan."
	}
	b.Status, b.DeclinedAt, b.DeclineReason = "DECLINED", ptr(e.Now()), ptr(reason)
	e.touch("bookings", id)
	e.notify(b.RenterID, "booking", "Request ditolak", e.titleOf(b)+": "+reason, bookingHref(b))
	e.sysMsg(id, "Request ditolak penyedia. Alasan: "+reason)
	return nil
}

func (e *Engine) CancelBooking(actor, id ID, reason string) error {
	b, err := e.partyBooking(actor, id, "any")
	if err != nil {
		return err
	}
	if b.Status != "REQUESTED" && b.Status != "ACCEPTED" {
		return fail("Booking hanya bisa dibatalkan sebelum serah terima.")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "-"
	}
	other, who := b.RenterID, "penyedia"
	if actor == b.RenterID {
		other, who = b.ProviderID, "penyewa"
	}
	b.Status, b.CancelledAt, b.CancelledBy, b.CancelReason = "CANCELLED", ptr(e.Now()), ptr(actor), ptr(reason)
	e.touch("bookings", id)
	e.notify(other, "booking", "Booking dibatalkan", PublicName(e.user(actor))+" membatalkan "+e.titleOf(b)+". Alasan: "+reason, bookingHref(b))
	e.sysMsg(id, "Booking dibatalkan oleh "+who+". Alasan: "+reason)
	return nil
}

func (e *Engine) PayBooking(renterID, id ID, method string) error {
	b, err := e.partyBooking(renterID, id, "renter")
	if err != nil {
		return err
	}
	if b.Status != "ACCEPTED" && b.Status != "ACTIVE" {
		return fail("Pembayaran tersedia setelah request disetujui.")
	}
	if b.Payment.Status == "lunas" {
		return fail("Sudah lunas.")
	}
	if !contains([]string{"qris", "va", "ewallet", "tunai"}, method) {
		method = "qris"
	}
	b.Payment.Status, b.Payment.Method, b.Payment.PaidAt = "lunas", ptr(method), ptr(e.Now())
	e.touch("bookings", id)
	e.notify(b.ProviderID, "booking", "Pembayaran diterima (simulasi)", e.titleOf(b)+": "+Rupiah(b.Payment.Total)+" lunas via Sewain.", bookingHref(b))
	e.sysMsg(id, "Pembayaran "+Rupiah(b.Payment.Total)+" ditandai lunas (simulasi "+strings.ToUpper(method)+").")
	return nil
}

type CheckoutInput struct {
	PhotoIDs           []ID    `json:"photoIds"`
	CollateralPhotoIDs []ID    `json:"collateralPhotoIds"`
	Odometer           *string `json:"odometer,omitempty"`
	Fuel               *string `json:"fuel,omitempty"`
}

func (e *Engine) photosExist(ids []ID) bool {
	for _, id := range ids {
		found := false
		for _, p := range e.d.Photos {
			if p.ID == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SubmitCheckout: ≥3 condition photos + a collateral photo (FR-J5); vehicles also odometer + fuel (PRD 12.3).
func (e *Engine) SubmitCheckout(actor, id ID, in CheckoutInput) error {
	b := e.booking(id)
	if b == nil || b.ProviderID != actor {
		return fail("Hanya penyedia yang memotret serah terima.")
	}
	if b.Status != "ACCEPTED" {
		return fail("Serah terima hanya untuk booking yang sudah disetujui.")
	}
	if len(in.PhotoIDs) < 3 {
		return fail("Minimal 3 foto kondisi barang.")
	}
	if len(in.CollateralPhotoIDs) < 1 {
		return fail("Foto jaminan wajib (FR-J5).")
	}
	if !e.photosExist(append(append([]ID{}, in.PhotoIDs...), in.CollateralPhotoIDs...)) {
		return fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	}
	l := e.listing(b.ListingID)
	if l != nil && IsVehicle(l.Category) && (deref(in.Odometer, "") == "" || deref(in.Fuel, "") == "") {
		return fail("Kendaraan: isi odometer dan indikator BBM.")
	}
	t := e.Now()
	b.Checkout = &Checkout{PhotoIDs: in.PhotoIDs, CollateralPhotoIDs: in.CollateralPhotoIDs, Odometer: in.Odometer, Fuel: in.Fuel, SubmittedAt: t, ProviderAgreedAt: ptr(t)}
	e.touch("bookings", id)
	e.notify(b.RenterID, "booking", "Cek kondisi barang", PublicName(e.user(actor))+" sudah memotret "+e.titleOf(b)+" + jaminan. Periksa lalu tekan \"Setuju kondisi\".", bookingHref(b)+"/serah-terima")
	e.sysMsg(id, fmt.Sprintf("Foto check-out diunggah (%d foto barang + %d foto jaminan). Menunggu penyewa menyetujui kondisi.", len(in.PhotoIDs), len(in.CollateralPhotoIDs)))
	return nil
}

func (e *Engine) AddCheckoutPhotos(actor, id ID, photoIDs []ID) error {
	b, err := e.partyBooking(actor, id, "any")
	if err != nil || b.Checkout == nil {
		return fail("Foto check-out belum ada.")
	}
	if b.Status != "ACCEPTED" {
		return fail("Serah terima sudah selesai.")
	}
	if !e.photosExist(photoIDs) {
		return fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	}
	b.Checkout.PhotoIDs = append(b.Checkout.PhotoIDs, photoIDs...)
	b.Checkout.RenterAgreedAt = nil
	e.touch("bookings", id)
	who := "Penyedia"
	if actor == b.RenterID {
		who = "Penyewa"
	}
	e.sysMsg(id, fmt.Sprintf("%s menambah %d foto kondisi.", who, len(photoIDs)))
	return nil
}

// AgreeCheckout: ACTIVE starts only after BOTH parties agree on the item + collateral condition.
func (e *Engine) AgreeCheckout(actor, id ID) error {
	b, err := e.partyBooking(actor, id, "any")
	if err != nil || b.Checkout == nil {
		return fail("Foto check-out belum ada.")
	}
	if b.Status != "ACCEPTED" {
		return fail("Serah terima sudah selesai.")
	}
	t := e.Now()
	if actor == b.RenterID {
		b.Checkout.RenterAgreedAt = ptr(t)
	} else {
		b.Checkout.ProviderAgreedAt = ptr(t)
	}
	e.touch("bookings", id)
	if b.Checkout.RenterAgreedAt != nil && b.Checkout.ProviderAgreedAt != nil {
		b.Status, b.ActiveAt = "ACTIVE", ptr(t)
		e.notify(b.RenterID, "booking", "Sewa dimulai", e.titleOf(b)+" resmi kamu sewa sampai "+b.EndDate+". Jaga baik-baik, ya!", bookingHref(b))
		e.notify(b.ProviderID, "booking", "Sewa dimulai", e.titleOf(b)+" sudah diserahkan. Jaminan tercatat.", bookingHref(b))
		e.sysMsg(id, "Serah terima selesai. Kedua pihak menyetujui kondisi barang dan jaminan. Status: Sedang disewa.")
	}
	return nil
}

func (e *Engine) RejectAtCheckout(renterID, id ID, note string) error {
	b, err := e.partyBooking(renterID, id, "renter")
	if err != nil {
		return err
	}
	if b.Status != "ACCEPTED" {
		return fail("Hanya saat serah terima.")
	}
	t := e.Now()
	r := &Report{ID: NewID("rp"), ReporterID: renterID, TargetType: "listing", TargetID: b.ListingID, Reason: "Barang tidak sesuai deskripsi (saat serah terima)", Detail: ptr(note), CreatedAt: t, Status: "baru"}
	e.d.Reports = append([]*Report{r}, e.d.Reports...)
	e.touch("reports", r.ID)
	b.Status, b.CancelledAt, b.CancelledBy, b.CancelReason = "CANCELLED", ptr(t), ptr(renterID), ptr("Barang tidak sesuai deskripsi: "+note)
	e.touch("bookings", id)
	e.notify(b.ProviderID, "booking", "Penyewa membatalkan saat serah terima", e.titleOf(b)+" dilaporkan tidak sesuai deskripsi. Admin akan meninjau.", bookingHref(b))
	e.notify(AdminID, "sistem", "Laporan: barang tidak sesuai", e.titleOf(b)+" dilaporkan saat serah terima.", "/admin/laporan")
	e.sysMsg(id, "Penyewa membatalkan tanpa penalti karena barang tidak sesuai deskripsi. Laporan diteruskan ke admin.")
	return nil
}

type CheckinInput struct {
	PhotoIDs        []ID    `json:"photoIds"`
	Result          string  `json:"result"`
	ProblemType     *string `json:"problemType,omitempty"`
	ProblemNote     *string `json:"problemNote,omitempty"`
	ProblemPhotoIDs []ID    `json:"problemPhotoIds,omitempty"`
	Odometer        *string `json:"odometer,omitempty"`
	Fuel            *string `json:"fuel,omitempty"`
}

var (
	reLost    = regexp.MustCompile(`(?i)hilang|kurang`)
	reHeavy   = regexp.MustCompile(`(?i)berat|pecah|mati`)
	reScratch = regexp.MustCompile(`(?i)lecet|gores`)
)

func (e *Engine) SubmitCheckin(actor, id ID, in CheckinInput) error {
	b := e.booking(id)
	if b == nil || b.ProviderID != actor {
		return fail("Hanya penyedia yang memotret pengembalian.")
	}
	if b.Status != "ACTIVE" {
		return fail("Pengembalian hanya untuk booking yang sedang disewa.")
	}
	if len(in.PhotoIDs) < 3 {
		return fail("Minimal 3 foto kondisi saat kembali.")
	}
	if in.Result != "sesuai" && in.Result != "masalah" {
		return fail("Pilih: kondisi sesuai atau ada masalah.")
	}
	if in.Result == "masalah" && (deref(in.ProblemType, "") == "" || len(in.ProblemPhotoIDs) == 0) {
		return fail("Pilih jenis masalah dan lampirkan foto masalahnya.")
	}
	all := append(append([]ID{}, in.PhotoIDs...), in.ProblemPhotoIDs...)
	if !e.photosExist(all) {
		return fail("Ada foto yang belum terunggah ke server. Coba lagi.")
	}
	t := e.Now()
	b.Status, b.ReturnedAt = "RETURNED", ptr(t)
	b.Checkin = &Checkin{PhotoIDs: all, Result: in.Result, ProblemType: in.ProblemType, ProblemNote: in.ProblemNote, Odometer: in.Odometer, Fuel: in.Fuel, At: t}
	e.touch("bookings", id)
	cond := "sesuai"
	if in.Result == "masalah" {
		cond = "ada masalah (" + deref(in.ProblemType, "") + ")"
	}
	e.notify(b.RenterID, "booking", "Pengembalian dicatat", e.titleOf(b)+" sudah diterima penyedia. Kondisi: "+cond+".", bookingHref(b))
	e.sysMsg(id, fmt.Sprintf("Pengembalian dicatat (%d foto). Kondisi: %s.", len(in.PhotoIDs), strings.Replace(cond, "ada masalah (", "ada masalah: ", 1)))
	if in.Result == "masalah" {
		pt := deref(in.ProblemType, "")
		caseType := "rusak-ringan"
		switch {
		case reLost.MatchString(pt):
			caseType = "hilang"
		case reHeavy.MatchString(pt):
			caseType = "rusak-berat"
		case reScratch.MatchString(pt):
			caseType = "lecet-ringan"
		}
		desc := pt
		if in.ProblemNote != nil && *in.ProblemNote != "" {
			desc += " — " + *in.ProblemNote
		}
		_, _ = e.OpenDispute(actor, id, DisputeInput{CaseType: caseType, Description: desc, PhotoIDs: in.ProblemPhotoIDs}, "")
	}
	return nil
}

func (e *Engine) ReturnCollateral(providerID, id ID) error {
	b, err := e.partyBooking(providerID, id, "provider")
	if err != nil {
		return err
	}
	if b.Status != "RETURNED" && b.Status != "RESOLVED" {
		return fail("Jaminan dikembalikan setelah barang kembali.")
	}
	t := e.Now()
	if b.CollateralReturn == nil {
		b.CollateralReturn = &CollateralReturn{}
	}
	b.CollateralReturn.ProviderReturnedAt, b.CollateralReturn.AutoCompleteAt = ptr(t), ptr(t+48*Hour)
	e.touch("bookings", id)
	e.notify(b.RenterID, "booking", "Jaminan dikembalikan", PublicName(e.user(providerID))+" menandai jaminan ("+DescribeCollateral(b.Collateral)+") sudah dikembalikan. Konfirmasi kalau sudah kamu terima.", bookingHref(b))
	e.sysMsg(id, "Penyedia menandai jaminan sudah dikembalikan. Penyewa punya 48 jam untuk konfirmasi, setelah itu otomatis selesai.")
	return nil
}

// ConfirmCollateral: COMPLETED needs the provider's "returned" AND the renter's "received" (PRD 8.3).
func (e *Engine) ConfirmCollateral(renterID, id ID) error {
	b, err := e.partyBooking(renterID, id, "renter")
	if err != nil {
		return err
	}
	if b.CollateralReturn == nil || b.CollateralReturn.ProviderReturnedAt == nil {
		return fail("Penyedia belum menandai jaminan dikembalikan.")
	}
	t := e.Now()
	completes := b.Status == "RETURNED"
	b.CollateralReturn.RenterConfirmedAt = ptr(t)
	if completes {
		b.Status, b.CompletedAt = "COMPLETED", ptr(t)
	}
	e.touch("bookings", id)
	if l := e.listing(b.ListingID); l != nil {
		l.RentCount++
		e.touch("listings", l.ID)
	}
	title, body, msg := "Jaminan diterima penyewa", e.titleOf(b)+": penyewa mengonfirmasi jaminan sudah diterima.", "Penyewa mengonfirmasi jaminan diterima."
	if completes {
		title, body, msg = "Sewa selesai", body+" Beri rating, yuk.", "Penyewa mengonfirmasi jaminan diterima. Sewa SELESAI. Silakan saling memberi rating."
	}
	e.notify(b.ProviderID, "booking", title, body, bookingHref(b))
	e.sysMsg(id, msg)
	return nil
}

func (e *Engine) SubmitReview(fromID, id ID, rating float64, comment string) error {
	b, err := e.partyBooking(fromID, id, "any")
	if err != nil {
		return err
	}
	if b.Status != "COMPLETED" && b.Status != "RESOLVED" {
		return fail("Rating diberikan setelah sewa selesai.")
	}
	for _, r := range e.d.Reviews {
		if r.BookingID == id && r.FromID == fromID {
			return fail("Kamu sudah memberi rating.")
		}
	}
	if rating < 1 || rating > 5 {
		return fail("Rating 1–5.")
	}
	role, to := "penyewa", b.ProviderID
	if fromID == b.ProviderID {
		role, to = "penyedia", b.RenterID
	}
	r := &Review{ID: NewID("r"), BookingID: id, ListingID: b.ListingID, FromID: fromID, ToID: to, FromRole: role, Rating: rating, Comment: strings.TrimSpace(comment), CreatedAt: e.Now()}
	e.d.Reviews = append(e.d.Reviews, r)
	e.touch("reviews", r.ID)
	e.notify(to, "booking", "Kamu dapat rating baru", "Rating dari "+PublicName(e.user(fromID))+" tampil setelah kamu juga mengisi (atau setelah 7 hari).", bookingHref(b))
	return nil
}

func (e *Engine) SendMessage(senderID, id ID, text *string, photoID *ID) error {
	b, err := e.partyBooking(senderID, id, "any")
	if err != nil {
		return fail("Chat tidak ditemukan.")
	}
	clean := ""
	if text != nil {
		clean = strings.TrimSpace(*text)
	}
	if clean == "" && photoID == nil {
		return fail("Pesan kosong.")
	}
	if photoID != nil && !e.photosExist([]ID{*photoID}) {
		return fail("Foto belum terunggah ke server. Coba lagi.")
	}
	t := e.Now()
	m := &Message{ID: NewID("m"), BookingID: id, SenderID: senderID, PhotoID: photoID, At: t}
	if clean != "" {
		m.Text = ptr(clean)
	}
	e.d.Messages = append(e.d.Messages, m)
	e.touch("messages", m.ID)
	e.d.ChatReads[senderID+":"+id] = t
	e.settings = true
	other := b.RenterID
	if senderID == b.RenterID {
		other = b.ProviderID
	}
	preview := truncate(clean, 80)
	if preview == "" {
		preview = "Mengirim foto"
	}
	e.notify(other, "chat", "Pesan dari "+PublicName(e.user(senderID)), preview, bookingHref(b)+"/chat")
	return nil
}

func (e *Engine) MarkThreadRead(userID, bookingID ID) error {
	e.d.ChatReads[userID+":"+bookingID] = e.Now()
	e.settings = true
	return nil
}

func (e *Engine) MarkNotificationsRead(userID ID, ids []ID) error {
	set := map[ID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for _, n := range e.d.Notifications {
		if n.UserID == userID && !n.Read && (ids == nil || set[n.ID]) {
			n.Read = true
			e.touch("notifications", n.ID)
		}
	}
	return nil
}

package domain

import (
	"fmt"
	"strings"
)

// BannedWordsIn: keyword filter for listings (FR-L4), whole-word, case-insensitive.
func BannedWordsIn(text string, banned []string) []string {
	norm := " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') }), " ") + " "
	var out []string
	for _, w := range banned {
		w = strings.ToLower(strings.TrimSpace(w))
		if w != "" && strings.Contains(norm, " "+w+" ") {
			out = append(out, w)
		}
	}
	return out
}

// ApplyListing merges a create/update payload into a listing (same semantics as the web store).
func ApplyListing(l *Listing, in ListingInput) {
	if in.Title != nil {
		l.Title = strings.TrimSpace(*in.Title)
	}
	if in.Category != nil {
		l.Category = *in.Category
	}
	if in.Description != nil {
		l.Description = *in.Description
	}
	if in.Photos != nil {
		l.Photos = in.Photos
	}
	if in.ConditionNotes != nil {
		l.ConditionNotes = *in.ConditionNotes
	}
	if in.PricePerDay != nil {
		l.PricePerDay = *in.PricePerDay
	}
	l.PricePerHour, l.PricePerWeek, l.DurationDiscount, l.MinDays, l.MaxDays = in.PricePerHour, in.PricePerWeek, in.DurationDiscount, in.MinDays, in.MaxDays
	if in.ItemValue != nil {
		l.ItemValue = *in.ItemValue
	}
	if in.CollateralOptions != nil {
		l.CollateralOptions = in.CollateralOptions
	}
	if in.Location != nil {
		l.Location = *in.Location
	}
	if in.RadiusKm != nil {
		l.RadiusKm = *in.RadiusKm
	}
	if in.Handover != nil {
		l.Handover = in.Handover
	}
	l.MeetingPoint, l.HandoverHours = in.MeetingPoint, in.HandoverHours
	if in.Rules != nil {
		l.Rules = *in.Rules
	}
	if in.Approval != nil {
		l.Approval = *in.Approval
	}
	l.AutoApproveMinRating = in.AutoApproveMinRating
	if in.Stock != nil {
		l.Stock = *in.Stock
	}
	l.Vehicle = in.Vehicle
	NormListing(l)
}

// ValidateListing checks a listing before it is saved (FR-L*). Messages match the web app.
func ValidateListing(l *Listing, banned []string) error {
	switch {
	case len([]rune(l.Title)) < 4:
		return Fail("Judul terlalu pendek.")
	case CategoryNames[l.Category] == "":
		return Fail("Pilih kategori yang tersedia.")
	case l.PricePerDay <= 0:
		return Fail("Harga per hari wajib diisi.")
	case l.ItemValue <= 0:
		return Fail("Isi perkiraan nilai barang.")
	case len(l.Photos) < 3:
		return Fail("Minimal 3 foto barang.")
	case len(l.CollateralOptions) == 0:
		return Fail("Pilih minimal satu bentuk jaminan.")
	case len(l.Handover) == 0:
		return Fail("Pilih minimal satu cara serah terima.")
	case IsVehicle(l.Category) && l.Vehicle == nil:
		return Fail("Kendaraan wajib mengisi data STNK.")
	}
	if err := validateListingBounds(l); err != nil {
		return err
	}
	if words := BannedWordsIn(l.Title+" "+l.Description, banned); len(words) > 0 {
		return Fail("Ada kata yang dilarang: " + strings.Join(words, ", ") + ".")
	}
	return nil
}

// NeedsManualApproval: high-value items and vehicles always need manual approval (PRD 11.4).
func NeedsManualApproval(l *Listing, rules CollateralRules) bool {
	return IsVehicle(l.Category) || l.ItemValue > rules.HighValueBadge
}

// Bounds of listing input (review wave 2, M1). Money is integer rupiah; MaxRupiah keeps price × days (≤ MaxRentalDays)
// far from int64 overflow and is above any item rented peer to peer.
const (
	MaxRupiah         int64 = 10_000_000_000 // Rp10 miliar
	MaxRentalDays           = 365
	MaxListingStock         = 1000
	MaxListingRadius        = 50 // km, like the search and the CSV import
	maxListingPhotos        = 20
	maxCollateralOpts       = 10
)

// textLimits are the longest texts a listing may carry (runes).
var textLimits = []struct {
	name string
	max  int
	get  func(*Listing) string
}{
	{"Judul", 120, func(l *Listing) string { return l.Title }},
	{"Deskripsi", 5000, func(l *Listing) string { return l.Description }},
	{"Catatan kondisi", 2000, func(l *Listing) string { return l.ConditionNotes }},
	{"Titik temu", 200, func(l *Listing) string { return Deref(l.MeetingPoint, "") }},
	{"Jam serah terima", 200, func(l *Listing) string { return Deref(l.HandoverHours, "") }},
	{"Kebijakan pembatalan", 500, func(l *Listing) string { return l.Rules.Cancellation }},
	{"Aturan khusus", 2000, func(l *Listing) string { return Deref(l.Rules.Special, "") }},
}

func validateListingBounds(l *Listing) error {
	for _, t := range textLimits {
		if n := len([]rune(t.get(l))); n > t.max {
			return Fail(fmt.Sprintf("%s terlalu panjang (maks %d karakter).", t.name, t.max))
		}
	}
	money := func(v int64) bool { return v > 0 && v <= MaxRupiah }
	optMoney := func(v *int64) bool { return v == nil || money(*v) }
	switch {
	case !money(l.PricePerDay) || !optMoney(l.PricePerHour) || !optMoney(l.PricePerWeek):
		return Fail("Harga sewa tidak valid.")
	case l.ItemValue > MaxRupiah:
		return Fail("Nilai barang terlalu besar.")
	case l.Rules.LateFeePerDay < 0 || l.Rules.LateFeePerDay > MaxRupiah:
		return Fail("Denda telat per hari tidak valid.")
	case l.Rules.LateFeeCapPercent < 0 || l.Rules.LateFeeCapPercent > 100:
		return Fail("Batas denda telat maksimal 100% nilai barang.")
	case l.DurationDiscount != nil && (l.DurationDiscount.Percent <= 0 || l.DurationDiscount.Percent >= 100 ||
		l.DurationDiscount.MinDays < 1 || l.DurationDiscount.MinDays > MaxRentalDays):
		return Fail("Diskon durasi tidak valid (lebih dari 0% dan kurang dari 100%).")
	case l.MinDays != nil && (*l.MinDays < 1 || *l.MinDays > MaxRentalDays),
		l.MaxDays != nil && (*l.MaxDays < 1 || *l.MaxDays > MaxRentalDays),
		l.MinDays != nil && l.MaxDays != nil && *l.MinDays > *l.MaxDays:
		return Fail(fmt.Sprintf("Minimal/maksimal hari sewa tidak valid (1–%d hari).", MaxRentalDays))
	case l.RadiusKm > MaxListingRadius:
		return Fail(fmt.Sprintf("Jangkauan maksimal %d km.", MaxListingRadius))
	case l.Stock > MaxListingStock:
		return Fail(fmt.Sprintf("Stok maksimal %d unit.", MaxListingStock))
	case l.AutoApproveMinRating != nil && (*l.AutoApproveMinRating < 1 || *l.AutoApproveMinRating > 5):
		return Fail("Ambang rating persetujuan otomatis 1–5.")
	case l.Location.Lat < -90 || l.Location.Lat > 90 || l.Location.Lng < -180 || l.Location.Lng > 180:
		return Fail("Lokasi tidak valid.")
	case len(l.Photos) > maxListingPhotos:
		return Fail(fmt.Sprintf("Maksimal %d foto barang.", maxListingPhotos))
	case len(l.CollateralOptions) > maxCollateralOpts:
		return Fail(fmt.Sprintf("Maksimal %d bentuk jaminan.", maxCollateralOpts))
	}
	for _, h := range l.Handover {
		if h != "cod" && h != "jemput" && h != "kurir" && h != "antar" { // web: cod | jemput | kurir; CSV import: antar
			return Fail("Cara serah terima tidak dikenal.")
		}
	}
	for _, o := range l.CollateralOptions {
		bad := false
		switch o.Kind {
		case "identitas":
		case "deposit":
			if Deref(o.DepositMode, "") == "persen" {
				p := Deref(o.DepositPercent, 0)
				bad = p <= 0 || p > 100
			} else {
				bad = !optMoney(o.DepositAmount) || o.DepositAmount == nil
			}
		case "barang", "custom":
			bad = o.EstimatedValue != nil && (*o.EstimatedValue < 0 || *o.EstimatedValue > MaxRupiah)
		default:
			bad = true
		}
		if bad || len([]rune(Deref(o.Description, ""))) > 500 {
			return Fail("Bentuk jaminan tidak valid (deposit lebih dari 0 dan maksimal 100% nilai barang).")
		}
	}
	return nil
}

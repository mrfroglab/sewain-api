package domain

import "strings"

// BannedWordsIn: keyword filter for listings (FR-L4), whole-word, case-insensitive.
func BannedWordsIn(text string, banned []string) []string {
	norm := " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }), " ") + " "
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
	if words := BannedWordsIn(l.Title+" "+l.Description, banned); len(words) > 0 {
		return Fail("Ada kata yang dilarang: " + strings.Join(words, ", ") + ".")
	}
	return nil
}

// NeedsManualApproval: high-value items and vehicles always need manual approval (PRD 11.4).
func NeedsManualApproval(l *Listing, rules CollateralRules) bool {
	return IsVehicle(l.Category) || l.ItemValue > rules.HighValueBadge
}

package domain

import "strings"

type ListingInput struct {
	Title                *string            `json:"title,omitempty"`
	Category             *string            `json:"category,omitempty"`
	Description          *string            `json:"description,omitempty"`
	Photos               []string           `json:"photos,omitempty"`
	ConditionNotes       *string            `json:"conditionNotes,omitempty"`
	PricePerDay          *int64             `json:"pricePerDay,omitempty"`
	PricePerHour         *int64             `json:"pricePerHour,omitempty"`
	PricePerWeek         *int64             `json:"pricePerWeek,omitempty"`
	DurationDiscount     *DurationDiscount  `json:"durationDiscount,omitempty"`
	MinDays              *int               `json:"minDays,omitempty"`
	MaxDays              *int               `json:"maxDays,omitempty"`
	ItemValue            *int64             `json:"itemValue,omitempty"`
	CollateralOptions    []CollateralOption `json:"collateralOptions,omitempty"`
	Location             *ListingLocation   `json:"location,omitempty"`
	RadiusKm             *float64           `json:"radiusKm,omitempty"`
	Handover             []string           `json:"handover,omitempty"`
	MeetingPoint         *string            `json:"meetingPoint,omitempty"`
	HandoverHours        *string            `json:"handoverHours,omitempty"`
	Rules                *ListingRules      `json:"rules,omitempty"`
	Approval             *string            `json:"approval,omitempty"`
	AutoApproveMinRating *float64           `json:"autoApproveMinRating,omitempty"`
	Stock                *int               `json:"stock,omitempty"`
	Vehicle              *VehicleInfo       `json:"vehicle,omitempty"`
}

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

func (e *Engine) applyListing(l *Listing, in ListingInput) {
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
	normListing(l)
}

func (e *Engine) validateListing(l *Listing) error {
	switch {
	case len([]rune(l.Title)) < 4:
		return fail("Judul terlalu pendek.")
	case CategoryNames[l.Category] == "":
		return fail("Pilih kategori yang tersedia.")
	case l.PricePerDay <= 0:
		return fail("Harga per hari wajib diisi.")
	case l.ItemValue <= 0:
		return fail("Isi perkiraan nilai barang.")
	case len(l.Photos) < 3:
		return fail("Minimal 3 foto barang.")
	case len(l.CollateralOptions) == 0:
		return fail("Pilih minimal satu bentuk jaminan.")
	case len(l.Handover) == 0:
		return fail("Pilih minimal satu cara serah terima.")
	case IsVehicle(l.Category) && l.Vehicle == nil:
		return fail("Kendaraan wajib mengisi data STNK.")
	}
	if words := BannedWordsIn(l.Title+" "+l.Description, e.d.BannedKeywords); len(words) > 0 {
		return fail("Ada kata yang dilarang: " + strings.Join(words, ", ") + ".")
	}
	return nil
}

func (e *Engine) CreateListing(ownerID ID, in ListingInput, wantID string) (ID, error) {
	if err := e.canTransact(ownerID); err != nil {
		return "", err
	}
	owner := e.user(ownerID)
	id := pickID(wantID, "l", func(x ID) bool { return e.listing(x) != nil })
	l := &Listing{ID: id, Slug: id, OwnerID: ownerID, Status: "aktif", CreatedAt: e.Now(), Stock: 1, Approval: "manual", RadiusKm: 3}
	e.applyListing(l, in)
	if owner.AccountType != "toko" || l.Stock < 1 {
		l.Stock = 1
	}
	// High value items and vehicles always need manual approval (PRD 11.4).
	if IsVehicle(l.Category) || l.ItemValue > e.d.CollateralRules.HighValueBadge {
		l.Approval = "manual"
	}
	if err := e.validateListing(l); err != nil {
		return "", err
	}
	e.d.Listings = append([]*Listing{l}, e.d.Listings...)
	e.touch("listings", id)
	// Wanted board: a new listing in a requested category notifies the requesters (demand → supply).
	for _, w := range e.d.Wanted {
		if w.Status == "terbuka" && w.Category != nil && *w.Category == l.Category && w.RequesterID != ownerID {
			e.notify(w.RequesterID, "papan", "Ada barang baru yang cocok", l.Title+" baru saja disewakan di dekatmu.", "/barang/"+id)
		}
	}
	return id, nil
}

func (e *Engine) UpdateListing(actor, listingID ID, in ListingInput) error {
	l := e.listing(listingID)
	if l == nil {
		return fail("Listing tidak ditemukan.")
	}
	if l.OwnerID != actor && !e.isAdmin(actor) {
		return fail("Bukan listing milikmu.")
	}
	next := *l
	e.applyListing(&next, in)
	if err := e.validateListing(&next); err != nil {
		return err
	}
	*l = next
	e.touch("listings", listingID)
	return nil
}

func (e *Engine) SetListingStatus(actor, listingID ID, status string) error {
	l := e.listing(listingID)
	if l == nil {
		return fail("Listing tidak ditemukan.")
	}
	if l.OwnerID != actor && !e.isAdmin(actor) {
		return fail("Bukan listing milikmu.")
	}
	if status != "aktif" && status != "jeda" && status != "diturunkan" {
		return fail("Status tidak dikenal.")
	}
	if status == "diturunkan" && !e.isAdmin(actor) {
		return fail("Hanya admin yang bisa menurunkan listing.")
	}
	old := l.Status
	l.Status = status
	e.touch("listings", listingID)
	if e.isAdmin(actor) {
		e.audit(actor, "listing."+status, l.Title, ptr(old), ptr(status))
	}
	return nil
}

func (e *Engine) ToggleFavorite(userID, listingID ID) error {
	cur := e.d.Favorites[userID]
	out := make([]ID, 0, len(cur)+1)
	found := false
	for _, x := range cur {
		if x == listingID {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		out = append(out, listingID)
	}
	e.d.Favorites[userID] = out
	e.settings = true
	return nil
}

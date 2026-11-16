package domain

import (
	"fmt"
	"strings"
	"unicode"
)

// BannedWordsIn: keyword filter for listings (FR-L4), whole-word, case-insensitive. Text and keywords are folded the
// same way (foldKeywordRune: full-width forms, accents, Cyrillic/Greek look-alikes). The text is read twice: split
// at every non-letter/digit (the plain whole-word match), and compacted (punctuation and invisible characters inside
// a word removed, runs of single spaced letters joined), so "A.I.R.S.O.F.T", "air-soft", "a i r s o f t" or a
// zero-width space do not hide a keyword, while words that merely contain it ("airy software") still pass.
//
// Each reading is also checked with simple leetspeak decoded inside words that contain a letter (0→o, 1→i or l, 3→e,
// 4→a, 5→s, 7→t: "4irs0ft", "p1st0l") and with repeated letters collapsed ("kuuucing"; the keyword is collapsed the
// same way). Numbers on their own ("tenda 4 orang", "500 W") are left alone.
func BannedWordsIn(text string, banned []string) []string {
	folded := foldKeywordText(text)
	plain := keywordVariants(alnumWords(folded))
	compact := keywordVariants(compactWords(folded))
	var out []string
	for _, b := range banned {
		words := alnumWords(foldKeywordText(b))
		if len(words) == 0 {
			continue
		}
		if matchKeyword(words, plain, compact) {
			out = append(out, strings.ToLower(strings.TrimSpace(b)))
		}
	}
	return out
}

// keywordText is one reading of a text as " w1 w2 … " (collapsed: repeated letters squeezed).
type keywordText struct {
	text      string
	collapsed bool
}

func matchKeyword(words []string, plain, compact []keywordText) bool {
	for _, collapsed := range []bool{false, true} {
		kw := words
		if collapsed {
			kw = collapseWords(words)
		}
		phrase, glued := joinWords(kw), " "+strings.Join(kw, "")+" "
		for _, p := range plain {
			if p.collapsed == collapsed && strings.Contains(p.text, phrase) {
				return true
			}
		}
		for _, c := range compact {
			if c.collapsed == collapsed && (strings.Contains(c.text, phrase) || strings.Contains(c.text, glued)) {
				return true
			}
		}
	}
	return false
}

func joinWords(words []string) string { return " " + strings.Join(words, " ") + " " }

// leetDigits are the digit-for-letter swaps decoded inside words (L3); '1' is tried as both i and l.
var leetDigits = map[rune]rune{'0': 'o', '3': 'e', '4': 'a', '5': 's', '7': 't'}

// keywordVariants returns the readings of a word list: as written and with leetspeak decoded (1 = i, 1 = l), each
// also with repeated letters collapsed. Duplicates are dropped.
func keywordVariants(words []string) []keywordText {
	seen := map[keywordText]bool{}
	var out []keywordText
	add := func(ws []string) {
		for _, v := range []keywordText{{joinWords(ws), false}, {joinWords(collapseWords(ws)), true}} {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	add(words)
	add(decodeLeet(words, 'i'))
	add(decodeLeet(words, 'l'))
	return out
}

func decodeLeet(words []string, one rune) []string {
	out := make([]string, len(words))
	for i, w := range words {
		if strings.IndexFunc(w, func(r rune) bool { return r >= 'a' && r <= 'z' }) < 0 {
			out[i] = w // a number on its own
			continue
		}
		out[i] = strings.Map(func(r rune) rune {
			if r == '1' {
				return one
			}
			if to, ok := leetDigits[r]; ok {
				return to
			}
			return r
		}, w)
	}
	return out
}

// collapseWords squeezes runs of the same letter ("kuuucing" → "kucing").
func collapseWords(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		var b strings.Builder
		var last rune
		for _, r := range w {
			if r == last && r >= 'a' && r <= 'z' {
				continue
			}
			b.WriteRune(r)
			last = r
		}
		out[i] = b.String()
	}
	return out
}

func isKeywordRune(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') }

func alnumWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !isKeywordRune(r) })
}

// compactWords drops everything but letters, digits and spaces, then joins runs of one-letter words.
func compactWords(s string) []string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case isKeywordRune(r):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		}
	}
	var out []string
	run := ""
	for _, w := range strings.Fields(b.String()) {
		if len(w) == 1 {
			run += w
			continue
		}
		if run != "" {
			out, run = append(out, run), ""
		}
		out = append(out, w)
	}
	if run != "" {
		out = append(out, run)
	}
	return out
}

func foldKeywordText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		b.WriteRune(foldKeywordRune(r))
	}
	return b.String()
}

// keywordFold maps accented Latin letters and Cyrillic/Greek look-alikes (lower case) to ASCII.
var keywordFold = func() map[rune]rune {
	m := map[rune]rune{}
	for to, from := range map[rune]string{
		'a': "àáâãäåāăąǎạảấầẩẫậắằẳẵặаα", 'b': "вβ", 'c': "çćĉċčс", 'd': "ďđ", 'e': "èéêëēĕėęěẹẻẽếềểễệеεё",
		'g': "ĝğġģ", 'h': "ĥħн", 'i': "ìíîïĩīĭįıǐỉịіїι", 'j': "ĵј", 'k': "ķкκ", 'l': "ĺļľŀł", 'm': "м",
		'n': "ñńņňŉη", 'o': "òóôõöøōŏőǒọỏốồổỗộớờởỡợоο", 'p': "рρ", 'r': "ŕŗř", 's': "śŝşšѕ", 't': "ţťŧтτ",
		'u': "ùúûüũūŭůűųǔưụủứừửữựυ", 'v': "ν", 'w': "ŵ", 'x': "хχ", 'y': "ýÿŷỳỵỷỹу", 'z': "źżž",
	} {
		for _, r := range from {
			m[r] = to
		}
	}
	return m
}()

func foldKeywordRune(r rune) rune {
	if r >= 0xFF01 && r <= 0xFF5E { // full-width ASCII
		r = unicode.ToLower(r - 0xFEE0)
	}
	if to, ok := keywordFold[r]; ok {
		return to
	}
	return r
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
	if words := BannedWordsIn(strings.Join([]string{l.Title, l.Description, l.ConditionNotes, Deref(l.Rules.Special, "")}, "\n"), banned); len(words) > 0 {
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
const MaxRupiah int64 = 10_000_000_000 // Rp10 miliar

// HandoverMethods are the ways to hand an item over (web HandoverMethod in sewain-web/src/lib/types.ts): cash on
// delivery at a meeting point, pick-up at the provider, courier, and delivery by the provider. The listing wizard
// offers the first three; the CSV import accepts all four.
var HandoverMethods = []string{"cod", "jemput", "kurir", "antar"}

const (
	MaxRentalDays     = 365
	MaxListingStock   = 1000
	MaxListingRadius  = 50 // km, like the search and the CSV import
	maxListingPhotos  = 20
	maxCollateralOpts = 10
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
		if !Contains(HandoverMethods, h) {
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

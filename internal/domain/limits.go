package domain

import (
	"fmt"
	"unicode/utf8"
)

// Lengths of free text outside listings (review wave 2, L2), in characters (runes). Listings have their own
// limits (textLimits). The request body limit (1 MB) stays the outer bound.
const (
	MaxNameLen        = 80   // display name, store name
	MaxShortLen       = 120  // nickname, campus, wanted title, store hours
	MaxReasonLen      = 500  // decline/cancel/report reason, strike, account status, verification refusal
	MaxBioLen         = 500  // profile bio
	MaxNoteLen        = 1000 // booking note, proposal note, check-in problem note, wanted offer, report resolution
	MaxMessageLen     = 2000 // chat message, review comment
	MaxDescriptionLen = 2000 // dispute description, report detail, admin recommendation/outcome, wanted description
	MaxMeterLen       = 50   // odometer / fuel reading
)

// CheckLen refuses a text longer than limit characters: "<label> terlalu panjang (maks N karakter)."
func CheckLen(label, s string, limit int) error {
	if utf8.RuneCountInString(s) > limit {
		return Fail(fmt.Sprintf("%s terlalu panjang (maks %d karakter).", label, limit))
	}
	return nil
}

// CheckLenPtr is CheckLen for an optional text.
func CheckLenPtr(label string, s *string, limit int) error {
	if s == nil {
		return nil
	}
	return CheckLen(label, *s, limit)
}

// FirstErr returns the first non-nil error.
func FirstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ProposalAmountCap is the largest amount a dispute settlement of this type may ask for (PRD §11.3): a deposit cut
// up to the deposit held, repairs or replacement up to the item value in the agreement (market value of a used
// item, never more than the value both parties signed). ok=false: this type takes no amount.
func ProposalAmountCap(b *Booking, kind string) (limit int64, ok bool) {
	value := b.Agreement.ItemValue
	if value <= 0 || value > MaxRupiah {
		value = MaxRupiah
	}
	switch kind {
	case "tanpa-kompensasi":
		return 0, false
	case "potong-deposit":
		return b.Payment.Deposit, true
	}
	return value, true
}

// ValidateProposalAmount checks the amount of a settlement proposal or an admin recommendation against
// ProposalAmountCap. kind "" (admin recommendation) is capped by the item value or the deposit, whichever is larger.
func ValidateProposalAmount(b *Booking, kind string, amount *int64) error {
	if amount == nil {
		return nil
	}
	if *amount < 0 {
		return Fail("Nominal tidak boleh negatif.")
	}
	limit, ok := ProposalAmountCap(b, kind)
	if kind == "" {
		limit = max(limit, b.Payment.Deposit)
	}
	switch {
	case !ok && *amount > 0:
		return Fail("Solusi tanpa kompensasi tidak memakai nominal.")
	case kind == "potong-deposit" && limit <= 0 && *amount > 0:
		return Fail("Booking ini tidak memakai deposit.")
	case *amount > limit && kind == "potong-deposit":
		return Fail("Potongan deposit maksimal " + Rupiah(limit) + " (deposit yang dipegang).")
	case *amount > limit:
		return Fail("Nominal maksimal " + Rupiah(limit) + " (nilai barang di kesepakatan).")
	}
	return nil
}

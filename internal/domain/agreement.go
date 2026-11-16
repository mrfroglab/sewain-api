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

// BuildAgreement creates the rental agreement snapshot for a request and hashes it (FR-R2). The hash covers the
// JSON of the agreement before the hash and the agreement timestamps are set, exactly as in v1.
func BuildAgreement(now int64, l *Listing, renter, owner *User, in BookingInput, collateral CollateralOption, commission CommissionConfig) Agreement {
	quote := QuoteRent(l, in.StartDate, in.EndDate)
	ag := Agreement{
		CreatedAt: now, ListingID: l.ID, ListingTitle: l.Title, Category: l.Category, ItemValue: l.ItemValue,
		RenterName: PublicName(renter), ProviderName: PublicName(owner), StartDate: in.StartDate, EndDate: in.EndDate,
		Quote: quote, Collateral: collateral, CollateralValue: CollateralValue(collateral, l.ItemValue), Handover: in.Handover,
		LateFeePerDay: l.Rules.LateFeePerDay, LateFeeCap: int64(float64(l.ItemValue) * l.Rules.LateFeeCapPercent / 100),
		Cancellation: l.Rules.Cancellation, Special: l.Rules.Special, ConditionNotes: l.ConditionNotes, ReferencePhotos: append([]string{}, l.Photos...),
		Commission: ComputeCommission(quote.Total, commission),
	}
	ag.Hash = AgreementHash(ag)
	return ag
}

// AgreementHash is the SHA-256 of the agreement JSON without hash and agreement timestamps.
func AgreementHash(ag Agreement) string {
	ag.Hash, ag.RenterAgreedAt, ag.ProviderAgreedAt = "", nil, nil
	body, _ := json.Marshal(ag)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// BookingCode formats the human booking code, e.g. SWN-2601-0061.
func BookingCode(now int64, n int64) string {
	return fmt.Sprintf("SWN-%s-%04d", time.UnixMilli(now).In(WIB).Format("0601"), n)
}

// Payment is the (simulated) payment summary of a new booking.
func Payment(ag Agreement) PaymentInfo {
	deposit := DepositCash(ag.Collateral, ag.ItemValue)
	return PaymentInfo{Status: "belum", Rent: ag.Quote.Total, Deposit: deposit, Commission: ag.Commission.RenterPays,
		Total: ag.Quote.Total + deposit + ag.Commission.RenterPays}
}

// CleanNote trims an optional note; empty becomes nil.
func CleanNote(note *string) *string {
	if note != nil && strings.TrimSpace(*note) != "" {
		return Ptr(strings.TrimSpace(*note))
	}
	return nil
}

var (
	reLost    = regexp.MustCompile(`(?i)hilang|kurang`)
	reHeavy   = regexp.MustCompile(`(?i)berat|pecah|mati`)
	reScratch = regexp.MustCompile(`(?i)lecet|gores`)
)

// CaseTypeFor maps a check-in problem type to a dispute case type.
func CaseTypeFor(problemType string) string {
	switch {
	case reLost.MatchString(problemType):
		return "hilang"
	case reHeavy.MatchString(problemType):
		return "rusak-berat"
	case reScratch.MatchString(problemType):
		return "lecet-ringan"
	}
	return "rusak-ringan"
}

// ProposalLabel names the dispute settlement types (L1).
var ProposalLabel = map[string]string{
	"potong-deposit": "potong deposit", "ganti-perbaikan": "ganti biaya perbaikan", "ganti-nilai": "ganti rugi nilai barang",
	"perpanjang": "perpanjang waktu sewa", "tanpa-kompensasi": "tanpa kompensasi (wajar pakai)",
}

// ReviewVisibleAt (FR-T2): a review is shown once both parties reviewed, otherwise 7 days after it was written.
func ReviewVisibleAt(createdAt int64, counterpartReviewed bool) int64 {
	if counterpartReviewed {
		return createdAt
	}
	return createdAt + ReviewReleaseTTL
}

package payments

import (
	"math"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Commission bearers (FR-K3), same strings as domain.CommissionConfig.Bearer.
const (
	BearerRenter   = "penyewa"
	BearerProvider = "penyedia"
	BearerSplit    = "split"
)

// CommissionTerms are the commission rules frozen in a booking's agreement snapshot (FR-K5).
type CommissionTerms struct {
	Active             bool
	Mode               string  // persen | flat
	Percent            float64 // for persen, e.g. 10 = 10%
	Flat               int64   // for flat, rupiah
	Min, Max           *int64  // optional bounds (FR-K2)
	Bearer             string  // penyewa | penyedia | split
	SplitRenterPercent float64 // renter's share for split, e.g. 50
}

// Commission is the result for one booking. Amount is always computed ("komisi seandainya aktif", FR-K6);
// Charged is Amount when the terms are active, else 0. RenterPays + ProviderPays = Charged.
type Commission struct {
	Amount       int64 `json:"amount"`
	Charged      int64 `json:"charged"`
	RenterPays   int64 `json:"renterPays"`
	ProviderPays int64 `json:"providerPays"`
}

// TermsFromSnapshot reads the terms stored in an agreement. The snapshot keeps the flat amount in Rate for
// flat mode (domain.ComputeCommission).
func TermsFromSnapshot(s domain.CommissionSnapshot) CommissionTerms {
	t := CommissionTerms{Active: s.Active, Mode: s.Mode, Min: s.Min, Max: s.Max, Bearer: s.Bearer,
		SplitRenterPercent: s.SplitRenterPercent}
	if s.Mode == "persen" {
		t.Percent = s.Rate
	} else {
		t.Flat = int64(math.Round(s.Rate))
	}
	return t
}

// basisPoints turns a configured percentage (a rate, not money) into integer hundredths of a percent.
func basisPoints(percent float64) int64 { return int64(math.Round(percent * 100)) }

// mulBP returns amount × bp / 10000 rounded half up (amount and bp ≥ 0).
func mulBP(amount, bp int64) int64 { return (amount*bp + 5000) / 10000 }

// ComputeCommission computes the commission from the rent value only (FR-K4), never the deposit, in integer
// rupiah. It matches domain.ComputeCommission for every rent < 2^53 / 10000.
func ComputeCommission(rent int64, t CommissionTerms) Commission {
	if rent < 0 {
		rent = 0
	}
	var amount int64
	if t.Mode == "persen" {
		amount = mulBP(rent, max(0, basisPoints(t.Percent)))
	} else {
		amount = t.Flat
	}
	if t.Min != nil && amount < *t.Min {
		amount = *t.Min
	}
	if t.Max != nil && amount > *t.Max {
		amount = *t.Max
	}
	amount = max(0, amount)
	c := Commission{Amount: amount}
	if !t.Active {
		return c
	}
	c.Charged = amount
	switch t.Bearer {
	case BearerRenter:
		c.RenterPays = amount
	case BearerProvider:
		c.RenterPays = 0
	default: // split
		bp := min(10000, max(0, basisPoints(t.SplitRenterPercent)))
		c.RenterPays = mulBP(amount, bp)
	}
	c.ProviderPays = c.Charged - c.RenterPays
	return c
}

// CommissionForBooking returns what to charge for a booking paid through the gateway: recomputed from the
// agreement snapshot, never from the current config (FR-K5). gatewayActive=false (pilot, FR-K6) charges 0
// but keeps Amount for the "seandainya aktif" report.
//
// When the snapshot already holds a consistent charge (what both parties saw and agreed to), those exact
// numbers are used; ComputeCommission can differ by 1 rupiah on a split share because the snapshot was
// computed in floating point.
func CommissionForBooking(rent int64, snap domain.CommissionSnapshot, gatewayActive bool) Commission {
	t := TermsFromSnapshot(snap)
	t.Active = t.Active && gatewayActive
	c := ComputeCommission(rent, t)
	if t.Active && snap.Charged == snap.Amount && snap.Amount >= 0 && snap.RenterPays >= 0 && snap.ProviderPays >= 0 &&
		snap.RenterPays+snap.ProviderPays == snap.Charged {
		return Commission{Amount: snap.Amount, Charged: snap.Charged, RenterPays: snap.RenterPays, ProviderPays: snap.ProviderPays}
	}
	return c
}

// MDR returns the gateway fee for amount at bp basis points (QRIS 0.7% = 70), rounded half up.
func MDR(amount, bp int64) int64 {
	if amount <= 0 || bp <= 0 {
		return 0
	}
	return mulBP(amount, bp)
}

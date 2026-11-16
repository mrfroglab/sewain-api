package payments

import (
	"math/rand/v2"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

func i64(v int64) *int64 { return &v }

func TestComputeCommission(t *testing.T) {
	tests := []struct {
		name  string
		rent  int64
		terms CommissionTerms
		want  Commission
	}{
		{"persen penyewa", 75000, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Bearer: BearerRenter},
			Commission{Amount: 7500, Charged: 7500, RenterPays: 7500}},
		{"persen rounding half up", 12345, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Bearer: BearerRenter},
			Commission{Amount: 1235, Charged: 1235, RenterPays: 1235}},
		{"persen min", 10000, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Min: i64(2000), Bearer: BearerRenter},
			Commission{Amount: 2000, Charged: 2000, RenterPays: 2000}},
		{"persen max", 1_000_000, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Max: i64(50000), Bearer: BearerProvider},
			Commission{Amount: 50000, Charged: 50000, ProviderPays: 50000}},
		{"flat", 500000, CommissionTerms{Active: true, Mode: "flat", Flat: 3000, Bearer: BearerRenter},
			Commission{Amount: 3000, Charged: 3000, RenterPays: 3000}},
		{"flat with max below", 500000, CommissionTerms{Active: true, Mode: "flat", Flat: 3000, Max: i64(2500), Bearer: BearerRenter},
			Commission{Amount: 2500, Charged: 2500, RenterPays: 2500}},
		{"split 50:50 odd", 75010, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Bearer: BearerSplit, SplitRenterPercent: 50},
			Commission{Amount: 7501, Charged: 7501, RenterPays: 3751, ProviderPays: 3750}},
		{"split 30:70", 100000, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Bearer: BearerSplit, SplitRenterPercent: 30},
			Commission{Amount: 10000, Charged: 10000, RenterPays: 3000, ProviderPays: 7000}},
		{"inactive pilot keeps amount (FR-K6)", 75000, CommissionTerms{Active: false, Mode: "persen", Percent: 10, Min: i64(2000), Bearer: BearerRenter},
			Commission{Amount: 7500}},
		{"zero rent with min", 0, CommissionTerms{Active: true, Mode: "persen", Percent: 10, Min: i64(2000), Bearer: BearerRenter},
			Commission{Amount: 2000, Charged: 2000, RenterPays: 2000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeCommission(tt.rent, tt.terms)
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
			if got.RenterPays+got.ProviderPays != got.Charged {
				t.Fatal("shares do not add up")
			}
		})
	}
}

// The integer implementation agrees with domain.ComputeCommission (float), and the snapshot round-trips:
// commission is recomputed from the agreement snapshot, not the current config (FR-K5).
func TestCommissionMatchesDomainSnapshot(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		cfg := domain.CommissionConfig{Active: true, Mode: []string{"persen", "flat"}[r.IntN(2)], Percent: float64(r.IntN(2500)) / 100,
			Flat: r.Int64N(10000), Bearer: []string{BearerRenter, BearerProvider, BearerSplit}[r.IntN(3)], SplitRenterPercent: float64(r.IntN(101))}
		if r.IntN(2) == 0 {
			cfg.Min = i64(r.Int64N(5000))
		}
		if r.IntN(2) == 0 {
			cfg.Max = i64(5000 + r.Int64N(100000))
		}
		rent := r.Int64N(10_000_000)
		snap := domain.ComputeCommission(rent, cfg)
		// Integer recomputation: same amount, split shares within 1 rupiah of the float version.
		re := ComputeCommission(rent, TermsFromSnapshot(snap))
		if re.Amount != snap.Amount || re.Charged != snap.Charged || abs(re.RenterPays-snap.RenterPays) > 1 {
			t.Fatalf("rent %d cfg %+v: recomputed %+v snapshot %+v", rent, cfg, re, snap)
		}
		got := CommissionForBooking(rent, snap, true)
		if got.Amount != snap.Amount || got.Charged != snap.Charged || got.RenterPays != snap.RenterPays || got.ProviderPays != snap.ProviderPays {
			t.Fatalf("rent %d cfg %+v: got %+v snapshot %+v", rent, cfg, got, snap)
		}
		// Changing the live config afterwards does not matter: only the snapshot is read.
		if off := CommissionForBooking(rent, snap, false); off.Charged != 0 || off.Amount != snap.Amount {
			t.Fatalf("gateway inactive should charge 0 and keep amount: %+v", off)
		}
	}
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestMDR(t *testing.T) {
	for _, c := range []struct{ amount, bp, want int64 }{{150000, 70, 1050}, {175000, 70, 1225}, {200000, 70, 1400}, {1, 70, 0}, {72, 70, 1}, {0, 70, 0}, {1000, 0, 0}} {
		if got := MDR(c.amount, c.bp); got != c.want {
			t.Errorf("MDR(%d,%d)=%d want %d", c.amount, c.bp, got, c.want)
		}
	}
}

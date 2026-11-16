package payments

import (
	"errors"
	"testing"
)

// Base booking: rent Rp100.000, commission Rp10.000 split 50:50, deposit Rp200.000. Renter paid
// 105.000 + 200.000; escrow holds 95.000 (rent − provider share) until handover.
func baseFunds() Funds {
	return Funds{BookingID: "b1", RenterID: "r1", ProviderID: "p1", Escrow: 95000, Deposit: 200000, RentPaid: 100000,
		Commission: Commission{Amount: 10000, Charged: 10000, RenterPays: 5000, ProviderPays: 5000}}
}

func afterHandover() Funds { f := baseFunds(); f.Escrow = 0; return f }

// TestPlanSettlementCases covers every row of PRD §11.3 plus a clean completion.
func TestPlanSettlementCases(t *testing.T) {
	type want struct{ toProvider, depositRefund, released, rentRefund, reversed, shortfall int64 }
	tests := []struct {
		name  string
		funds Funds
		out   Outcome
		want  want
	}{
		{"selesai tanpa masalah: deposit kembali penuh", afterHandover(), Outcome{Case: CaseCompleted}, want{depositRefund: 200000}},
		{"selesai, escrow belum dilepas: sewa ke penyedia", baseFunds(), Outcome{Case: CaseCompleted}, want{depositRefund: 200000, released: 95000}},
		{"lecet ringan: wajar pakai", afterHandover(), Outcome{Case: CaseWearAndTear}, want{depositRefund: 200000}},
		{"rusak ringan: potong biaya perbaikan", afterHandover(), Outcome{Case: CaseRepairable, Claim: 75000}, want{toProvider: 75000, depositRefund: 125000}},
		{"rusak ringan: perbaikan > deposit jadi tagihan", afterHandover(), Outcome{Case: CaseRepairable, Claim: 260000}, want{toProvider: 200000, shortfall: 60000}},
		{"rusak ringan: perbaikan = deposit", afterHandover(), Outcome{Case: CaseRepairable, Claim: 200000}, want{toProvider: 200000}},
		{"rusak berat: nilai pasar bekas", afterHandover(), Outcome{Case: CaseHeavyDamage, Claim: 1500000}, want{toProvider: 200000, shortfall: 1300000}},
		{"hilang: nilai pasar bekas", afterHandover(), Outcome{Case: CaseLost, Claim: 900000}, want{toProvider: 200000, shortfall: 700000}},
		{"telat: denda di bawah batas", afterHandover(), Outcome{Case: CaseLate, Claim: 40000, ClaimCap: 300000}, want{toProvider: 40000, depositRefund: 160000}},
		{"telat: denda dipotong batas kesepakatan", afterHandover(), Outcome{Case: CaseLate, Claim: 500000, ClaimCap: 250000}, want{toProvider: 200000, shortfall: 50000}},
		{"tidak dikembalikan: deposit kompensasi awal", afterHandover(), Outcome{Case: CaseNotReturned, Claim: 2000000}, want{toProvider: 200000, shortfall: 1800000}},
		{"jaminan ditahan penyedia: deposit uang kembali", afterHandover(), Outcome{Case: CaseCollateralKept}, want{depositRefund: 200000}},
		{"tidak sesuai deskripsi: batal tanpa penalti", baseFunds(), Outcome{Case: CaseNotAsDescribed}, want{depositRefund: 200000, rentRefund: 95000, reversed: 10000}},
		{"penyedia batal sepihak", baseFunds(), Outcome{Case: CaseProviderCancel}, want{depositRefund: 200000, rentRefund: 95000, reversed: 10000}},
		{"penyewa tidak datang", baseFunds(), Outcome{Case: CaseRenterNoShow}, want{depositRefund: 200000, rentRefund: 95000, reversed: 10000}},
		{"deposit ditahan semena-mena", afterHandover(), Outcome{Case: CaseDepositWithheld}, want{depositRefund: 200000}},
		{"kendaraan: tilang", afterHandover(), Outcome{Case: CaseVehicleIncident, Claim: 250000}, want{toProvider: 200000, shortfall: 50000}},
		{"klaim palsu: tidak ada uang bergerak selain pengembalian", afterHandover(), Outcome{Case: CaseFalseClaim}, want{depositRefund: 200000}},
		{"tanpa deposit uang: klaim jadi tagihan penuh", func() Funds { f := afterHandover(); f.Deposit = 0; return f }(), Outcome{Case: CaseRepairable, Claim: 80000}, want{shortfall: 80000}},
		{"refund sewa parsial dari escrow", baseFunds(), Outcome{Case: CaseCompleted, RentRefund: 20000}, want{depositRefund: 200000, released: 75000, rentRefund: 20000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := PlanSettlement(tt.funds, tt.out)
			if err != nil {
				t.Fatal(err)
			}
			got := want{s.DepositToProvider, s.DepositRefund, s.RentReleased, s.RentRefund, s.CommissionReversed, s.Shortfall}
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
			if Sum(s.Entries) != 0 {
				t.Fatalf("unbalanced: %v", s.Entries)
			}
			b := Balances(s.Entries)
			// Holding accounts are emptied exactly.
			if b["escrow:b1"] != -tt.funds.Escrow || b["deposit:b1"] != -tt.funds.Deposit {
				t.Fatalf("holding accounts not emptied: %v", b)
			}
			if b["renter:r1"] != tt.want.depositRefund+tt.want.rentRefund+tt.want.reversed {
				t.Fatalf("renter gets %d", b["renter:r1"])
			}
			if b["provider:p1"] != tt.want.toProvider+tt.want.released {
				t.Fatalf("provider gets %d", b["provider:p1"])
			}
			// Deterministic.
			again, _ := PlanSettlement(tt.funds, tt.out)
			if len(again.Entries) != len(s.Entries) {
				t.Fatal("plan not deterministic")
			}
			for i := range again.Entries {
				if again.Entries[i] != s.Entries[i] {
					t.Fatal("plan not deterministic")
				}
			}
		})
	}
}

func TestPlanSettlementRejects(t *testing.T) {
	bad := []struct {
		name  string
		funds Funds
		out   Outcome
	}{
		{"unknown case", afterHandover(), Outcome{Case: "entah"}},
		{"claim on a no-claim case", afterHandover(), Outcome{Case: CaseWearAndTear, Claim: 1}},
		{"negative claim", afterHandover(), Outcome{Case: CaseRepairable, Claim: -1}},
		{"rent refund above escrow", afterHandover(), Outcome{Case: CaseCompleted, RentRefund: 1}},
		{"cancel after handover", afterHandover(), Outcome{Case: CaseProviderCancel}},
		{"cancel with claim", baseFunds(), Outcome{Case: CaseNotAsDescribed, Claim: 10}},
	}
	for _, tt := range bad {
		if _, err := PlanSettlement(tt.funds, tt.out); !errors.Is(err, ErrBadOutcome) {
			t.Errorf("%s: err %v", tt.name, err)
		}
	}
}

func TestPlanPayment(t *testing.T) {
	es, err := PlanPayment(PaidIntent{Kind: KindRent, Amount: 105000, BookingID: "b1", Provider: "xendit",
		CommissionRenter: 5000, CommissionProvider: 5000, Fee: 735})
	if err != nil {
		t.Fatal(err)
	}
	b := Balances(es)
	if b["escrow:b1"] != 95000 || b[AccountCommission] != 10000 || b[AccountFees] != -735 || b["gateway:xendit"] != -105000+735 || Sum(es) != 0 {
		t.Fatalf("%v", b)
	}
	es, _ = PlanPayment(PaidIntent{Kind: KindClaim, Amount: 60000, BookingID: "b1", ProviderID: "p1", Provider: "xendit", Fee: 420})
	if b := Balances(es); b["provider:p1"] != 60000 || Sum(es) != 0 {
		t.Fatalf("%v", b)
	}
	if _, err := PlanPayment(PaidIntent{Kind: "x", Amount: 1, Provider: "xendit"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := PlanPayment(PaidIntent{Kind: KindRent, Amount: 100, CommissionRenter: 80, CommissionProvider: 80, Provider: "x"}); err == nil {
		t.Fatal("commission above rent accepted")
	}
}

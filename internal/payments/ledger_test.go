package payments

import (
	"math/rand/v2"
	"testing"
)

func TestTxnValidate(t *testing.T) {
	ok := Txn{ID: "t1", Entries: move("gateway:fake", "escrow:b1", 100)}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Txn{
		{Entries: ok.Entries},
		{ID: "t", Entries: []Entry{{Account: "escrow:b", Amount: 5}}},
		{ID: "t", Entries: []Entry{{Account: "escrow:b", Amount: 5}, {Account: "provider:u", Amount: -4}}},
		{ID: "t", Entries: []Entry{{Account: "bank:x", Amount: 5}, {Account: "provider:u", Amount: -5}}},
		{ID: "t", Entries: []Entry{{Account: "escrow:", Amount: 5}, {Account: "provider:u", Amount: -5}}},
		{ID: "t", Entries: []Entry{{Account: "escrow:b", Amount: 0}, {Account: "provider:u", Amount: 0}}},
	}
	for i, b := range bad {
		if b.Validate() == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

// TestLedgerAlwaysBalancedProperty runs random booking lives (payment, release, settlement of a random
// case) and checks that every plan sums to zero, no holding account goes negative and money is conserved.
func TestLedgerAlwaysBalancedProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 7))
	cases := make([]string, 0, len(caseRules))
	for c := range caseRules {
		cases = append(cases, c)
	}
	for iter := 0; iter < 5000; iter++ {
		rent := r.Int64N(5_000_000) + 1000
		deposit := r.Int64N(3) * r.Int64N(5_000_000)
		terms := CommissionTerms{Active: r.IntN(2) == 0, Mode: []string{"persen", "flat"}[r.IntN(2)],
			Percent: float64(r.IntN(3000)) / 100, Flat: r.Int64N(20000), Bearer: []string{BearerRenter, BearerProvider, BearerSplit}[r.IntN(3)],
			SplitRenterPercent: float64(r.IntN(101))}
		c := ComputeCommission(rent, terms)
		if c.ProviderPays > rent {
			continue // nonsensical config (flat commission above the rent); rejected by CreateIntent's caller
		}
		fee := MDR(rent+c.RenterPays, 70)
		var all [][]Entry
		pay, err := PlanPayment(PaidIntent{Kind: KindRent, Amount: rent + c.RenterPays, BookingID: "b", Provider: "fake",
			CommissionRenter: c.RenterPays, CommissionProvider: c.ProviderPays, Fee: fee})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, pay)
		if deposit > 0 {
			dp, err := PlanPayment(PaidIntent{Kind: KindDeposit, Amount: deposit, BookingID: "b", Provider: "fake", Fee: MDR(deposit, 70)})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, dp)
		}
		bal := Balances(all...)
		f := Funds{BookingID: "b", RenterID: "r", ProviderID: "p", Escrow: bal["escrow:b"], Deposit: bal["deposit:b"], RentPaid: rent, Commission: c}
		cs := cases[r.IntN(len(cases))]
		released := false
		if caseRules[cs] != ruleFullRefund && r.IntN(2) == 0 {
			rel := PlanRelease(f)
			all = append(all, rel)
			f.Escrow = 0
			released = true
		}
		o := Outcome{Case: cs}
		if caseRules[cs] == ruleClaim {
			o.Claim = r.Int64N(2 * (deposit + 1))
			if r.IntN(3) == 0 {
				o.ClaimCap = r.Int64N(deposit + 1)
			}
		}
		if caseRules[cs] != ruleFullRefund && !released && r.IntN(3) == 0 {
			o.RentRefund = r.Int64N(f.Escrow + 1)
		}
		s, err := PlanSettlement(f, o)
		if err != nil {
			t.Fatalf("iter %d case %s: %v (funds %+v outcome %+v)", iter, cs, err, f, o)
		}
		if Sum(s.Entries) != 0 {
			t.Fatalf("iter %d: unbalanced settlement %+v", iter, s)
		}
		all = append(all, s.Entries)
		for _, es := range all {
			if Sum(es) != 0 {
				t.Fatalf("iter %d: unbalanced txn %v", iter, es)
			}
		}
		end := Balances(all...)
		if end["escrow:b"] != 0 || end["deposit:b"] != 0 {
			t.Fatalf("iter %d case %s: holding accounts not emptied: %v", iter, cs, end)
		}
		var total int64
		for a, v := range end {
			total += v
			if v < 0 && a != AccountFees && a != "gateway:fake" && a != AccountCommission {
				t.Fatalf("iter %d: negative %s=%d", iter, a, v)
			}
		}
		if total != 0 {
			t.Fatalf("iter %d: money not conserved: %d", iter, total)
		}
		// Renter + provider + platform receive exactly what came in.
		if got := end["renter:r"] + end["provider:p"] + end[AccountCommission]; got != rent+c.RenterPays+deposit {
			t.Fatalf("iter %d case %s: distributed %d, received %d", iter, cs, got, rent+c.RenterPays+deposit)
		}
	}
}

func TestCompactDeterministic(t *testing.T) {
	es := Compact([]Entry{{"b:1", 5}, {"a:1", -3}, {"b:1", -5}, {"a:1", 3}, {"c:1", 1}, {"a:1", -1}})
	if len(es) != 2 || es[0] != (Entry{"a:1", -1}) || es[1] != (Entry{"c:1", 1}) {
		t.Fatalf("%v", es)
	}
}

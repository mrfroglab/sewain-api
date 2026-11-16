package domain_test

import (
	"math"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

func TestCommission(t *testing.T) {
	min, max := int64(2000), int64(50000)
	c := domain.CommissionConfig{Active: false, Mode: "persen", Percent: 10, Min: &min, Max: &max, Bearer: "split", SplitRenterPercent: 50}
	s := domain.ComputeCommission(15000, c)
	if s.Amount != 2000 || s.Charged != 0 {
		t.Fatalf("min applies and nothing is charged in pilot: %+v", s)
	}
	c.Active = true
	s = domain.ComputeCommission(1_000_000, c)
	if s.Amount != 50000 || s.RenterPays != 25000 || s.ProviderPays != 25000 {
		t.Fatalf("max + 50:50 split: %+v", s)
	}
}

func TestQuoteRentWeeklyAndDiscount(t *testing.T) {
	week := int64(200000)
	l := &domain.Listing{PricePerDay: 35000, PricePerWeek: &week, DurationDiscount: &domain.DurationDiscount{MinDays: 3, Percent: 10}}
	q := domain.QuoteRent(l, "2026-11-01", "2026-11-09") // 8 days = 1 week + 1 day
	if q.Days != 8 || q.Total != int64(float64(200000+35000)*0.9) {
		t.Fatalf("unexpected quote %+v", q)
	}
}

func TestBannedKeywords(t *testing.T) {
	if got := domain.BannedWordsIn("Airsoft gun replika", []string{"airsoft", "senjata"}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got := domain.BannedWordsIn("Pisau dapur", []string{"senjata"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestAgreementHashIgnoresAgreementTimestamps(t *testing.T) {
	ag := domain.Agreement{ListingID: "l-1", ReferencePhotos: []string{}}
	h := domain.AgreementHash(ag)
	ag.RenterAgreedAt, ag.ProviderAgreedAt, ag.Hash = domain.Ptr(int64(1)), domain.Ptr(int64(2)), "x"
	if domain.AgreementHash(ag) != h || len(h) != 64 {
		t.Fatal("hash must cover only the agreement terms")
	}
	ag.ItemValue = 1
	if domain.AgreementHash(ag) == h {
		t.Fatal("hash must change with the terms")
	}
}

func TestPublicLocationGrid(t *testing.T) {
	lat, lng := -7.7623999, 110.3699
	plat, plng := domain.PublicLocation(lat, lng)
	if plat == lat || plng == lng {
		t.Fatal("public location must differ from the precise one")
	}
	if d := domain.DistanceMeters(lat, lng, plat, lng); d > 251 {
		t.Fatalf("latitude moved %.0f m (> 250)", d)
	}
	if d := domain.DistanceMeters(lat, lng, lat, plng); d > 251 {
		t.Fatalf("longitude moved %.0f m (> 250)", d)
	}
	// Stable: nearby points in the same cell map to the same public point.
	a, b := domain.PublicLocation(lat+0.00001, lng+0.00001)
	if math.Abs(a-plat) > 1e-9 || math.Abs(b-plng) > 1e-9 {
		t.Fatal("grid snapping must be stable")
	}
}

func TestCaseTypeAndSuggestion(t *testing.T) {
	cases := map[string]string{"Barang hilang": "hilang", "Rusak berat": "rusak-berat", "Lecet": "lecet-ringan", "Kotor": "rusak-ringan"}
	for in, want := range cases {
		if got := domain.CaseTypeFor(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	r := domain.CollateralRules{LowMax: 500000, MidMax: 2000000, MidDepositPercent: 25, HighDepositPercent: 40, VehicleDepositPercent: 30}
	if s := domain.SuggestCollateral(100000, "camping", r); s.Level != "rendah" || s.MinCoverage != 0 {
		t.Fatalf("%+v", s)
	}
	if s := domain.SuggestCollateral(1_200_000, "camping", r); s.Level != "sedang" || s.MinCoverage != 300000 {
		t.Fatalf("%+v", s)
	}
	if s := domain.SuggestCollateral(10_000_000, "kendaraan", r); s.Level != "kendaraan" || len(s.Preset) != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestPickID(t *testing.T) {
	none := func(string) bool { return false }
	if got := domain.PickID("b-testapi12345", "b", none); got != "b-testapi12345" {
		t.Fatal("valid client id must be kept")
	}
	if got := domain.PickID("x-testapi12345", "b", none); got == "x-testapi12345" {
		t.Fatal("wrong prefix must be replaced")
	}
	if got := domain.PickID("b-testapi12345", "b", func(string) bool { return true }); got == "b-testapi12345" {
		t.Fatal("used id must be replaced")
	}
}

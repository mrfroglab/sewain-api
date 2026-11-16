package service_test

import (
	"math"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/geo"
	"github.com/mrfroglab/sewain-api/internal/service"
)

func hitIDs(r service.SearchResult) map[string]bool {
	m := map[string]bool{}
	for _, h := range r.Hits {
		m[h.ID] = true
	}
	return m
}

// TestSearch (F1.9): two-way radius, filters, full-text and stock-aware date availability.
func TestSearch(t *testing.T) {
	e := newEnv(t)
	ugm, err := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(ugm.Hits) <= 30 {
		t.Fatalf("UGM 3 km: %d hits", len(ugm.Hits))
	}
	for i := 1; i < len(ugm.Hits); i++ {
		if ugm.Hits[i].DistanceKm < ugm.Hits[i-1].DistanceKm {
			t.Fatal("hits must be ordered by distance")
		}
	}
	// Two-way rule from Pogung, checked against independently computed distances (away from the edges, where the
	// spheroid and haversine distances may disagree by a few meters). Same result as v1: scripts/compare-v1.sh.
	plat, plng := -7.7596, 110.3727
	pogung, _ := e.svc.Search(e.ctx, service.SearchParams{Lat: plat, Lng: plng, RadiusKm: 3})
	hits, hidden := hitIDs(pogung), 0
	for _, l := range e.state().Listings {
		if l.Status != "aktif" {
			continue
		}
		d := geo.DistanceKm(plat, plng, l.Location.Lat, l.Location.Lng) * 1000
		reach := math.Min(3000, l.RadiusKm*1000)
		switch {
		case math.Abs(d-reach) < 30 || math.Abs(d-3000) < 30:
		case d <= reach && !hits[l.ID]:
			t.Errorf("%s (%.0f m, reach %.0f m) must be visible", l.ID, d, reach)
		case d > reach && hits[l.ID]:
			t.Errorf("%s (%.0f m, reach %.0f m) must be hidden", l.ID, d, reach)
		case d > reach && d <= 3000:
			hidden++
		}
	}
	if pogung.HiddenByProviderReach == 0 || pogung.HiddenByProviderReach != hidden {
		t.Fatalf("hidden by provider reach: got %d, want %d", pogung.HiddenByProviderReach, hidden)
	}
	cam, _ := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 5, Category: "kamera", Collateral: "cukup-identitas"})
	if len(cam.Hits) == 0 {
		t.Fatal("category + collateral filter")
	}
	toko, _ := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 5, ProviderType: "toko"})
	for id := range hitIDs(toko) {
		if l := e.listing(id); l.OwnerID != "u-bayu" {
			t.Fatalf("toko filter returned %s", id)
		}
	}
	// Full-text with prefix matching, user input cannot inject tsquery syntax.
	q, _ := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 5, Q: "tend dome"})
	if !hitIDs(q)["l-tenda-dome-4p"] || len(q.Hits) > 3 {
		t.Fatalf("full-text: %v", hitIDs(q))
	}
	if _, err := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 5, Q: "a & | ! :* ("}); err != nil {
		t.Fatalf("hostile query: %v", err)
	}
	if service.TSQuery("Tenda, dome!") != "tenda:* & dome:*" {
		t.Fatal(service.TSQuery("Tenda, dome!"))
	}

	// Availability: a stock-1 listing booked (ACCEPTED) on some dates disappears for overlapping dates only.
	l := e.listing("l-kamera-mirrorless")
	id := e.must("u-raka", "createBooking", "u-raka", bookingInput(l, e.today(10), e.today(12))).(string)
	e.must("u-dina", "acceptBooking", "u-dina", id)
	at := func(start, end string) bool {
		r, err := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 5, StartDate: start, EndDate: end})
		if err != nil {
			t.Fatal(err)
		}
		return hitIDs(r)[l.ID]
	}
	if at(e.today(11), e.today(13)) {
		t.Fatal("booked dates must hide the listing")
	}
	if !at(e.today(13), e.today(14)) {
		t.Fatal("free dates must show the listing")
	}
	// Toko stock: l-tenda-dome-4p has stock 3; one acceptance leaves it available.
	tenda := e.listing("l-tenda-dome-4p")
	e.must("u-raka", "createBooking", "u-raka", bookingInput(tenda, e.today(10), e.today(12)))
	r, _ := e.svc.Search(e.ctx, service.SearchParams{Lat: -7.7713, Lng: 110.3776, RadiusKm: 5, StartDate: e.today(10), EndDate: e.today(12)})
	if !hitIDs(r)["l-tenda-dome-4p"] {
		t.Fatal("stock left: listing stays available")
	}
}

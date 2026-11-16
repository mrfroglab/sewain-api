package service_test

import (
	"math"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
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
	// Two-way rule from Pogung, checked against independently computed distances from the PUBLIC point of each
	// listing (review C1: the search never measures from the precise location).
	plat, plng := -7.7596, 110.3727
	pogung, _ := e.svc.Search(e.ctx, service.SearchParams{Lat: plat, Lng: plng, RadiusKm: 3})
	hits, hidden := hitIDs(pogung), 0
	for _, l := range e.state().Listings {
		if l.Status != "aktif" {
			continue
		}
		flat, flng := geo.FuzzPoint(l.ID, l.Location.Lat, l.Location.Lng)
		d := geo.DistanceKm(plat, plng, flat, flng) * 1000
		reach := math.Min(3000, l.RadiusKm*1000)
		switch {
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

func TestSearchRadiusSnapsToOptions(t *testing.T) {
	cases := map[float64]float64{0.2: 1, 1: 1, 1.01: 3, 2.5: 3, 4: 5, 7: 10, 12: 25, 40: 25}
	for in, want := range cases {
		if got := service.SnapRadiusKm(in); got != want {
			t.Errorf("SnapRadiusKm(%v) = %v, want %v", in, got, want)
		}
	}
}

// TestSearchCannotTrilateratePreciseLocation plays the attack from review C1: binary-search the edge of the 1 km
// search circle along three rays, then take the circumcenter of the three edge points. Before the fix this recovered
// the precise location; now it recovers the public (fuzzed) point, which is > 150 m away from the precise one.
func TestSearchCannotTrilateratePreciseLocation(t *testing.T) {
	e := newEnv(t)
	var target *domain.Listing
	var pubLat, pubLng float64
	for _, l := range e.state().Listings {
		if l.Status != "aktif" || l.RadiusKm < 2 {
			continue
		}
		la, ln := geo.FuzzPoint(l.ID, l.Location.Lat, l.Location.Lng)
		if geo.DistanceKm(la, ln, l.Location.Lat, l.Location.Lng) > 0.2 {
			target, pubLat, pubLng = l, la, ln
			break
		}
	}
	if target == nil {
		t.Fatal("no seed listing with a public point > 200 m from its precise point")
	}
	// Local plane (meters) around the public point.
	const m = 111_320.0
	cos := math.Cos(pubLat * math.Pi / 180)
	toLatLng := func(x, y float64) (float64, float64) { return pubLat + y/m, pubLng + x/(m*cos) }
	toXY := func(lat, lng float64) (float64, float64) { return (lng - pubLng) * m * cos, (lat - pubLat) * m }
	member := func(x, y float64) bool {
		lat, lng := toLatLng(x, y)
		r, err := e.svc.Search(e.ctx, service.SearchParams{Lat: lat, Lng: lng, RadiusKm: 1, Category: target.Category})
		if err != nil {
			t.Fatal(err)
		}
		return hitIDs(r)[target.ID]
	}
	var pts [3][2]float64
	for i := range pts {
		ux, uy := math.Cos(float64(i)*2*math.Pi/3), math.Sin(float64(i)*2*math.Pi/3)
		lo, hi := 0.0, 2000.0 // inside at the public point, outside 2 km away
		for hi-lo > 1 {
			mid := (lo + hi) / 2
			if member(ux*mid, uy*mid) {
				lo = mid
			} else {
				hi = mid
			}
		}
		pts[i] = [2]float64{ux * lo, uy * lo}
	}
	// Circumcenter of the three edge points = the center the search measures from.
	ax, ay, bx, by, cx, cy := pts[0][0], pts[0][1], pts[1][0], pts[1][1], pts[2][0], pts[2][1]
	d := 2 * (ax*(by-cy) + bx*(cy-ay) + cx*(ay-by))
	ex := ((ax*ax+ay*ay)*(by-cy) + (bx*bx+by*by)*(cy-ay) + (cx*cx+cy*cy)*(ay-by)) / d
	ey := ((ax*ax+ay*ay)*(cx-bx) + (bx*bx+by*by)*(ax-cx) + (cx*cx+cy*cy)*(bx-ax)) / d
	rx, ry := toXY(target.Location.Lat, target.Location.Lng)
	toReal, toPublic := math.Hypot(ex-rx, ey-ry), math.Hypot(ex, ey)
	if toPublic > 30 {
		t.Fatalf("estimate should land on the public point: %.0f m away", toPublic)
	}
	if toReal <= 150 {
		t.Fatalf("precise location recovered within %.0f m", toReal)
	}
}

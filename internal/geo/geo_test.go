package geo

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

func TestFuzzPointBoundsAndStability(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 20000 {
		// Mostly around Yogyakarta, plus some points elsewhere on the globe.
		lat, lng := -7.77+rng.Float64()*0.1-0.05, 110.37+rng.Float64()*0.1-0.05
		if i%4 == 0 {
			lat, lng = rng.Float64()*160-80, rng.Float64()*360-180
		}
		id := fmt.Sprintf("l-%d", i)
		flat, flng := FuzzPoint(id, lat, lng)
		d := DistanceKm(lat, lng, flat, flng) * 1000
		if d > MaxErrorMeters {
			t.Fatalf("%s (%f,%f) → (%f,%f): %.1f m > %v m", id, lat, lng, flat, flng, d, MaxErrorMeters)
		}
		if flat == lat && flng == lng || d < 1 {
			t.Fatalf("%s: public point equals the true point", id)
		}
		if a, b := FuzzPoint(id, lat, lng); a != flat || b != flng {
			t.Fatalf("%s: not stable", id)
		}
	}
}

func TestFuzzPointDoesNotLeakPrecisePoint(t *testing.T) {
	// Two different true points in the same cell give the same public point for one listing: the output
	// carries no information beyond the cell.
	lat, lng := -7.7700, 110.3770
	a1, b1 := FuzzPoint("l-1", lat, lng)
	moved := false
	for _, d := range []float64{0.0003, 0.0006, -0.0003, -0.0006} {
		a2, b2 := FuzzPoint("l-1", lat+d, lng+d)
		if a2 == a1 && b2 == b1 {
			moved = true
		}
	}
	if !moved {
		t.Fatal("no nearby point in the same cell produced the same output")
	}

	// Averaging the public points of many listings at one location lands near the cell centre, not on the
	// true point: the error stays at cell scale.
	lat, lng = -7.771234, 110.378901
	var sLat, sLng float64
	n := 2000
	for i := range n {
		a, b := FuzzPoint(fmt.Sprintf("l-%d", i), lat, lng)
		sLat += a
		sLng += b
	}
	avgLat, avgLng := sLat/float64(n), sLng/float64(n)
	latStep := CellMeters / metersPerDeg
	cLat := (math.Floor(lat/latStep) + 0.5) * latStep
	lngStep := CellMeters / (metersPerDeg * math.Cos(cLat*math.Pi/180))
	cLng := (math.Floor(lng/lngStep) + 0.5) * lngStep
	toCentre := DistanceKm(avgLat, avgLng, cLat, cLng) * 1000
	toTruth := DistanceKm(avgLat, avgLng, lat, lng) * 1000
	if toCentre > 15 {
		t.Fatalf("average is %.1f m from the cell centre", toCentre)
	}
	if math.Abs(toTruth-DistanceKm(cLat, cLng, lat, lng)*1000) > 15 {
		t.Fatalf("average converges towards the truth: %.1f m", toTruth)
	}
	// Rounded to 6 decimals.
	if a, _ := FuzzPoint("l-1", lat, lng); math.Abs(a*1e6-math.Round(a*1e6)) > 1e-6 {
		t.Fatalf("not rounded: %v", a)
	}
}

func TestFuzzPointEdgeInputs(t *testing.T) {
	for _, p := range [][2]float64{{math.NaN(), 1}, {1, math.Inf(1)}} {
		if a, b := FuzzPoint("l", p[0], p[1]); a != 0 || b != 0 {
			t.Fatalf("bad input leaked: %v %v", a, b)
		}
	}
	a, b := FuzzPoint("l", 0, 179.9999)
	if a < -90 || a > 90 || b < -181 || b > 181 {
		t.Fatalf("out of range: %v %v", a, b)
	}
}

func TestRoundDistanceKm(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
		str  string
	}{
		{0, 0.5, "± 0,5 km"},
		{0.12, 0.5, "± 0,5 km"},
		{0.74, 0.7, "± 0,7 km"},
		{1.234, 1.2, "± 1,2 km"},
		{1.25, 1.3, "± 1,3 km"},
		{2, 2, "± 2,0 km"},
		{9.96, 10, "± 10 km"},
		{12.4, 12, "± 12 km"},
		{-1, 0, "± 0,0 km"},
	}
	for _, c := range cases {
		if got := RoundDistanceKm(c.in); got != c.want {
			t.Errorf("RoundDistanceKm(%v) = %v, want %v", c.in, got, c.want)
		}
		if got := FormatDistance(c.in); got != c.str {
			t.Errorf("FormatDistance(%v) = %q, want %q", c.in, got, c.str)
		}
	}
}

func TestDistanceKm(t *testing.T) {
	// UGM Bulaksumur → Tugu Yogyakarta ≈ 1.7 km.
	d := DistanceKm(-7.7713, 110.3775, -7.7829, 110.3671)
	if d < 1.5 || d > 2.0 {
		t.Fatalf("got %.3f km", d)
	}
}

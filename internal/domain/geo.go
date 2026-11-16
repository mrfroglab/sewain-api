package domain

import "math"

// PublicGridMeters is the cell size of the public location grid. Snapping to the cell centre keeps every public
// point within ±250 m (per axis) of the real one and is stable: the same listing always shows the same point (FR-S6).
const PublicGridMeters = 500.0

const metersPerDegreeLat = 111_320.0

// PublicLocation rounds a precise point to the centre of its grid cell. The media module may replace this with its
// own implementation (F3.5); callers only rely on the signature.
func PublicLocation(lat, lng float64) (float64, float64) {
	latStep := PublicGridMeters / metersPerDegreeLat
	rlat := (math.Floor(lat/latStep) + 0.5) * latStep
	// Longitude cells use the cosine of the (already rounded) latitude so the step is stable within a row.
	lngStep := PublicGridMeters / (metersPerDegreeLat * math.Cos(rlat*math.Pi/180))
	rlng := (math.Floor(lng/lngStep) + 0.5) * lngStep
	return round6(rlat), round6(rlng)
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// DistanceMeters is the great-circle distance between two points (haversine).
func DistanceMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6_371_000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lng2-lng1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

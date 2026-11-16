// Package geo hides precise listing locations from the public (FR-S6).
package geo

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

const (
	earthRadiusM = 6371008.8
	metersPerDeg = math.Pi * earthRadiusM / 180

	// CellMeters is the grid cell edge. The cell center is at most CellMeters/√2 ≈ 283 m from any point
	// in the cell, i.e. a public position is "± 250 m" in the usual sense.
	CellMeters = 400.0
	// Offset ring around the cell center. Every listing in a cell gets its own deterministic point on this
	// ring, so pins of neighboring listings do not stack, and the public point is never the cell center.
	minOffsetMeters = 40.0
	maxOffsetMeters = 100.0
	// MaxErrorMeters bounds the distance between the true and the public point: 283 + 100 < 400.
	MaxErrorMeters = 400.0
)

// FuzzPoint returns the public position of a listing. The true point is snapped to a 400 m grid cell and
// moved by an offset derived from the listing ID and the cell, so:
//   - it is stable: the same listing at the same place always yields the same point (no jitter between
//     requests, so averaging many responses learns nothing);
//   - it depends on the true point only through its cell, so it never reveals more than the cell;
//   - it stays within MaxErrorMeters of the true point and never equals it.
func FuzzPoint(listingID string, lat, lng float64) (float64, float64) {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return 0, 0
	}
	lat = math.Max(-85, math.Min(85, lat))
	lng = math.Mod(lng+540, 360) - 180

	latStep := CellMeters / metersPerDeg
	row := math.Floor(lat / latStep)
	cLat := (row + 0.5) * latStep
	// Columns use the cosine at the row center, so every point of a row shares one column width.
	lngStep := CellMeters / (metersPerDeg * math.Cos(cLat*math.Pi/180))
	col := math.Floor(lng / lngStep)
	cLng := (col + 0.5) * lngStep

	h := sha256.Sum256([]byte("sewain-geo-v1|" + listingID + "|" +
		strconv.FormatFloat(row, 'f', 0, 64) + "|" + strconv.FormatFloat(col, 'f', 0, 64)))
	u1 := float64(binary.BigEndian.Uint64(h[0:8])) / float64(math.MaxUint64)
	u2 := float64(binary.BigEndian.Uint64(h[8:16])) / float64(math.MaxUint64)
	angle := 2 * math.Pi * u1
	dist := minOffsetMeters + (maxOffsetMeters-minOffsetMeters)*u2

	outLat := cLat + dist*math.Sin(angle)/metersPerDeg
	outLng := cLng + dist*math.Cos(angle)/(metersPerDeg*math.Cos(cLat*math.Pi/180))
	if DistanceKm(lat, lng, outLat, outLng) < 0.001 { // astronomically unlikely; never hand back the truth
		outLat += 20 / metersPerDeg
	}
	return round6(outLat), round6(outLng)
}

// round6 keeps 6 decimals (~11 cm); more digits would only suggest false precision.
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// DistanceKm is the great-circle (haversine) distance.
func DistanceKm(lat1, lng1, lat2, lng2 float64) float64 {
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(a))) / 1000
}

// RoundDistanceKm rounds a distance for display. Below 0.5 km it reports 0.5 (closer than that is within
// the fuzz and must not look precise); up to 10 km one decimal; beyond that whole kilometers.
func RoundDistanceKm(km float64) float64 {
	switch {
	case math.IsNaN(km) || km < 0:
		return 0
	case km < 0.5:
		return 0.5
	case km < 10:
		return math.Round(km*10) / 10
	default:
		return math.Round(km)
	}
}

// FormatDistance renders the display string, e.g. "± 1,2 km" (Indonesian decimal comma).
func FormatDistance(km float64) string {
	r := RoundDistanceKm(km)
	s := strconv.FormatFloat(r, 'f', -1, 64)
	if r < 10 && !strings.Contains(s, ".") {
		s += ".0"
	}
	return "± " + strings.Replace(s, ".", ",", 1) + " km"
}

package domain

import "math"

// RatingSummary is a combined rating: seed base rating plus visible live reviews (web: userRating/listingRating).
type RatingSummary struct {
	Rating float64 // 0 when there are no ratings; rounded to one decimal
	Count  int
}

// CombineRating merges a base rating (seed data) with live review ratings, like the web's combine().
func CombineRating(baseRating float64, baseCount int, live []float64) RatingSummary {
	count := baseCount + len(live)
	if count == 0 {
		return RatingSummary{}
	}
	sum := baseRating * float64(baseCount)
	for _, r := range live {
		sum += r
	}
	return RatingSummary{Rating: jsRound(sum/float64(count)*10) / 10, Count: count}
}

// TrustScore is the web's trustScore() (sewain-web src/lib/selectors.ts), moved to the server because a scoped
// snapshot no longer carries the bookings, disputes and strikes of other users. completed already includes the
// seed baseRatingCount.
func TrustScore(verified bool, rating RatingSummary, completed, disputes, strikes int) int {
	score := 40.0
	if verified {
		score += 20
	}
	if rating.Count > 0 {
		score += (rating.Rating - 3) * 12
	}
	score += math.Min(15, float64(completed))
	score -= float64(disputes*4 + strikes*10)
	return int(math.Max(0, math.Min(100, jsRound(score))))
}

// jsRound is JavaScript's Math.round: halves round towards +Infinity (Go's math.Round rounds them away from zero).
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

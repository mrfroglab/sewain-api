package domain_test

import (
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Expected values are what sewain-web's trustScore()/userRating() return for the same inputs (formula:
// 40 + 20·verified + (rating−3)·12 when rated + min(15, completed) − 4·disputes − 10·strikes, Math.round, clamp 0–100).
func TestTrustScoreMatchesWeb(t *testing.T) {
	cases := []struct {
		name                         string
		verified                     bool
		baseRating                   float64
		baseCount                    int
		live                         []float64
		completed, disputes, strikes int
		wantRating                   float64
		want                         int
	}{
		{name: "new account", want: 40},
		{name: "verified, unrated", verified: true, want: 60},
		{name: "seed provider", verified: true, baseRating: 4.8, baseCount: 23, completed: 23, wantRating: 4.8, want: 97},
		{name: "live reviews only", verified: true, live: []float64{5, 4}, completed: 2, wantRating: 4.5, want: 80},
		{name: "rating rounds to one decimal", baseRating: 4.66, baseCount: 3, live: []float64{4}, completed: 3, wantRating: 4.5, want: 61},
		{name: "half rounds up like Math.round", verified: true, live: []float64{3.15}, wantRating: 3.2, want: 62},
		{name: "disputes and strikes", verified: true, baseRating: 4, baseCount: 2, completed: 4, disputes: 2, strikes: 1, wantRating: 4, want: 58},
		{name: "clamped at 0", live: []float64{1}, disputes: 5, strikes: 3, wantRating: 1, want: 0},
		{name: "best possible score", verified: true, baseRating: 5, baseCount: 400, completed: 400, wantRating: 5, want: 99},
	}
	for _, c := range cases {
		r := domain.CombineRating(c.baseRating, c.baseCount, c.live)
		if r.Rating != c.wantRating {
			t.Errorf("%s: rating %v, want %v", c.name, r.Rating, c.wantRating)
		}
		if got := domain.TrustScore(c.verified, r, c.completed, c.disputes, c.strikes); got != c.want {
			t.Errorf("%s: trust score %d, want %d", c.name, got, c.want)
		}
	}
}

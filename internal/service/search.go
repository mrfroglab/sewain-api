package service

import (
	"context"
	"math"
	"strings"
	"unicode"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

type SearchParams struct {
	Lat, Lng     float64
	RadiusKm     float64
	Category     string
	MinPrice     int64
	MaxPrice     int64
	Handover     string
	Collateral   string // identitas | deposit | barang | custom | cukup-identitas
	ProviderType string // individu | toko
	Q            string
	StartDate    string // availability filter (both dates required), YYYY-MM-DD
	EndDate      string
}

type SearchHit struct {
	ID         string  `json:"id"`
	DistanceKm float64 `json:"distanceKm"`
}

type SearchResult struct {
	Hits                  []SearchHit `json:"hits"`
	HiddenByProviderReach int         `json:"hiddenByProviderReach"`
	Query                 string      `json:"query"`
}

func optStr(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

func optInt(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// TSQuery turns free text into a prefix-matching tsquery for the "simple" configuration ("tenda dom" → "tenda:* &
// dom:*"). Only letters and digits survive, so user input can never form tsquery syntax.
func TSQuery(q string) string {
	var terms []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if w != "" {
			terms = append(terms, w+":*")
		}
	}
	return strings.Join(terms, " & ")
}

// Search implements the two-way radius rule with PostGIS (FR-S3) plus filters (FR-S4). Distances are rounded to
// 100 m so they cannot be used to triangulate a provider's precise location (FR-S6).
func (s *Service) Search(ctx context.Context, p SearchParams) (SearchResult, error) {
	arg := dbq.SearchListingsParams{Lat: p.Lat, Lng: p.Lng, RadiusM: p.RadiusKm * 1000, Category: optStr(p.Category), MinPrice: optInt(p.MinPrice),
		MaxPrice: optInt(p.MaxPrice), Handover: optStr(p.Handover), ProviderType: optStr(p.ProviderType), Tsquery: optStr(TSQuery(p.Q))}
	switch p.Collateral {
	case "":
	case "cukup-identitas":
		arg.Collateral = domain.Ptr("identitas")
	default:
		arg.Collateral = domain.Ptr(p.Collateral)
	}
	if p.StartDate != "" && p.EndDate != "" {
		if _, err := domain.ParseIsoDate(p.StartDate); err == nil {
			if _, err := domain.ParseIsoDate(p.EndDate); err == nil {
				arg.StartDate, arg.EndDate = datePtr(&p.StartDate), datePtr(&p.EndDate)
			}
		}
	}
	rows, err := s.q.SearchListings(ctx, arg)
	if err != nil {
		return SearchResult{}, err
	}
	res := SearchResult{Hits: []SearchHit{}, Query: "ST_DWithin(location, point, LEAST(radius_penyewa, radius_penyedia))"}
	for _, r := range rows {
		if r.Visible {
			res.Hits = append(res.Hits, SearchHit{ID: r.ID, DistanceKm: math.Round(r.DistanceKm*10) / 10})
		} else {
			res.HiddenByProviderReach++
		}
	}
	return res, nil
}

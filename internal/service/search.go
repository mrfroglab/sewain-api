package service

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/geo"
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
	RadiusKm              float64     `json:"radiusKm"` // the radius actually used (SnapRadiusKm)
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

// SearchRadiiKm are the only search radii (review C1): free-form radii would let a caller binary-search a listing's
// distance. Other values round up to the next option (above the largest: the largest).
var SearchRadiiKm = []float64{1, 3, 5, 10, 25}

// SnapRadiusKm rounds a requested radius up to the next allowed option.
func SnapRadiusKm(r float64) float64 {
	for _, o := range SearchRadiiKm {
		if r <= o {
			return o
		}
	}
	return SearchRadiiKm[len(SearchRadiiKm)-1]
}

// fuzzMarginM is larger than the largest distance between a precise and a public point (geo: ≤ 400 m), so the SQL
// prefilter on the precise location never drops a listing whose public point is inside the radius.
const fuzzMarginM = 500

// Search implements the two-way radius rule (FR-S3) plus filters (FR-S4). Everything visible to the caller is computed
// from the public point of each listing (geo.FuzzPoint, the one the snapshot shows): membership in the radius, the
// provider-reach check and distanceKm (rounded with geo.RoundDistanceKm). The radius is snapped to SearchRadiiKm.
// Together this means repeated searches reveal nothing beyond the public point (FR-S6, review C1).
func (s *Service) Search(ctx context.Context, p SearchParams) (SearchResult, error) {
	radiusKm := SnapRadiusKm(p.RadiusKm)
	arg := dbq.SearchListingsParams{Lat: p.Lat, Lng: p.Lng, RadiusM: radiusKm*1000 + fuzzMarginM, Category: optStr(p.Category), MinPrice: optInt(p.MinPrice),
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
	type hit struct {
		id   string
		km   float64
		rank float64
	}
	var hits []hit
	res := SearchResult{Hits: []SearchHit{}, RadiusKm: radiusKm,
		Query: "jarak(titik publik, titik cari) ≤ LEAST(radius_penyewa, radius_penyedia)"}
	for _, r := range rows {
		lat, lng := geo.FuzzPoint(r.ID, r.Lat, r.Lng)
		km := geo.DistanceKm(p.Lat, p.Lng, lat, lng)
		if km > radiusKm {
			continue
		}
		if km > r.RadiusKm {
			res.HiddenByProviderReach++
			continue
		}
		hits = append(hits, hit{r.ID, km, r.Rank})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].km != hits[j].km {
			return hits[i].km < hits[j].km
		}
		if hits[i].rank != hits[j].rank {
			return hits[i].rank > hits[j].rank
		}
		return hits[i].id < hits[j].id
	})
	for _, h := range hits {
		res.Hits = append(res.Hits, SearchHit{ID: h.id, DistanceKm: geo.RoundDistanceKm(h.km)})
	}
	return res, nil
}

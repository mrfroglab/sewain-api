package domain

import (
	"fmt"
	"sort"
	"strings"
)

// FormatCommission is the audit-log text of a commission config.
func FormatCommission(c CommissionConfig) string {
	state := "nonaktif"
	if c.Active {
		state = "aktif"
	}
	rate := Rupiah(c.Flat)
	if c.Mode == "persen" {
		rate = FmtPercent(c.Percent) + "%"
	}
	opt := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return Rupiah(*p)
	}
	s := fmt.Sprintf("%s, %s, min %s, maks %s, %s", state, rate, opt(c.Min), opt(c.Max), c.Bearer)
	if c.Bearer == "split" {
		s += fmt.Sprintf(" %s:%s", FmtPercent(c.SplitRenterPercent), FmtPercent(100-c.SplitRenterPercent))
	}
	return s
}

// ApplyCommissionPatch (FR-K1..K7) returns the new config or a validation error.
func ApplyCommissionPatch(c CommissionConfig, p CommissionPatch) (CommissionConfig, error) {
	if p.Active != nil {
		c.Active = *p.Active
	}
	if p.Mode != nil && (*p.Mode == "persen" || *p.Mode == "flat") {
		c.Mode = *p.Mode
	}
	if p.Percent != nil {
		c.Percent = *p.Percent
	}
	if p.Flat != nil {
		c.Flat = *p.Flat
	}
	if p.HasMin {
		c.Min = p.Min
	}
	if p.HasMax {
		c.Max = p.Max
	}
	if p.Bearer != nil && Contains([]string{"penyewa", "penyedia", "split"}, *p.Bearer) {
		c.Bearer = *p.Bearer
	}
	if p.SplitRenterPercent != nil {
		c.SplitRenterPercent = *p.SplitRenterPercent
	}
	if c.Percent < 0 || c.Percent > 100 || c.Flat < 0 || c.SplitRenterPercent < 0 || c.SplitRenterPercent > 100 {
		return c, Fail("Nilai komisi tidak valid.")
	}
	return c, nil
}

// FormatCollateralRules is the audit-log text of the collateral suggestion rules (FR-J3).
func FormatCollateralRules(x CollateralRules) string {
	return fmt.Sprintf("<%s identitas; ≤%s +%s%%; >%s %s%%; kendaraan %s%%", Rupiah(x.LowMax), Rupiah(x.MidMax), FmtPercent(x.MidDepositPercent), Rupiah(x.MidMax), FmtPercent(x.HighDepositPercent), FmtPercent(x.VehicleDepositPercent))
}

func ApplyCollateralRulesPatch(r CollateralRules, p CollateralRulesPatch) (CollateralRules, error) {
	if p.LowMax != nil {
		r.LowMax = *p.LowMax
	}
	if p.MidMax != nil {
		r.MidMax = *p.MidMax
	}
	if p.MidDepositPercent != nil {
		r.MidDepositPercent = *p.MidDepositPercent
	}
	if p.HighDepositPercent != nil {
		r.HighDepositPercent = *p.HighDepositPercent
	}
	if p.VehicleDepositPercent != nil {
		r.VehicleDepositPercent = *p.VehicleDepositPercent
	}
	if p.HighValueBadge != nil {
		r.HighValueBadge = *p.HighValueBadge
	}
	if r.LowMax <= 0 || r.MidMax <= r.LowMax {
		return r, Fail("Ambang nilai tidak valid: batas sedang harus di atas batas rendah.")
	}
	return r, nil
}

// CollateralSuggestion mirrors suggestCollateral in sewain-web/src/lib/pricing.ts (FR-J3, PRD Lampiran A).
type CollateralSuggestion struct {
	Level       string             `json:"level"` // rendah | sedang | tinggi | kendaraan
	MinCoverage int64              `json:"minCoverage"`
	Preset      []CollateralOption `json:"preset"`
}

// SuggestCollateral: identity only for cheap items, identity + deposit for mid-range, a bigger deposit (or
// something of equal value) above that, and identity + deposit or BPKB for vehicles.
func SuggestCollateral(itemValue int64, category string, r CollateralRules) CollateralSuggestion {
	pct := func(p float64) int64 { return Round1000(float64(itemValue) * p / 100) }
	deposit := func(p float64) CollateralOption {
		return CollateralOption{Kind: "deposit", DepositMode: Ptr("persen"), DepositPercent: Ptr(p), WithIdentity: Ptr(true), Cards: []string{"KTM", "SIM"}}
	}
	switch {
	case IsVehicle(category):
		return CollateralSuggestion{Level: "kendaraan", MinCoverage: pct(r.VehicleDepositPercent), Preset: []CollateralOption{
			deposit(r.VehicleDepositPercent), {Kind: "custom", Description: Ptr("BPKB kendaraan penyewa (asli)"), EstimatedValue: Ptr(itemValue)}}}
	case itemValue < r.LowMax:
		return CollateralSuggestion{Level: "rendah", Preset: []CollateralOption{{Kind: "identitas", Cards: []string{"KTM", "SIM", "Kartu pelajar"}}}}
	case itemValue <= r.MidMax:
		return CollateralSuggestion{Level: "sedang", MinCoverage: pct(r.MidDepositPercent), Preset: []CollateralOption{deposit(r.MidDepositPercent)}}
	}
	return CollateralSuggestion{Level: "tinggi", MinCoverage: pct(r.HighDepositPercent), Preset: []CollateralOption{
		deposit(r.HighDepositPercent), {Kind: "custom", Description: Ptr("Barang/dokumen senilai (mis. laptop, BPKB)"), EstimatedValue: Ptr(pct(r.HighDepositPercent))}}}
}

// CleanKeywords lowercases, trims, de-duplicates and sorts the banned keyword list.
func CleanKeywords(words []string) []string {
	seen := map[string]bool{}
	clean := []string{}
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if w != "" && !seen[w] {
			seen[w] = true
			clean = append(clean, w)
		}
	}
	sort.Strings(clean)
	return clean
}

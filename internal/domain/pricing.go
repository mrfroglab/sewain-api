package domain

import (
	"fmt"
	"math"
	"strings"
)

// QuoteRent: days × daily price, weekly price when ≥7 days, then the duration discount (mirrors pricing.ts).
func QuoteRent(l *Listing, start, end string) RentQuote {
	days := DiffDays(start, end)
	base := int64(days) * l.PricePerDay
	total := base
	breakdown := fmt.Sprintf("%d hari × %s", days, Rupiah(l.PricePerDay))
	if l.PricePerWeek != nil && days >= 7 {
		weeks, rest := days/7, days%7
		total = int64(weeks)**l.PricePerWeek + int64(rest)*l.PricePerDay
		breakdown = fmt.Sprintf("%d minggu × %s", weeks, Rupiah(*l.PricePerWeek))
		if rest > 0 {
			breakdown += fmt.Sprintf(" + %d hari × %s", rest, Rupiah(l.PricePerDay))
		}
	}
	if d := l.DurationDiscount; d != nil && days >= d.MinDays {
		total = int64(math.Round(float64(total) * (1 - d.Percent/100)))
		breakdown += fmt.Sprintf(" − diskon %s%%", FmtPercent(d.Percent))
	}
	return RentQuote{Days: days, Base: base, Discount: base - total, Total: total, Breakdown: breakdown}
}

// ComputeCommission: from the rent value only, never the deposit (FR-K4). "Amount" is always computed
// so the pilot records "komisi seandainya aktif" (FR-K6); "Charged" is 0 while inactive.
func ComputeCommission(rent int64, c CommissionConfig) CommissionSnapshot {
	var amount float64
	rate := float64(c.Flat)
	if c.Mode == "persen" {
		amount = math.Round(float64(rent) * c.Percent / 100)
		rate = c.Percent
	} else {
		amount = float64(c.Flat)
	}
	if c.Min != nil {
		amount = math.Max(amount, float64(*c.Min))
	}
	if c.Max != nil {
		amount = math.Min(amount, float64(*c.Max))
	}
	amt := int64(math.Max(0, math.Round(amount)))
	charged := int64(0)
	if c.Active {
		charged = amt
	}
	share := c.SplitRenterPercent / 100
	switch c.Bearer {
	case "penyewa":
		share = 1
	case "penyedia":
		share = 0
	}
	renter := int64(math.Round(float64(charged) * share))
	return CommissionSnapshot{Active: c.Active, Mode: c.Mode, Rate: rate, Min: c.Min, Max: c.Max, Bearer: c.Bearer,
		SplitRenterPercent: c.SplitRenterPercent, Amount: amt, Charged: charged, RenterPays: renter, ProviderPays: charged - renter}
}

// CollateralValue: monetary coverage of one option. Identity cards count as 0.
func CollateralValue(o CollateralOption, itemValue int64) int64 {
	switch o.Kind {
	case "deposit":
		if Deref(o.DepositMode, "") == "persen" {
			return Round1000(float64(itemValue) * Deref(o.DepositPercent, 0) / 100)
		}
		return Deref(o.DepositAmount, 0)
	case "barang", "custom":
		return Deref(o.EstimatedValue, 0)
	}
	return 0
}

// DepositCash is the cash deposit the renter hands over (payment simulation).
func DepositCash(o CollateralOption, itemValue int64) int64 {
	if o.Kind == "deposit" {
		return CollateralValue(o, itemValue)
	}
	return 0
}

func DescribeCollateral(o CollateralOption) string {
	cards := "kartu identitas"
	if len(o.Cards) > 0 {
		cards = strings.Join(o.Cards, "/")
	}
	switch o.Kind {
	case "identitas":
		return "Titip " + cards
	case "deposit":
		amount := Rupiah(Deref(o.DepositAmount, 0))
		if Deref(o.DepositMode, "") == "persen" {
			amount = FmtPercent(Deref(o.DepositPercent, 0)) + "% nilai barang"
		}
		if Deref(o.WithIdentity, false) {
			return "Titip " + cards + " + deposit " + amount
		}
		return "Deposit " + amount
	case "barang":
		return "Titip barang: " + Deref(o.Description, "barang lain")
	default:
		return "Jaminan khusus: " + Deref(o.Description, "-")
	}
}

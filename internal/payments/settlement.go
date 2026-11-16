package payments

import (
	"errors"
	"fmt"
)

// Settlement cases: PRD §11.3 rows plus a clean completion. Codes follow domain.CaseTypeFor where they
// overlap (lecet-ringan, rusak-ringan, rusak-berat, hilang).
const (
	CaseCompleted       = "selesai"              // COMPLETED without a problem
	CaseWearAndTear     = "lecet-ringan"         // normal wear: no compensation
	CaseRepairable      = "rusak-ringan"         // renter pays a fair repair cost
	CaseHeavyDamage     = "rusak-berat"          // renter pays the used market value
	CaseLost            = "hilang"               // same as heavy damage
	CaseLate            = "telat"                // late fee per day, capped by the agreement
	CaseNotReturned     = "tidak-dikembalikan"   // deposit is the first compensation, rest is claimed
	CaseCollateralKept  = "jaminan-ditahan"      // provider keeps the collateral: a cash deposit is refunded in full
	CaseNotAsDescribed  = "tidak-sesuai"         // reported at check-out: cancel without penalty
	CaseProviderCancel  = "penyedia-batal"       // provider cancelled unilaterally
	CaseRenterNoShow    = "penyewa-tidak-datang" // no-show: booking cancelled, money returned (strike only)
	CaseDepositWithheld = "deposit-ditahan"      // provider cannot show damage: deposit back in full
	CaseVehicleIncident = "kendaraan"            // tickets/damage during the rental: renter pays
	CaseFalseClaim      = "klaim-palsu"          // false claim either way: strikes only, no money moves
)

type caseRule int

const (
	ruleNoClaim    caseRule = iota // deposit back, rent released, Claim must be 0
	ruleClaim                      // Claim from deposit first, rest becomes a new intent
	ruleFullRefund                 // cancel: rent (incl. renter commission) and deposit back, commission reversed
)

var caseRules = map[string]caseRule{
	CaseCompleted: ruleNoClaim, CaseWearAndTear: ruleNoClaim, CaseCollateralKept: ruleNoClaim,
	CaseDepositWithheld: ruleNoClaim, CaseFalseClaim: ruleNoClaim,
	CaseRepairable: ruleClaim, CaseHeavyDamage: ruleClaim, CaseLost: ruleClaim, CaseLate: ruleClaim,
	CaseNotReturned: ruleClaim, CaseVehicleIncident: ruleClaim,
	CaseNotAsDescribed: ruleFullRefund, CaseProviderCancel: ruleFullRefund, CaseRenterNoShow: ruleFullRefund,
}

// KnownCase reports whether c is a settlement case.
func KnownCase(c string) bool { _, ok := caseRules[c]; return ok }

// Funds is what the ledger still holds for a booking.
type Funds struct {
	BookingID  string
	RenterID   string
	ProviderID string
	Escrow     int64      // rent still in escrow (rent − provider's commission share), 0 after handover
	Deposit    int64      // cash deposit held
	RentPaid   int64      // rent value paid (without the renter's commission)
	Commission Commission // commission charged when the rent was paid
}

// Outcome is an executed recommendation (dispute RESOLVED) or a clean completion.
type Outcome struct {
	Case       string
	Claim      int64 // compensation owed to the provider: repair cost, market value, late fee, ticket
	ClaimCap   int64 // optional cap (late fee cap from the agreement), 0 = none
	RentRefund int64 // optional partial rent refund out of escrow for non-cancel cases
}

// Settlement is the plan for one booking. Entries sum to zero; the amounts summarize them.
type Settlement struct {
	Entries            []Entry
	DepositToProvider  int64 // part of the deposit awarded to the provider
	DepositRefund      int64 // part of the deposit going back to the renter
	RentReleased       int64 // escrow released to the provider
	RentRefund         int64 // escrow refunded to the renter
	CommissionReversed int64 // commission handed back to the renter (cancel cases)
	Shortfall          int64 // claim above the deposit: becomes a new payment intent to the renter
}

// ErrBadOutcome is returned for an outcome that cannot be executed.
var ErrBadOutcome = errors.New("payments: invalid settlement outcome")

// PlanSettlement turns an outcome into ledger entries. It is pure: same input, same plan.
//
//   - no-claim cases: deposit back to the renter, escrow (if still held) to the provider;
//   - claim cases: min(claim, deposit) to the provider, the rest of the deposit back, claim − deposit as
//     Shortfall (a new intent); the late fee is capped by ClaimCap;
//   - cancel cases: escrow + the commission charged back to the renter (they paid rent + their share),
//     deposit back. Needs the escrow untouched (cancel happens before handover).
func PlanSettlement(f Funds, o Outcome) (Settlement, error) {
	rule, ok := caseRules[o.Case]
	if !ok {
		return Settlement{}, fmt.Errorf("%w: unknown case %q", ErrBadOutcome, o.Case)
	}
	if f.Escrow < 0 || f.Deposit < 0 || o.Claim < 0 || o.ClaimCap < 0 || o.RentRefund < 0 {
		return Settlement{}, fmt.Errorf("%w: negative amount", ErrBadOutcome)
	}
	var s Settlement
	renter, provider := RenterAccount(f.RenterID), ProviderAccount(f.ProviderID)
	escrow, deposit := EscrowAccount(f.BookingID), DepositAccount(f.BookingID)
	var es []Entry
	switch rule {
	case ruleNoClaim, ruleClaim:
		claim := o.Claim
		if rule == ruleNoClaim && claim != 0 {
			return Settlement{}, fmt.Errorf("%w: case %s takes no claim", ErrBadOutcome, o.Case)
		}
		if o.Case == CaseLate && o.ClaimCap > 0 {
			claim = min(claim, o.ClaimCap)
		}
		s.DepositToProvider = min(claim, f.Deposit)
		s.DepositRefund = f.Deposit - s.DepositToProvider
		s.Shortfall = claim - s.DepositToProvider
		if o.RentRefund > f.Escrow {
			return Settlement{}, fmt.Errorf("%w: rent refund %d exceeds escrow %d", ErrBadOutcome, o.RentRefund, f.Escrow)
		}
		s.RentRefund = o.RentRefund
		s.RentReleased = f.Escrow - o.RentRefund
	case ruleFullRefund:
		if o.Claim != 0 || o.RentRefund != 0 {
			return Settlement{}, fmt.Errorf("%w: case %s refunds everything", ErrBadOutcome, o.Case)
		}
		if f.Escrow != f.RentPaid-f.Commission.ProviderPays {
			return Settlement{}, fmt.Errorf("%w: escrow already released, cannot cancel", ErrBadOutcome)
		}
		s.DepositRefund = f.Deposit
		s.RentRefund = f.Escrow
		s.CommissionReversed = f.Commission.Charged
	}
	es = append(es, move(deposit, provider, s.DepositToProvider)...)
	es = append(es, move(deposit, renter, s.DepositRefund)...)
	es = append(es, move(escrow, provider, s.RentReleased)...)
	es = append(es, move(escrow, renter, s.RentRefund)...)
	es = append(es, move(AccountCommission, renter, s.CommissionReversed)...)
	s.Entries = Compact(es)
	return s, nil
}

// PlanRelease releases the escrow to the provider at handover (booking ACTIVE).
func PlanRelease(f Funds) []Entry {
	return Compact(move(EscrowAccount(f.BookingID), ProviderAccount(f.ProviderID), f.Escrow))
}

// PaidIntent is what PlanPayment needs from a paid intent.
type PaidIntent struct {
	Kind               string
	Amount             int64
	BookingID          string
	ProviderID         string // the booking's provider (Sewain user), receives claim payments
	CommissionRenter   int64
	CommissionProvider int64
	Provider           string // gateway name
	Fee                int64  // gateway fee (MDR) for this payment
}

// PlanPayment books a payment received at the gateway:
//
//	rent:    gateway −amount, escrow +(amount − renter share − provider share), commission +(both shares)
//	deposit: gateway −amount, deposit +amount
//	claim:   gateway −amount, provider +amount
//
// plus the fee: gateway +fee, platform:fees −fee (the platform bears MDR, PRD §14.3).
func PlanPayment(p PaidIntent) ([]Entry, error) {
	if p.Amount <= 0 || p.Fee < 0 || p.CommissionRenter < 0 || p.CommissionProvider < 0 {
		return nil, fmt.Errorf("%w: bad paid intent amounts", ErrBadOutcome)
	}
	gw := GatewayAccount(p.Provider)
	var es []Entry
	switch p.Kind {
	case KindRent:
		commission := p.CommissionRenter + p.CommissionProvider
		net := p.Amount - commission
		if net < 0 {
			return nil, fmt.Errorf("%w: commission above rent", ErrBadOutcome)
		}
		es = append(es, move(gw, EscrowAccount(p.BookingID), net)...)
		es = append(es, move(gw, AccountCommission, commission)...)
	case KindDeposit:
		es = append(es, move(gw, DepositAccount(p.BookingID), p.Amount)...)
	case KindClaim:
		es = append(es, move(gw, ProviderAccount(p.ProviderID), p.Amount)...)
	default:
		return nil, fmt.Errorf("%w: unknown intent kind %q", ErrBadOutcome, p.Kind)
	}
	es = append(es, move(AccountFees, gw, p.Fee)...)
	return Compact(es), nil
}

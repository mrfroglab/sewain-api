package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Result is the outcome of an action: Value on success, or a rule violation in Err (domain.UserError).
type Result struct {
	Value   any
	Version int64
	Change  Change
}

// Dispatch runs a web-store action on the server as one transaction. args are the exact positional arguments the
// web app passed; wantID is the id the client generated for a newly created entity. Returns domain.ErrForbidden
// when the acting user (first argument) is not the viewer, domain.ErrUnknownAction for unknown names, and a
// domain.UserError for rule violations (nothing is written then).
func (s *Service) Dispatch(ctx context.Context, viewer *Viewer, name string, args []json.RawMessage, wantID string) (Result, error) {
	if res, handled, err := s.reviewViaKYC(ctx, viewer, name, args); handled {
		return res, err
	}
	var value any
	ch, err := s.run(ctx, func(t *txn) error {
		v, err := t.dispatch(viewer, name, args, wantID)
		value = v
		return err
	})
	if err != nil {
		return Result{}, err
	}
	version := ch.Version
	if version == 0 {
		if version, err = s.Version(ctx); err != nil {
			return Result{}, err
		}
	}
	return Result{Value: value, Version: version, Change: ch}, nil
}

func (t *txn) dispatch(viewer *Viewer, name string, args []json.RawMessage, wantID string) (any, error) {
	arg := func(i int, v any) error {
		if i >= len(args) || string(args[i]) == "null" {
			return nil
		}
		if err := json.Unmarshal(args[i], v); err != nil {
			return domain.Fail(fmt.Sprintf("Argumen %d tidak valid.", i+1))
		}
		return nil
	}
	str := func(i int) string {
		var s string
		_ = arg(i, &s)
		return s
	}
	var none any

	if name == "registerUser" {
		var in domain.RegisterInput
		if err := arg(0, &in); err != nil {
			return nil, err
		}
		return t.registerUser(in, wantID)
	}
	if _, known := actionNames[name]; !known {
		return nil, domain.ErrUnknownAction
	}
	// The first argument of every other action is the acting user: it must be the signed-in user.
	a := str(0)
	if viewer == nil || a != viewer.UserID {
		return nil, domain.ErrForbidden
	}

	switch name {
	case "updateUser":
		var p domain.UserPatch
		if err := arg(1, &p); err != nil {
			return nil, err
		}
		return none, t.updateUser(a, p)
	case "submitVerification":
		var in domain.VerificationInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return t.submitVerification(a, in, wantID)
	case "reviewVerification":
		var approve bool
		var reason *string
		_ = arg(2, &approve)
		_ = arg(3, &reason)
		return none, t.reviewVerification(a, str(1), approve, reason)
	case "createListing":
		var in domain.ListingInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return t.createListing(a, in, wantID)
	case "updateListing":
		var in domain.ListingInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, t.updateListing(a, str(1), in)
	case "setListingStatus":
		return none, t.setListingStatus(a, str(1), str(2))
	case "toggleFavorite":
		return none, t.toggleFavorite(a, str(1))
	case "createBooking":
		var in domain.BookingInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return t.createBooking(a, in, wantID)
	case "acceptBooking":
		var meeting *string
		_ = arg(2, &meeting)
		return none, t.acceptBooking(a, str(1), meeting)
	case "declineBooking":
		return none, t.declineBooking(a, str(1), str(2))
	case "cancelBooking":
		return none, t.cancelBooking(a, str(1), str(2))
	case "payBooking":
		return none, t.payBooking(a, str(1), str(2))
	case "confirmDirectPayment":
		return none, t.confirmDirectPayment(a, str(1))
	case "submitCheckout":
		var in domain.CheckoutInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, t.submitCheckout(a, str(1), in)
	case "addCheckoutPhotos":
		var ids []domain.ID
		_ = arg(2, &ids)
		return none, t.addCheckoutPhotos(a, str(1), ids)
	case "agreeCheckout":
		return none, t.agreeCheckout(a, str(1))
	case "rejectAtCheckout":
		return none, t.rejectAtCheckout(a, str(1), str(2))
	case "submitCheckin":
		var in domain.CheckinInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, t.submitCheckin(a, str(1), in)
	case "returnCollateral":
		return none, t.returnCollateral(a, str(1))
	case "confirmCollateral":
		return none, t.confirmCollateral(a, str(1))
	case "submitReview":
		var rating float64
		_ = arg(2, &rating)
		return none, t.submitReview(a, str(1), rating, str(3))
	case "sendMessage":
		var in struct {
			Text    *string    `json:"text"`
			PhotoID *domain.ID `json:"photoId"`
		}
		_ = arg(2, &in)
		return none, t.sendMessage(a, str(1), in.Text, in.PhotoID)
	case "markThreadRead":
		return none, t.markThreadRead(a, str(1))
	case "markNotificationsRead":
		var ids []domain.ID
		_ = arg(1, &ids)
		return none, t.markNotificationsRead(a, ids)
	case "openDispute":
		var in domain.DisputeInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return t.openDispute(a, str(1), in, wantID)
	case "proposeSolution":
		var in domain.ProposalInput
		if err := arg(2, &in); err != nil {
			return nil, err
		}
		return none, t.proposeSolution(a, str(1), in)
	case "respondProposal":
		var accept bool
		_ = arg(3, &accept)
		return none, t.respondProposal(a, str(1), str(2), accept)
	case "escalateDispute":
		return none, t.escalateDispute(a, str(1))
	case "adminRecommend":
		var in struct {
			Text   string `json:"text"`
			Amount *int64 `json:"amount"`
		}
		_ = arg(2, &in)
		return none, t.adminRecommend(a, str(1), in.Text, in.Amount)
	case "resolveDispute":
		return none, t.resolveDispute(a, str(1), str(2))
	case "createReport":
		var in domain.ReportInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return t.createReport(a, in, wantID)
	case "resolveReport":
		return none, t.resolveReport(a, str(1), str(2), str(3))
	case "addStrike":
		var in domain.StrikeInput
		_ = arg(2, &in)
		return none, t.addStrike(a, str(1), in)
	case "setUserStatus":
		return none, t.setUserStatus(a, str(1), str(2), str(3))
	case "updateCommission":
		var p domain.CommissionPatch
		if err := arg(1, &p); err != nil {
			return nil, err
		}
		var keys map[string]json.RawMessage
		_ = arg(1, &keys)
		_, p.HasMin = keys["min"]
		_, p.HasMax = keys["max"]
		return none, t.updateCommission(a, p)
	case "updateCollateralRules":
		var p domain.CollateralRulesPatch
		if err := arg(1, &p); err != nil {
			return nil, err
		}
		return none, t.updateCollateralRules(a, p)
	case "setBannedKeywords":
		var words []string
		_ = arg(1, &words)
		return none, t.setBannedKeywords(a, words)
	case "createWanted":
		var in domain.WantedInput
		if err := arg(1, &in); err != nil {
			return nil, err
		}
		return t.createWanted(a, in, wantID)
	case "offerWanted":
		var in struct {
			ListingID *domain.ID `json:"listingId"`
			Message   string     `json:"message"`
		}
		_ = arg(2, &in)
		return none, t.offerWanted(a, str(1), in.ListingID, in.Message)
	case "closeWanted":
		return none, t.closeWanted(a, str(1), str(2))
	}
	return nil, domain.ErrUnknownAction
}

// actionNames lists the server actions taking the acting user as first argument (registerUser is separate,
// the demo controls advanceClock/resetClock/resetDemo are handled by the HTTP layer in DEMO_MODE).
var actionNames = map[string]struct{}{
	"updateUser": {}, "submitVerification": {}, "reviewVerification": {}, "createListing": {}, "updateListing": {},
	"setListingStatus": {}, "toggleFavorite": {}, "createBooking": {}, "acceptBooking": {}, "declineBooking": {},
	"cancelBooking": {}, "payBooking": {}, "confirmDirectPayment": {}, "submitCheckout": {}, "addCheckoutPhotos": {}, "agreeCheckout": {},
	"rejectAtCheckout": {}, "submitCheckin": {}, "returnCollateral": {}, "confirmCollateral": {}, "submitReview": {},
	"sendMessage": {}, "markThreadRead": {}, "markNotificationsRead": {}, "openDispute": {}, "proposeSolution": {},
	"respondProposal": {}, "escalateDispute": {}, "adminRecommend": {}, "resolveDispute": {}, "createReport": {},
	"resolveReport": {}, "addStrike": {}, "setUserStatus": {}, "updateCommission": {}, "updateCollateralRules": {},
	"setBannedKeywords": {}, "createWanted": {}, "offerWanted": {}, "closeWanted": {},
}

// ActionNames returns every server action name (for docs and tests).
func ActionNames() []string {
	out := []string{"registerUser"}
	for n := range actionNames {
		out = append(out, n)
	}
	return out
}
